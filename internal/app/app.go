package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/catalog"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/transport/httpapi"
	"github.com/HasonoCell/Orbit-DevOps/internal/webhook"
	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	DatabaseURL          string
	LocalActorID         string
	LocalClusterRef      string
	LocalNamespace       string
	BuildAllowedGitHosts []string
	BuildPlatform        string
	BuildRegistryHost    string
	BuildRegistryPrefix  string
	MigrateOnBoot        bool
	WebhookConfig        webhook.Config
}

type Runtime struct {
	handler http.Handler
	db      *sqlx.DB
}

type Dependencies struct {
	RuntimeSource      diagnostics.RuntimeSource
	GitSourceInspector pipeline.GitSourceInspector
	RecoveryPublisher  releaseworker.RecoveryPublisher
	Logger             *slog.Logger
	Metrics            *observability.Metrics
	Tracer             trace.Tracer
	Propagator         propagation.TextMapPropagator
}

func New(ctx context.Context, config Config) (*Runtime, error) {
	return NewWithDependencies(ctx, config, Dependencies{})
}

func NewWithDependencies(
	ctx context.Context,
	config Config,
	dependencies Dependencies,
) (*Runtime, error) {
	if config.DatabaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	if err := projectauth.ValidateActorID(config.LocalActorID); err != nil {
		return nil, fmt.Errorf("local actor ID: %w", err)
	}
	if config.LocalClusterRef == "" {
		return nil, errors.New("local cluster reference is required")
	}
	if config.LocalNamespace == "" {
		return nil, errors.New("local namespace is required")
	}

	if config.MigrateOnBoot {
		if err := database.Migrate(config.DatabaseURL); err != nil {
			return nil, fmt.Errorf("migrate database: %w", err)
		}
	}

	db, err := sqlx.Open("pgx", config.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	openAPISpec, err := api.GetSpec()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load OpenAPI specification: %w", err)
	}

	authorizer := projectauth.New(db)
	projectModule := project.New(db, authorizer)
	catalogModule := catalog.New(db, catalog.Config{
		ClusterRef: config.LocalClusterRef,
		Namespace:  config.LocalNamespace,
	}, authorizer)
	releaseOperationModule := releaseoperation.New(db, releaseoperation.WithAuthorizer(authorizer))
	buildOperationModule := buildoperation.New(db, buildoperation.WithAuthorizer(authorizer))
	buildModule := build.New(db, build.Config{
		AllowedGitHosts: config.BuildAllowedGitHosts,
		Platform:        config.BuildPlatform,
		RegistryHost:    config.BuildRegistryHost,
		RegistryPrefix:  config.BuildRegistryPrefix,
	}, buildOperationModule, authorizer)
	deliveryModule := delivery.New(db, releaseOperationModule, authorizer)
	pipelineModule := pipeline.New(db, pipeline.Config{Platform: config.BuildPlatform}, buildModule, deliveryModule, authorizer, dependencies.GitSourceInspector)
	diagnosticModule := diagnostics.New(db, authorizer, dependencies.RuntimeSource)
	logger := dependencies.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	metrics := dependencies.Metrics
	if metrics == nil {
		metrics = observability.NewMetrics(releaseOperationModule.CountPending)
	} else {
		metrics.RegisterReleaseOperationPending(releaseOperationModule.CountPending)
	}
	metrics.RegisterReleaseOperations(releaseOperationModule.ReadMetricsSnapshot)
	tracer := dependencies.Tracer
	if tracer == nil {
		tracer = otel.Tracer("github.com/HasonoCell/Orbit-DevOps/internal/app")
	}
	propagator := dependencies.Propagator
	if propagator == nil {
		propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)
	}
	webhookModule := webhook.New(db, config.WebhookConfig, tracer, propagator)
	server := httpapi.NewServer(
		projectModule,
		catalogModule,
		buildModule,
		buildOperationModule,
		deliveryModule,
		diagnosticModule,
		pipelineModule,
		webhookModule,
		releaseOperationModule,
		authorizer,
		dependencies.RecoveryPublisher,
		config.LocalActorID,
		propagator,
	)
	strictHandler := api.NewStrictHandler(server, nil)

	router := gin.New()
	router.GET("/healthz", func(ginContext *gin.Context) {
		checkContext, cancel := context.WithTimeout(ginContext.Request.Context(), time.Second)
		defer cancel()
		if err := db.PingContext(checkContext); err != nil {
			ginContext.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		ginContext.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/metrics", gin.WrapH(metrics.Handler()))
	router.Use(gin.Recovery())
	router.Use(observability.TraceMiddleware(tracer, propagator))
	router.Use(observability.RequestMiddleware(metrics, logger, config.LocalActorID))
	router.Use(ginmiddleware.OapiRequestValidator(openAPISpec))
	api.RegisterHandlers(router, strictHandler)

	return &Runtime{
		handler: router,
		db:      db,
	}, nil
}

func (r *Runtime) Handler() http.Handler {
	return r.handler
}

func (r *Runtime) Close() error {
	return r.db.Close()
}
