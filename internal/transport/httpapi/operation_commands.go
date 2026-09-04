package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
)

// RetryOperation 将普通失败重新放到目标队尾；未知结果只有经 owner 只读确认后才能重试。
func (s *Server) RetryOperation(
	ctx context.Context,
	request api.RetryOperationRequestObject,
) (api.RetryOperationResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	command := operation.RetryCommand{
		OperationID:    request.OperationId,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	}
	current, err := s.operations.Get(requestContext, request.OperationId)
	if err != nil {
		if errors.Is(err, operation.ErrNotFound) {
			return api.RetryOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		}
		return nil, err
	}
	if current.Status == operation.StatusAttentionRequired {
		release, authorizationErr := s.authorizeRecoveryCommand(
			ctx,
			current,
			projectauth.PermissionResolveUnknown,
		)
		if authorizationErr != nil {
			return s.retryAuthorizationError(authorizationErr)
		}
		if s.recovery == nil {
			return api.RetryOperation409JSONResponse{
				Code:    "recovery_inspection_unavailable",
				Message: "external state cannot be inspected before retry",
			}, nil
		}
		observation, inspectionErr := s.recovery.InspectRecovery(
			requestContext,
			publishRequest(current, release),
		)
		if inspectionErr != nil {
			return api.RetryOperation409JSONResponse{
				Code:    "recovery_inspection_failed",
				Message: "external state could not be confirmed safe for retry",
			}, nil
		}
		safe, safetyErr := s.retryIsSafe(requestContext, current, observation)
		if safetyErr != nil {
			return nil, safetyErr
		}
		if !safe {
			return api.RetryOperation409JSONResponse{
				Code:    "unsafe_operation_retry",
				Message: "external state does not allow this operation to be retried safely",
			}, nil
		}
		command.AttentionConfirmed = true
		command.ExpectedUpdatedAt = current.UpdatedAt
	}
	updated, err := s.operations.Retry(requestContext, command)
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
		case errors.Is(err, operation.ErrStaleObservation):
			return api.RetryOperation409JSONResponse{
				Code: "operation_changed", Message: "operation changed after external inspection",
			}, nil
		default:
			return nil, err
		}
	}
	s.setOperationProjectID(ctx, updated)
	return api.RetryOperation200JSONResponse(operationResponse(updated)), nil
}

