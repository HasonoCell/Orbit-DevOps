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

	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/Orbit-DevOps/internal/platform/process"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releasedispatch"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("Orbit-DevOps Release Worker 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := envconfig.LoadReleaseWorker()
	if err != nil {
		return err
	}
	if err := envconfig.ValidateKubernetes(config.Kubernetes); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlx.Open("pgx", config.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("关闭 Release Worker 数据库连接失败", "error", err)
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	releaseOperations := releaseoperation.New(
		db,
		releaseoperation.WithAutomaticRetryPolicy(
			config.MaximumAutomaticRetries,
			func(retryNumber int) time.Duration {
				// 每次重试成倍退避，避免 Kubernetes 短暂不可用时形成请求风暴。
				delay := config.RetryBaseDelay
				for current := 1; current < retryNumber && delay < 30*time.Second; current++ {
					delay *= 2
					if delay > 30*time.Second {
						return 30 * time.Second
					}
				}
				return delay
			},
		),
	)
	releases := delivery.New(db, releaseOperations, projectauth.New(db))
	metrics := observability.NewMetrics(releaseOperations.CountPending)
	metrics.RegisterReleaseOperations(releaseOperations.ReadMetricsSnapshot)
	tracing := observability.NewTracing(logger)
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracing.Shutdown(shutdownContext); err != nil {
			logger.Error("关闭 Trace Provider 失败", "error", err)
		}
	}()
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
	runner, err := releaseworker.New(releaseworker.Config{
		WorkerID:                config.WorkerID,
		LeaseDuration:           config.LeaseDuration,
		ReleaseOperationTimeout: config.ReleaseOperationTimeout,
		Logger:                  logger,
		Recorder:                metrics,
		Tracer:                  tracing.Provider.Tracer("orbit-devops-release-worker"),
		Propagator:              tracing.Propagator,
	}, releaseOperations, releases, adapter)
	if err != nil {
		return err
	}

	// 正常入口只消费指定投递意图；Redis 故障不回退到全局数据库领取。
	queue, err := releasedispatch.New(releasedispatch.Config{
		RedisAddress: config.ReleaseQueue.RedisAddress, RedisUsername: config.ReleaseQueue.RedisUsername,
		RedisPassword: config.ReleaseQueue.RedisPassword, RedisDB: config.ReleaseQueue.RedisDB,
		Queue: config.ReleaseQueue.Name, Concurrency: config.ReleaseQueue.Concurrency, PollInterval: config.PollInterval,
		RepairInterval: config.ReleaseQueue.RepairInterval, ConsumptionGrace: config.ReleaseQueue.ConsumptionGrace,
		TaskTimeout: config.ReleaseQueue.TaskTimeout, ShutdownTimeout: config.ReleaseQueue.ShutdownTimeout, Logger: logger,
	}, releaseOperations, runner)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	metrics.RegisterCollector(queue)
	mux.Handle("/readyz", queue.ReadinessHandler())
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, request *http.Request) {
		checkContext, cancel := context.WithTimeout(request.Context(), time.Second)
		defer cancel()
		if err := db.PingContext(checkContext); err != nil {
			http.Error(response, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{
		Addr:              config.Address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() {
		results <- processruntime.ServeHTTP(runContext, server, logger)
	}()
	go func() {
		results <- queue.Run(runContext)
	}()
	firstErr := <-results
	cancel()
	secondErr := <-results
	return errors.Join(firstErr, secondErr)
}
