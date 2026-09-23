package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/githubsource"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/Orbit-DevOps/internal/platform/process"
	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/HasonoCell/Orbit-DevOps/internal/webhook"
	"github.com/gin-gonic/gin"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("Orbit-DevOps API 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	gin.SetMode(gin.ReleaseMode)
	config, err := envconfig.LoadAPI()
	if err != nil {
		return err
	}
	if err := envconfig.ValidateKubernetes(config.Kubernetes); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracing := observability.NewTracing(logger)
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracing.Shutdown(shutdownContext); err != nil {
			logger.Error("关闭 Trace Provider 失败", "error", err)
		}
	}()
	metrics := observability.NewMetrics(nil)
	adapter, err := kube.NewVerifiedLocalAdapter(
		ctx,
		config.Kubernetes.KubeconfigPath,
		config.Kubernetes.Context,
		kube.Config{
			ClusterRef:          config.Kubernetes.ClusterRef,
			Namespace:           config.Kubernetes.Namespace,
			FieldManager:        config.Kubernetes.FieldManager,
			PollInterval:        config.Kubernetes.PollInterval,
			ReadFailureRecorder: metrics,
		},
	)
	if err != nil {
		return err
	}
	var accessObserver access.ControllerObserver
	if config.Access.GatewayClassName != "" {
		accessObserver, err = kube.NewGatewayAdapter(adapter, config.Access.GatewayClassName)
		if err != nil {
			return err
		}
	}
	githubAdapter, err := githubsource.New(githubsource.Config{APIBaseURL: config.GitHubSource.APIBaseURL, Token: config.GitHubSource.Token, EndpointKeys: webhookEndpointKeys(config.GitHubWebhook.Endpoints), Timeout: config.GitHubSource.Timeout})
	if err != nil {
		return err
	}
	var oidcConfig *identity.OIDCConfig
	if len(config.OIDC.EncryptionKey) > 0 {
		oidcConfig = &identity.OIDCConfig{Adapter: identity.NewCoreOSOIDCAdapter(config.OIDC.HTTPTimeout),
			ClientSecrets: config.OIDC.ClientSecrets, RedirectURL: config.OIDC.RedirectURL,
			EncryptionKey: config.OIDC.EncryptionKey}
	}
	issuerPolicies := make(map[string]access.IssuerPolicy, len(config.Access.IssuerPolicies))
	for key, policy := range config.Access.IssuerPolicies {
		issuerPolicies[key] = access.IssuerPolicy{Kind: policy.Kind, Name: policy.Name}
	}
	runtime, err := app.NewWithDependencies(ctx, app.Config{
		DatabaseURL: config.DatabaseURL,
		BrowserSecurity: httpapi.BrowserSecurityConfig{
			ExternalURL:         config.Browser.ExternalURL,
			TrustedOrigins:      config.Browser.TrustedOrigins,
			AllowLoopbackHTTP:   config.Browser.AllowLoopbackHTTP,
			WebhookMaxBodyBytes: config.GitHubWebhook.MaxBodyBytes,
		},
		OIDC:                 oidcConfig,
		LocalClusterRef:      config.Kubernetes.ClusterRef,
		LocalNamespace:       config.Kubernetes.Namespace,
		GatewayClassName:     config.Access.GatewayClassName,
		AccessIssuerPolicies: issuerPolicies,
		BuildAllowedGitHosts: config.SourceBuild.AllowedGitHosts,
		BuildPlatform:        config.SourceBuild.Platform,
		BuildRegistryHost:    config.SourceBuild.RegistryHost,
		BuildRegistryPrefix:  config.SourceBuild.RegistryPrefix,
		MigrateOnBoot:        config.MigrateOnBoot,
		WebhookConfig: webhook.Config{
			Endpoints:    webhookEndpoints(config.GitHubWebhook.Endpoints),
			MaxBodyBytes: config.GitHubWebhook.MaxBodyBytes,
		},
	}, app.Dependencies{
		RuntimeSource:      adapter,
		SecretVerifier:     adapter,
		AccessObserver:     accessObserver,
		GitSourceInspector: githubAdapter,
		RecoveryPublisher:  adapter,
		Logger:             logger,
		Metrics:            metrics,
		Tracer:             tracing.Provider.Tracer("orbit-devops-api"),
		Propagator:         tracing.Propagator,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			logger.Error("关闭 API 数据库连接失败", "error", err)
		}
	}()

	server := &http.Server{
		Addr:              config.Address,
		Handler:           runtime.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := processruntime.ServeHTTP(ctx, server, logger); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// webhookEndpoints 在进程启动边界复制 Secret，避免配置层结构渗入领域模块。
func webhookEndpoints(configured map[string]envconfig.GitHubWebhookSecrets) map[string]webhook.EndpointSecrets {
	result := make(map[string]webhook.EndpointSecrets, len(configured))
	for key, secrets := range configured {
		result[key] = webhook.EndpointSecrets{Current: secrets.CurrentSecret, Previous: secrets.PreviousSecret}
	}
	return result
}

func webhookEndpointKeys(configured map[string]envconfig.GitHubWebhookSecrets) map[string]struct{} {
	result := make(map[string]struct{}, len(configured))
	for key := range configured {
		result[key] = struct{}{}
	}
	return result
}
