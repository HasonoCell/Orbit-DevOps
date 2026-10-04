// orbit-devops-pipeline-worker 消费内部状态事件并编排 Build 与 Release，不持有 Kubernetes 权限。
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

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/githubsource"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	platformdb "github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/Orbit-DevOps/internal/platform/process"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("Orbit-DevOps Pipeline Worker 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := envconfig.LoadPipelineWorker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := platformdb.Open(ctx, config.DatabaseURL, config.DatabasePool)
	if err != nil {
		return err
	}
	defer database.Close()
	authorizer := projectauth.New(database, nil)
	buildOperations := buildoperation.New(database)
	releaseOperations := releaseoperation.New(database)
	builds := build.New(database, build.Config{AllowedGitHosts: config.SourceBuild.AllowedGitHosts,
		Platform: config.SourceBuild.Platform, RegistryHost: config.SourceBuild.RegistryHost,
		RegistryPrefix: config.SourceBuild.RegistryPrefix}, buildOperations, authorizer)
	releases := delivery.New(database, releaseOperations, authorizer)
	github, err := githubsource.New(githubsource.Config{APIBaseURL: config.GitHubSource.APIBaseURL,
		Token: config.GitHubSource.Token, EndpointKeys: map[string]struct{}{}, Timeout: config.GitHubSource.Timeout})
	if err != nil {
		return err
	}
	metrics := pipeline.NewMetrics(database)
	pipelines := pipeline.New(database, pipeline.Config{Platform: config.SourceBuild.Platform,
		SourceRecoveryWindow: config.SourceRecoveryWindow, SourceRetryBaseDelay: config.SourceRetryBaseDelay, Recorder: metrics},
		builds, releases, authorizer, github)
	events := internalevent.New(database)
	service, err := internalevent.NewService(internalevent.Config{RedisAddress: config.Queue.RedisAddress,
		RedisUsername: config.Queue.RedisUsername, RedisPassword: config.Queue.RedisPassword,
		RedisDB: config.Queue.RedisDB, Queue: config.Queue.Name, Concurrency: config.Queue.Concurrency,
		Topics:       []string{"webhook_delivery.received.v1", "build_operation.changed.v1", "release_operation.changed.v1", "delivery_run.reconcile.v1"},
		PollInterval: config.PollInterval, ConsumptionGrace: config.Queue.ConsumptionGrace,
		TaskTimeout: config.Queue.TaskTimeout, ShutdownTimeout: config.Queue.ShutdownTimeout, Logger: logger}, events, pipelines)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewDBStatsCollector(database.DB, "pipeline-worker"))
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), service, metrics)
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		check, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if database.PingContext(check) != nil {
			http.Error(response, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/readyz", func(response http.ResponseWriter, request *http.Request) {
		check, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if database.PingContext(check) != nil || service.Ready(check) != nil {
			http.Error(response, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ready"}`))
	})
	server := &http.Server{Addr: config.Address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 3)
	go func() { results <- processruntime.ServeHTTP(runContext, server, logger) }()
	go func() { results <- service.Run(runContext) }()
	go func() { results <- maintain(runContext, pipelines, config.MaintenanceInterval, logger) }()
	first := <-results
	cancel()
	second, third := <-results, <-results
	return errors.Join(first, second, third)
}

func maintain(ctx context.Context, pipelines *pipeline.Module, interval time.Duration, logger *slog.Logger) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := pipelines.Maintain(cycle)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "Pipeline 维护暂时不可用")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
