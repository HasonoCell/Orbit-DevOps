package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
)

// RetryOperation 将普通失败重新放到目标队尾，Operation ID 与 Attempt 历史保持不变。
func (s *Server) RetryOperation(
	ctx context.Context,
	request api.RetryOperationRequestObject,
) (api.RetryOperationResponseObject, error) {
	updated, err := s.operations.Retry(httpRequestContext(ctx), operation.RetryCommand{
		OperationID:    request.OperationId,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, operation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.RetryOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.RetryOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot retry operations",
			}, nil
		case errors.Is(err, operation.ErrInvalidTransition):
			return api.RetryOperation409JSONResponse{
				Code: "operation_state_conflict", Message: "operation state does not allow retry",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.RetryOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	s.setOperationProjectID(ctx, updated)
	return api.RetryOperation200JSONResponse(operationResponse(updated)), nil
}

// CancelOperation 只停止当前 Operation，不创建或隐式执行回滚。
func (s *Server) CancelOperation(
	ctx context.Context,
	request api.CancelOperationRequestObject,
) (api.CancelOperationResponseObject, error) {
	updated, err := s.operations.Cancel(httpRequestContext(ctx), operation.CancelCommand{
		OperationID:    request.OperationId,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, operation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CancelOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CancelOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot cancel operations",
			}, nil
		case errors.Is(err, operation.ErrInvalidTransition):
			return api.CancelOperation409JSONResponse{
				Code: "operation_state_conflict", Message: "operation state does not allow cancellation",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CancelOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	s.setOperationProjectID(ctx, updated)
	return api.CancelOperation200JSONResponse(operationResponse(updated)), nil
}

func (s *Server) setOperationProjectID(ctx context.Context, record operation.Record) {
	release, err := s.delivery.GetRelease(httpRequestContext(ctx), record.ReleaseID)
	if err == nil {
		observability.SetRequestProjectID(ctx, release.TargetSnapshot.ProjectID)
	}
}
