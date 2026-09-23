package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/catalog"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/webhook"
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/propagation"
)

type Server struct {
	access            *access.Module
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
	identities        *identity.Module
	browserSecurity   *BrowserSecurity
	recovery          releaseworker.RecoveryPublisher
	propagator        propagation.TextMapPropagator
}

func (s *Server) GetProject(
	ctx context.Context,
	request api.GetProjectRequestObject,
) (api.GetProjectResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	requestContext := httpRequestContext(ctx)
	existingProject, err := s.projects.Get(requestContext, request.ProjectId, requestCaller(ctx))
	if err != nil {
		if errors.Is(err, project.ErrNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetProject404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		}
		return nil, err
	}

	return api.GetProject200JSONResponse{
		Id: existingProject.ID, Name: existingProject.Name, Slug: existingProject.Slug,
		CreatedBy: existingProject.CreatedBy, CreatedAt: existingProject.CreatedAt}, nil
}

func NewServer(
	accessModule *access.Module,
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
	identities *identity.Module,
	browserSecurity *BrowserSecurity,
	recovery releaseworker.RecoveryPublisher,
	propagator propagation.TextMapPropagator,
) *Server {
	return &Server{
		access:            accessModule,
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
		identities:        identities,
		browserSecurity:   browserSecurity,
		recovery:          recovery,
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
		Caller:         requestCaller(ctx),
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

	return api.CreateProject201JSONResponse(projectResponse(createdProject)), nil
}

func httpRequestContext(ctx context.Context) context.Context {
	if ginContext, ok := ctx.(*gin.Context); ok {
		return ginContext.Request.Context()
	}
	return ctx
}

// requestCaller 只读取认证中间件写入 Request.Context 的不可构造证据。
func requestCaller(ctx context.Context) identity.Caller {
	return identity.CallerFromContext(httpRequestContext(ctx))
}
