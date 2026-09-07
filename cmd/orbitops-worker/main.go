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

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/OrbitOps/internal/platform/process"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("OrbitOps Worker 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := envconfig.LoadWorker()
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
			logger.Error("关闭 Worker 数据库连接失败", "error", err)
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	operations := operation.New(
		db,
		operation.WithAutomaticRetryPolicy(
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
	releases := delivery.New(db, operations, projectauth.New(db))
	metrics := observability.NewMetrics(operations.CountPending)
	metrics.RegisterOperations(operations.ReadMetricsSnapshot)
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
	runner, err := worker.New(worker.Config{
		WorkerID:         config.WorkerID,
		LeaseDuration:    config.LeaseDuration,
		OperationTimeout: config.OperationTimeout,
		Logger:           logger,
		Recorder:         metrics,
		Tracer:           tracing.Provider.Tracer("orbitops-worker"),
		Propagator:       tracing.Propagator,
	}, operations, releases, adapter)
	if err != nil {
		return err
	}

	// 正常入口只消费指定投递意图；Redis 故障不回退到全局数据库领取。
	queue, err := dispatch.New(dispatch.Config{
		RedisAddress: config.Queue.RedisAddress, RedisUsername: config.Queue.RedisUsername,
		RedisPassword: config.Queue.RedisPassword, RedisDB: config.Queue.RedisDB,
		Queue: config.Queue.Name, Concurrency: config.Queue.Concurrency, PollInterval: config.PollInterval,
		RepairInterval: config.Queue.RepairInterval, ConsumptionGrace: config.Queue.ConsumptionGrace,
		TaskTimeout: config.Queue.TaskTimeout, ShutdownTimeout: config.Queue.ShutdownTimeout, Logger: logger,
	}, operations, runner)
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