// ReconcileOperation 读取 Kubernetes 权威状态并保存核验结论，本身不执行任何发布写入。
func (s *Server) ReconcileOperation(
	ctx context.Context,
	request api.ReconcileOperationRequestObject,
) (api.ReconcileOperationResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	current, err := s.operations.Get(requestContext, request.OperationId)
	if err != nil {
		if errors.Is(err, operation.ErrNotFound) {
			return api.ReconcileOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		}
		return nil, err
	}
	release, err := s.authorizeRecoveryCommand(ctx, current, projectauth.PermissionDevelop)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot reconcile operations",
			}, nil
		default:
			return nil, err
		}
	}
	evidence := operation.ReconcileEvidence{
		Resolution:        operation.ReconcileUnclear,
		ErrorCode:         "operation_not_attention_required",
		ErrorSummary:      "Operation does not currently require manual reconciliation",
		ExpectedUpdatedAt: current.UpdatedAt,
	}
	if current.Status == operation.StatusAttentionRequired {
		if s.recovery == nil {
			return api.ReconcileOperation409JSONResponse{
				Code: "recovery_inspection_unavailable", Message: "external state inspection is unavailable",
			}, nil
		}
		observation, inspectionErr := s.recovery.InspectRecovery(
			requestContext,
			publishRequest(current, release),
		)
		if inspectionErr != nil {
			evidence.ErrorCode = "recovery_inspection_failed"
			evidence.ErrorSummary = "Kubernetes state could not be read during manual reconciliation"
		} else if err := s.resolveObservation(requestContext, current, observation, &evidence); err != nil {
			return nil, err
		}
	}

	updated, err := s.operations.ReconcileAttention(requestContext, operation.ReconcileCommand{
		OperationID:    request.OperationId,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
		Evidence:       evidence,
	})
	if err != nil {
		switch {
		case errors.Is(err, operation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot reconcile operations",
			}, nil
		case errors.Is(err, operation.ErrInvalidTransition):
			return api.ReconcileOperation409JSONResponse{
				Code: "operation_state_conflict", Message: "operation state does not allow reconciliation",
			}, nil
		case errors.Is(err, operation.ErrStaleObservation):
			return api.ReconcileOperation409JSONResponse{
				Code: "operation_changed", Message: "operation changed after external inspection",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.ReconcileOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	return api.ReconcileOperation200JSONResponse(operationResponse(updated)), nil
}

// ForceFailOperation 仅允许 owner 用人工原因解除未知结果对目标队列的阻塞。
func (s *Server) ForceFailOperation(
	ctx context.Context,
	request api.ForceFailOperationRequestObject,
) (api.ForceFailOperationResponseObject, error) {
	updated, err := s.operations.ForceFailAttention(
		httpRequestContext(ctx),
		operation.ForceFailCommand{
			OperationID:    request.OperationId,
			ActorID:        s.localActorID,
			IdempotencyKey: request.Params.IdempotencyKey,
			Reason:         request.Body.Reason,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, operation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ForceFailOperation404JSONResponse{
				Code: "operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ForceFailOperation403JSONResponse{
				Code: "project_permission_denied", Message: "only the project owner can force an operation to fail",
			}, nil
		case errors.Is(err, operation.ErrInvalidTransition):
			return api.ForceFailOperation409JSONResponse{
				Code: "operation_state_conflict", Message: "operation state does not allow manual failure",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.ForceFailOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	s.setOperationProjectID(ctx, updated)
	return api.ForceFailOperation200JSONResponse(operationResponse(updated)), nil
}

// authorizeRecoveryCommand 在访问 Kubernetes 前完成项目授权，避免未授权请求触发外部读取。
func (s *Server) authorizeRecoveryCommand(
	ctx context.Context,
	record operation.Record,
	permission projectauth.Permission,
) (delivery.Release, error) {
	release, err := s.delivery.GetRelease(httpRequestContext(ctx), record.ReleaseID)
	if err != nil {
		return delivery.Release{}, err
	}
	observability.SetRequestProjectID(ctx, release.TargetSnapshot.ProjectID)
	if err := s.authorizer.Require(
		httpRequestContext(ctx),
		release.TargetSnapshot.ProjectID,
		s.localActorID,
		permission,
	); err != nil {
		return delivery.Release{}, err
	}
	return release, nil
}

func (s *Server) retryAuthorizationError(err error) (api.RetryOperationResponseObject, error) {
	switch {
	case errors.Is(err, projectauth.ErrNotMember):
		return api.RetryOperation404JSONResponse{
			Code: "operation_not_found", Message: "operation not found",
		}, nil
	case errors.Is(err, projectauth.ErrForbidden):
		return api.RetryOperation403JSONResponse{
			Code: "project_permission_denied", Message: "current project role cannot retry operations",
		}, nil
	default:
		return nil, err
	}
}

// retryIsSafe 只接受“尚未写入”或“目标仍由已知历史发布占用”的可覆盖事实。
func (s *Server) retryIsSafe(
	ctx context.Context,
	record operation.Record,
	observation worker.RecoveryObservation,
) (bool, error) {
	switch observation.Action {
	case worker.RecoveryApply:
		return true, nil
	case worker.RecoveryReleaseObserved:
		if observation.ObservedReleaseID == nil {
			return false, nil
		}
		return s.delivery.IsKnownReleaseForTarget(
			ctx,
			*observation.ObservedReleaseID,
			record.DeploymentTargetID,
		)
	default:
		return false, nil
	}
}

// resolveObservation 把只读观察归一成稳定的领域结论；未知外部发布不会被误判为可覆盖。
func (s *Server) resolveObservation(
	ctx context.Context,
	record operation.Record,
	observation worker.RecoveryObservation,
	evidence *operation.ReconcileEvidence,
) error {
	switch observation.Action {
	case worker.RecoverySucceeded:
		evidence.Resolution = operation.ReconcileSucceeded
		evidence.ErrorCode = "release_ready"
		evidence.ErrorSummary = "Kubernetes resources are ready for this release"
	case worker.RecoveryApply:
		evidence.Resolution = operation.ReconcileFailed
		evidence.ErrorCode = "release_not_applied"
		evidence.ErrorSummary = "Kubernetes resources do not show a completed application of this release"
	case worker.RecoveryObserve:
		evidence.Resolution = operation.ReconcileUnclear
		evidence.ErrorCode = "release_still_progressing"
		evidence.ErrorSummary = "Kubernetes resources still show this release progressing"
	case worker.RecoveryReleaseObserved:
		if observation.ObservedReleaseID == nil {
			evidence.Resolution = operation.ReconcileUnclear
			evidence.ErrorCode = "recovery_state_invalid"
			evidence.ErrorSummary = "Kubernetes observation omitted the active release identifier"
			return nil
		}
		known, err := s.delivery.IsKnownReleaseForTarget(
			ctx,
			*observation.ObservedReleaseID,
			record.DeploymentTargetID,
		)
		if err != nil {
			return err
		}
		if known {
			evidence.Resolution = operation.ReconcileFailed
			evidence.ErrorCode = "different_known_release_active"
			evidence.ErrorSummary = "Kubernetes resources still reference another known release"
		} else {
			evidence.Resolution = operation.ReconcileUnclear
			evidence.ErrorCode = "unexpected_release_observed"
			evidence.ErrorSummary = "Kubernetes resources reference a release outside this target history"
		}
	case worker.RecoveryAttention:
		evidence.Resolution = operation.ReconcileUnclear
		evidence.ErrorCode = observation.ErrorCode
		evidence.ErrorSummary = observation.ErrorSummary
		if evidence.ErrorCode == "" {
			evidence.ErrorCode = "recovery_state_conflict"
		}
		if evidence.ErrorSummary == "" {
			evidence.ErrorSummary = "Kubernetes state cannot be reconciled safely"
		}
	default:
		evidence.Resolution = operation.ReconcileUnclear
		evidence.ErrorCode = "recovery_state_invalid"
		evidence.ErrorSummary = "Kubernetes returned an unsupported recovery observation"
	}
	return nil
}

func publishRequest(record operation.Record, release delivery.Release) worker.PublishRequest {
	return worker.PublishRequest{
		OperationID:        record.ID,
		ReleaseID:          release.ID,
		ProjectID:          release.TargetSnapshot.ProjectID,
		ApplicationID:      release.TargetSnapshot.ApplicationID,
		DeploymentTargetID: release.DeploymentTargetID,
		ImageReference:     release.ImageReference,
		Stage:              release.TargetSnapshot.Stage,
		ClusterRef:         release.TargetSnapshot.ClusterRef,
		Namespace:          release.TargetSnapshot.Namespace,
		Replicas:           release.TargetSnapshot.Replicas,
		ContainerPort:      release.TargetSnapshot.ContainerPort,
	}
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
