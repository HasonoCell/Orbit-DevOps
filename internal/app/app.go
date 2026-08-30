package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/catalog"
	"github.com/HasonoCell/OrbitOps/internal/platform/database"
	"github.com/HasonoCell/OrbitOps/internal/project"
	"github.com/HasonoCell/OrbitOps/internal/transport/httpapi"
	"github.com/gin-gonic/gin"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"
)

type Config struct {
	DatabaseURL     string
	LocalActorID    string
	LocalClusterRef string
	LocalNamespace  string
	MigrateOnBoot   bool
}

type Runtime struct {
	handler http.Handler
	db      *sqlx.DB
}

func New(ctx context.Context, config Config) (*Runtime, error) {
	if config.DatabaseURL == "" {
		return nil, errors.New("database URL is required")
	}
	if config.LocalActorID == "" {
		return nil, errors.New("local actor ID is required")
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

	projectModule := project.New(db)
	catalogModule := catalog.New(db, catalog.Config{
		ClusterRef: config.LocalClusterRef,
		Namespace:  config.LocalNamespace,
	})
	server := httpapi.NewServer(projectModule, catalogModule, config.LocalActorID)
	strictHandler := api.NewStrictHandler(server, nil)

	router := gin.New()
	router.Use(gin.Recovery())
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
