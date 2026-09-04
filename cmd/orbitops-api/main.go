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

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/OrbitOps/internal/platform/process"
	"github.com/gin-gonic/gin"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("OrbitOps API 退出", "error", err)
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
	runtime, err := app.NewWithDependencies(ctx, app.Config{
		DatabaseURL:     config.DatabaseURL,
		LocalActorID:    config.ActorID,
		LocalClusterRef: config.Kubernetes.ClusterRef,
		LocalNamespace:  config.Kubernetes.Namespace,
		MigrateOnBoot:   config.MigrateOnBoot,
	}, app.Dependencies{
		RuntimeObserver:   adapter,
		RecoveryPublisher: adapter,
		Logger:            logger,
		Metrics:           metrics,
		Tracer:            tracing.Provider.Tracer("orbitops-api"),
		Propagator:        tracing.Propagator,
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
