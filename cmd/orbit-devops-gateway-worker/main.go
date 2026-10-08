// orbit-devops-gateway-worker 独立持有受限集群写权限，Pipeline Worker 无需 Kubernetes 凭据。
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
	"github.com/HasonoCell/Orbit-DevOps/internal/accessworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	platformdb "github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
	processruntime "github.com/HasonoCell/Orbit-DevOps/internal/platform/process"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("Orbit-DevOps Gateway Worker 退出", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	config, err := envconfig.LoadGatewayWorker()
	if err != nil {
		return err
	}
	if err := envconfig.ValidateKubernetes(config.Kubernetes); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := platformdb.Open(ctx, config.DatabaseURL, config.DatabasePool)
	if err != nil {
		return err
	}
	defer database.Close()
	tracing, err := observability.NewTracing(ctx, config.Tracing, logger, "gateway-worker")
	if err != nil {
		return err
	}
	defer func() {
		if err := tracing.Shutdown(context.Background()); err != nil {
			logger.Warn("Trace 退出刷新未完成", "error", err)
		}
	}()
	adapter, err := kube.NewVerifiedAdapter(ctx, config.Kubernetes.Connection,
		kube.Config{ClusterRef: config.Kubernetes.ClusterRef, Namespace: config.Kubernetes.Namespace,
			FieldManager: config.Kubernetes.FieldManager, PollInterval: config.Kubernetes.PollInterval})
	if err != nil {
		return err
	}
	gateway, err := kube.NewGatewayAdapter(adapter, config.Access.GatewayClassName)
	if err != nil {
		return err
	}
	policies := make(map[string]access.IssuerPolicy, len(config.Access.IssuerPolicies))
	for key, policy := range config.Access.IssuerPolicies {
		policies[key] = access.IssuerPolicy{Kind: policy.Kind, Name: policy.Name}
	}
	module, err := access.New(database, projectauth.New(database, nil), access.Config{
		ClusterRef: config.Kubernetes.ClusterRef, Namespace: config.Kubernetes.Namespace,
		GatewayClassName: config.Access.GatewayClassName, IssuerPolicies: policies}, nil, nil)
	if err != nil {
		return err
	}
	worker, err := accessworker.New(module, gateway, config.LeaseDuration, logger, accessworker.WithTracer(tracing.Provider.Tracer("orbit-devops-gateway-worker")))
	if err != nil {
		return err
	}
	events := internalevent.New(database)
	service, err := internalevent.NewService(internalevent.Config{
		RedisAddress: config.Queue.RedisAddress, RedisUsername: config.Queue.RedisUsername,
		RedisPassword: config.Queue.RedisPassword, RedisDB: config.Queue.RedisDB,
		Queue: config.Queue.Name, Topics: []string{accessworker.Topic},
		Concurrency: config.Queue.Concurrency, PollInterval: config.PollInterval,
		ConsumptionGrace: config.Queue.ConsumptionGrace, TaskTimeout: config.Queue.TaskTimeout,
		ShutdownTimeout: config.Queue.ShutdownTimeout, Logger: logger, Tracer: tracing.Provider.Tracer("orbit-devops-gateway-worker"), Propagator: tracing.Propagator}, events, worker)
	if err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(tracing.ExportFailures)
	registry.MustRegister(collectors.NewDBStatsCollector(database.DB, "gateway-worker"))
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), service)
	mux := http.NewServeMux()
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
	go func() { results <- worker.Maintain(runContext, config.MaintenanceInterval) }()
	first := <-results
	cancel()
	return errors.Join(first, <-results, <-results)
}
