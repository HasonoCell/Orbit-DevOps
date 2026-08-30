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
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/OrbitOps/internal/platform/process"
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
	operations := operation.New(db)
	releases := delivery.New(db, operations)
	metrics := observability.NewMetrics(operations.CountPending)
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

	mux := http.NewServeMux()
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
		results <- runWorker(runContext, runner, config.PollInterval, logger)
	}()
	firstErr := <-results
	cancel()
	secondErr := <-results
	return errors.Join(firstErr, secondErr)
}

func runWorker(
	ctx context.Context,
	runner *worker.Runner,
	pollInterval time.Duration,
	logger *slog.Logger,
) error {
	for {
		processed, err := runner.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.ErrorContext(ctx, "Worker 执行失败", "error", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && processed {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}
