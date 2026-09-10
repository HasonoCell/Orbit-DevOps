package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/catalog"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/pipeline"
	"github.com/HasonoCell/OrbitOps/internal/project"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/HasonoCell/OrbitOps/internal/releaseworker"
	"github.com/HasonoCell/OrbitOps/internal/webhook"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/propagation"
)

type Server struct {
	projects          *project.Module
	catalog           *catalog.Module
	builds            *build.Module
	buildOperations   *buildoperation.Module
	delivery          *delivery.Module
	diagnostics       *diagnostics.Module
	pipelines         *pipeline.Module
	webhooks          *webhook.Module
	releaseOperations *releaseoperation.Module
	authorizer        *projectauth.Module
	recovery          releaseworker.RecoveryPublisher
	localActorID      string
	propagator        propagation.TextMapPropagator
}

func (s *Server) GetProject(
	ctx context.Context,
	request api.GetProjectRequestObject,
) (api.GetProjectResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	requestContext := httpRequestContext(ctx)
	if err := s.authorizer.Require(
		requestContext,
		request.ProjectId,
		s.localActorID,
		projectauth.PermissionRead,
	); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.GetProject404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		}
		return nil, err
	}

	existingProject, err := s.projects.Get(requestContext, request.ProjectId)
	if err != nil {
		if errors.Is(err, project.ErrNotFound) {
			return api.GetProject404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		}
		return nil, err
	}

	return api.GetProject200JSONResponse{
		Id:        existingProject.ID,
		Name:      existingProject.Name,
		Slug:      existingProject.Slug,
		CreatedBy: existingProject.CreatedBy,
		CreatedAt: existingProject.CreatedAt,
	}, nil
}

func NewServer(
	projects *project.Module,
	catalogModule *catalog.Module,
	buildModule *build.Module,
	buildOperationModule *buildoperation.Module,
	deliveryModule *delivery.Module,
	diagnosticModule *diagnostics.Module,
	pipelineModule *pipeline.Module,
	webhookModule *webhook.Module,
	releaseOperationModule *releaseoperation.Module,
	authorizer *projectauth.Module,
	recovery releaseworker.RecoveryPublisher,
	localActorID string,
	propagator propagation.TextMapPropagator,
) *Server {
	return &Server{
		projects:          projects,
		catalog:           catalogModule,
		builds:            buildModule,
		buildOperations:   buildOperationModule,
		delivery:          deliveryModule,
		diagnostics:       diagnosticModule,
		pipelines:         pipelineModule,
		webhooks:          webhookModule,
		releaseOperations: releaseOperationModule,
		authorizer:        authorizer,
		recovery:          recovery,
		localActorID:      localActorID,
		propagator:        propagator,
	}
}

func (s *Server) CreateProject(
	ctx context.Context,
	request api.CreateProjectRequestObject,
) (api.CreateProjectResponseObject, error) {
	requestContext := httpRequestContext(ctx)

	createdProject, err := s.projects.Create(requestContext, project.CreateCommand{
		Name:           request.Body.Name,
		Slug:           request.Body.Slug,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, project.ErrIdempotencyConflict) {
			return api.CreateProject409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, createdProject.ID)

	return api.CreateProject201JSONResponse{
		Id:        createdProject.ID,
		Name:      createdProject.Name,
		Slug:      createdProject.Slug,
		CreatedBy: createdProject.CreatedBy,
		CreatedAt: createdProject.CreatedAt,
	}, nil
}

func httpRequestContext(ctx context.Context) context.Context {
	if ginContext, ok := ctx.(*gin.Context); ok {
		return ginContext.Request.Context()
	}
	return ctx
}
