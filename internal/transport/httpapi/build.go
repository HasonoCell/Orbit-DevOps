package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	builddomain "github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"go.opentelemetry.io/otel/propagation"
)

// CreateBuild 把 HTTP 输入转换为不可变构建命令；Registry 与资源策略只来自服务端配置。
func (s *Server) CreateBuild(ctx context.Context, request api.CreateBuildRequestObject) (api.CreateBuildResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	traceCarrier := propagation.MapCarrier{}
	s.propagator.Inject(requestContext, traceCarrier)
	acceptance, err := s.builds.Create(requestContext, builddomain.CreateCommand{
		ApplicationID:  request.ApplicationId,
		RepositoryURL:  request.Body.RepositoryUrl,
		SourceCommit:   request.Body.SourceCommit,
		DockerfilePath: stringValue(request.Body.DockerfilePath),
		ContextPath:    stringValue(request.Body.ContextPath),
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
		TraceParent:    traceCarrier.Get("traceparent"),
		TraceState:     traceCarrier.Get("tracestate"),
	})
	if err != nil {
		switch {
		case errors.Is(err, builddomain.ErrInvalidInput):
			return api.CreateBuild400JSONResponse{Code: "invalid_build_input", Message: err.Error()}, nil
		case errors.Is(err, builddomain.ErrApplicationNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CreateBuild404JSONResponse{Code: "application_not_found", Message: "application not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CreateBuild403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot create builds"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CreateBuild409JSONResponse{Code: "idempotency_conflict", Message: "idempotency key was already used with a different request"}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, acceptance.Build.ProjectID)
	return api.CreateBuild201JSONResponse(buildAcceptanceResponse(acceptance)), nil
}

// GetBuild 通过 Build Module 的权限表面读取构建，隐藏内部 Dispatch。
func (s *Server) GetBuild(ctx context.Context, request api.GetBuildRequestObject) (api.GetBuildResponseObject, error) {
	acceptance, err := s.builds.Get(httpRequestContext(ctx), request.BuildId, s.localActorID)
	if err != nil {
		if errors.Is(err, builddomain.ErrNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetBuild404JSONResponse{Code: "build_not_found", Message: "build not found"}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, acceptance.Build.ProjectID)
	return api.GetBuild200JSONResponse(buildAcceptanceResponse(acceptance)), nil
}

func buildAcceptanceResponse(acceptance builddomain.Acceptance) api.BuildAcceptance {
	return api.BuildAcceptance{
		Build: api.Build{
			Id: acceptance.Build.ID, ProjectId: acceptance.Build.ProjectID,
			ApplicationId: acceptance.Build.ApplicationID,
			RepositoryUrl: acceptance.Build.RepositoryURL, SourceCommit: acceptance.Build.SourceCommit,
			DockerfilePath: acceptance.Build.DockerfilePath, ContextPath: acceptance.Build.ContextPath,
			Platform: acceptance.Build.Platform, DestinationRepository: acceptance.Build.DestinationRepository,
			CreatedBy: acceptance.Build.CreatedBy, CreatedAt: acceptance.Build.CreatedAt,
		},
		BuildOperation: buildOperationResponse(acceptance.BuildOperation),
	}
}

func buildOperationResponse(operation buildoperation.Record) api.BuildOperation {
	response := api.BuildOperation{
		Id: operation.ID, BuildId: operation.BuildID, CreatedBy: operation.CreatedBy,
		IdempotencyKey: operation.IdempotencyKey, Status: api.BuildOperationStatus(operation.Status),
		AttemptCount: operation.AttemptCount, AutomaticRetryCount: operation.AutomaticRetryCount,
		RecoveryRequired: operation.RecoveryRequired, ErrorCode: operation.ErrorCode,
		ErrorSummary: operation.ErrorSummary, QueuedAt: operation.QueuedAt,
		AvailableAt: operation.AvailableAt, CreatedAt: operation.CreatedAt,
		UpdatedAt: operation.UpdatedAt, StartedAt: operation.StartedAt, FinishedAt: operation.FinishedAt,
	}
	if operation.RetryDisposition != nil {
		value := api.BuildOperationRetryDisposition(*operation.RetryDisposition)
		response.RetryDisposition = &value
	}
	return response
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
