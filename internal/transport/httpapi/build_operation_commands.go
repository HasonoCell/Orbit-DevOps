package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
)

// GetBuildOperation 先通过 Build 权限表面隐藏资源存在性，再返回完整 Attempt 历史。
func (s *Server) GetBuildOperation(ctx context.Context, request api.GetBuildOperationRequestObject) (api.GetBuildOperationResponseObject, error) {
	current, err := s.buildOperations.Get(httpRequestContext(ctx), request.BuildOperationId)
	if err != nil {
		if errors.Is(err, buildoperation.ErrNotFound) {
			return api.GetBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		}
		return nil, err
	}
	accepted, err := s.builds.Get(httpRequestContext(ctx), current.BuildID, s.localActorID)
	if err != nil {
		if errors.Is(err, build.ErrNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, accepted.Build.ProjectID)
	return api.GetBuildOperation200JSONResponse(buildOperationResponse(current)), nil
}

func (s *Server) RetryBuildOperation(ctx context.Context, request api.RetryBuildOperationRequestObject) (api.RetryBuildOperationResponseObject, error) {
	updated, err := s.buildOperations.Retry(httpRequestContext(ctx), buildoperation.RetryCommand{
		BuildOperationID: request.BuildOperationId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, buildoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.RetryBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.RetryBuildOperation403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot retry builds"}, nil
		case errors.Is(err, buildoperation.ErrInvalidTransition), errors.Is(err, idempotency.ErrConflict):
			return api.RetryBuildOperation409JSONResponse{Code: "build_operation_state_conflict", Message: "build operation cannot be retried"}, nil
		default:
			return nil, err
		}
	}
	return api.RetryBuildOperation200JSONResponse(buildOperationResponse(updated)), nil
}

func (s *Server) CancelBuildOperation(ctx context.Context, request api.CancelBuildOperationRequestObject) (api.CancelBuildOperationResponseObject, error) {
	updated, err := s.buildOperations.Cancel(httpRequestContext(ctx), buildoperation.CancelCommand{
		BuildOperationID: request.BuildOperationId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, buildoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CancelBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CancelBuildOperation403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot cancel builds"}, nil
		case errors.Is(err, buildoperation.ErrInvalidTransition), errors.Is(err, idempotency.ErrConflict):
			return api.CancelBuildOperation409JSONResponse{Code: "build_operation_state_conflict", Message: "build operation cannot be canceled"}, nil
		default:
			return nil, err
		}
	}
	return api.CancelBuildOperation200JSONResponse(buildOperationResponse(updated)), nil
}

func (s *Server) ReconcileBuildOperation(ctx context.Context, request api.ReconcileBuildOperationRequestObject) (api.ReconcileBuildOperationResponseObject, error) {
	updated, err := s.buildOperations.Reconcile(httpRequestContext(ctx), buildoperation.ReconcileCommand{
		BuildOperationID: request.BuildOperationId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, buildoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileBuildOperation403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot reconcile builds"}, nil
		case errors.Is(err, buildoperation.ErrInvalidTransition), errors.Is(err, idempotency.ErrConflict):
			return api.ReconcileBuildOperation409JSONResponse{Code: "build_operation_state_conflict", Message: "build operation cannot be reconciled"}, nil
		default:
			return nil, err
		}
	}
	return api.ReconcileBuildOperation200JSONResponse(buildOperationResponse(updated)), nil
}

func (s *Server) ForceFailBuildOperation(ctx context.Context, request api.ForceFailBuildOperationRequestObject) (api.ForceFailBuildOperationResponseObject, error) {
	updated, err := s.buildOperations.ForceFail(httpRequestContext(ctx), buildoperation.ForceFailCommand{
		BuildOperationID: request.BuildOperationId, ActorID: s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey, Reason: request.Body.Reason,
	})
	if err != nil {
		switch {
		case errors.Is(err, buildoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ForceFailBuildOperation404JSONResponse{Code: "build_operation_not_found", Message: "build operation not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ForceFailBuildOperation403JSONResponse{Code: "project_permission_denied", Message: "only the project owner can force a build to fail"}, nil
		case errors.Is(err, buildoperation.ErrInvalidTransition), errors.Is(err, idempotency.ErrConflict):
			return api.ForceFailBuildOperation409JSONResponse{Code: "build_operation_state_conflict", Message: "build operation cannot be force-failed"}, nil
		default:
			return nil, err
		}
	}
	return api.ForceFailBuildOperation200JSONResponse(buildOperationResponse(updated)), nil
}
