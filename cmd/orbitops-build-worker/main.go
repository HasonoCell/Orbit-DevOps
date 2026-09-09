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

	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/builddispatch"
	"github.com/HasonoCell/OrbitOps/internal/buildkube"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/OrbitOps/internal/platform/process"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("OrbitOps Build Worker 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := envconfig.LoadBuildWorker()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := sqlx.Open("pgx", config.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return err
	}
	operations := buildoperation.New(database, buildoperation.WithAutomaticRetryPolicy(
		config.MaximumAutomaticRetries, func(retryNumber int) time.Duration {
			delay := config.RetryBaseDelay
			for current := 1; current < retryNumber && delay < time.Minute; current++ {
				delay *= 2
			}
			return min(delay, time.Minute)
		}))
	builds := build.New(database, build.Config{}, operations, projectauth.New(database))
	adapter, err := buildkube.NewVerifiedLocalAdapter(ctx, config.KubeconfigPath, config.KubernetesContext, buildkube.Config{
		Namespace: config.Namespace, FieldManager: config.FieldManager, GitImage: config.GitImage,
		BuildkitImage: config.BuildkitImage, RegistrySecretName: config.RegistrySecretName,
		RegistryInsecure: config.RegistryInsecure, DockerHubMirror: config.DockerHubMirror,
		DockerHubMirrorInsecure: config.DockerHubMirrorInsecure,
		ActiveDeadline:          config.BuildOperationTimeout, TTL: config.JobTTL, CPU: config.CPU,
		Memory: config.Memory, PollInterval: config.PollInterval,
	})
	if err != nil {
		return err
	}
	runner, err := buildworker.New(buildworker.Config{WorkerID: config.WorkerID,
		LeaseDuration: config.LeaseDuration,
		// Job 自己持有业务截止时间；Runner 多留一个观察窗口，用 Kubernetes 终态而非本地时钟判定超时。
		BuildTimeout: config.BuildOperationTimeout + 30*time.Second,
		PollInterval: config.PollInterval, Logger: logger}, operations, builds, adapter)
	if err != nil {
		return err
	}
	queue, err := builddispatch.New(builddispatch.Config{
		RedisAddress: config.BuildQueue.RedisAddress, RedisUsername: config.BuildQueue.RedisUsername,
		RedisPassword: config.BuildQueue.RedisPassword, RedisDB: config.BuildQueue.RedisDB,
		Queue: config.BuildQueue.Name, Concurrency: config.BuildQueue.Concurrency,
		PollInterval: config.PollInterval, RepairInterval: config.BuildQueue.RepairInterval,
		ConsumptionGrace: config.BuildQueue.ConsumptionGrace, TaskTimeout: config.BuildQueue.TaskTimeout,
		ShutdownTimeout: config.BuildQueue.ShutdownTimeout, Logger: logger,
	}, operations, runner)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), queue)
	mux.Handle("/readyz", queue.ReadinessHandler())
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		checkContext, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if err := database.PingContext(checkContext); err != nil {
			http.Error(response, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Addr: config.Address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- processruntime.ServeHTTP(runContext, server, logger) }()
	go func() { results <- queue.Run(runContext) }()
	first := <-results
	cancel()
	second := <-results
	return errors.Join(first, second)
}
