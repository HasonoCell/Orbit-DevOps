package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/HasonoCell/OrbitOps/internal/releaseworker"
)

// RetryReleaseOperation 将普通失败重新放到目标队尾；未知结果只有经 owner 只读确认后才能重试。
func (s *Server) RetryReleaseOperation(
	ctx context.Context,
	request api.RetryReleaseOperationRequestObject,
) (api.RetryReleaseOperationResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	command := releaseoperation.RetryCommand{
		ReleaseOperationID: request.ReleaseOperationId,
		ActorID:            s.localActorID,
		IdempotencyKey:     request.Params.IdempotencyKey,
	}
	current, err := s.releaseOperations.Get(requestContext, request.ReleaseOperationId)
	if err != nil {
		if errors.Is(err, releaseoperation.ErrNotFound) {
			return api.RetryReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		}
		return nil, err
	}
	if current.Status == releaseoperation.StatusAttentionRequired {
		release, authorizationErr := s.authorizeRecoveryCommand(
			ctx,
			current,
			projectauth.PermissionResolveUnknown,
		)
		if authorizationErr != nil {
			return s.retryAuthorizationError(authorizationErr)
		}
		if s.recovery == nil {
			return api.RetryReleaseOperation409JSONResponse{
				Code:    "recovery_inspection_unavailable",
				Message: "external state cannot be inspected before retry",
			}, nil
		}
		observation, inspectionErr := s.recovery.InspectRecovery(
			requestContext,
			publishRequest(current, release),
		)
		if inspectionErr != nil {
			return api.RetryReleaseOperation409JSONResponse{
				Code:    "recovery_inspection_failed",
				Message: "external state could not be confirmed safe for retry",
			}, nil
		}
		safe, safetyErr := s.retryIsSafe(requestContext, current, observation)
		if safetyErr != nil {
			return nil, safetyErr
		}
		if !safe {
			return api.RetryReleaseOperation409JSONResponse{
				Code:    "unsafe_release_operation_retry",
				Message: "external state does not allow this operation to be retried safely",
			}, nil
		}
		command.AttentionConfirmed = true
		command.ExpectedUpdatedAt = current.UpdatedAt
	}
	updated, err := s.releaseOperations.Retry(requestContext, command)
	if err != nil {
		switch {
		case errors.Is(err, releaseoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.RetryReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.RetryReleaseOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot retry operations",
			}, nil
		case errors.Is(err, releaseoperation.ErrInvalidTransition):
			return api.RetryReleaseOperation409JSONResponse{
				Code: "release_operation_state_conflict", Message: "operation state does not allow retry",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.RetryReleaseOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		case errors.Is(err, releaseoperation.ErrStaleObservation):
			return api.RetryReleaseOperation409JSONResponse{
				Code: "release_operation_changed", Message: "operation changed after external inspection",
			}, nil
		default:
			return nil, err
		}
	}
	s.setReleaseOperationProjectID(ctx, updated)
	return api.RetryReleaseOperation200JSONResponse(releaseOperationResponse(updated)), nil
}

// ReconcileReleaseOperation 读取 Kubernetes 权威状态并保存核验结论，本身不执行任何发布写入。
func (s *Server) ReconcileReleaseOperation(
	ctx context.Context,
	request api.ReconcileReleaseOperationRequestObject,
) (api.ReconcileReleaseOperationResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	current, err := s.releaseOperations.Get(requestContext, request.ReleaseOperationId)
	if err != nil {
		if errors.Is(err, releaseoperation.ErrNotFound) {
			return api.ReconcileReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		}
		return nil, err
	}
	release, err := s.authorizeRecoveryCommand(ctx, current, projectauth.PermissionDevelop)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileReleaseOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot reconcile operations",
			}, nil
		default:
			return nil, err
		}
	}
	evidence := releaseoperation.ReconcileEvidence{
		Resolution:        releaseoperation.ReconcileUnclear,
		ErrorCode:         "release_operation_not_attention_required",
		ErrorSummary:      "ReleaseOperation does not currently require manual reconciliation",
		ExpectedUpdatedAt: current.UpdatedAt,
	}
	if current.Status == releaseoperation.StatusAttentionRequired {
		if s.recovery == nil {
			return api.ReconcileReleaseOperation409JSONResponse{
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

	updated, err := s.releaseOperations.ReconcileAttention(requestContext, releaseoperation.ReconcileCommand{
		ReleaseOperationID: request.ReleaseOperationId,
		ActorID:            s.localActorID,
		IdempotencyKey:     request.Params.IdempotencyKey,
		Evidence:           evidence,
	})
	if err != nil {
		switch {
		case errors.Is(err, releaseoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileReleaseOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot reconcile operations",
			}, nil
		case errors.Is(err, releaseoperation.ErrInvalidTransition):
			return api.ReconcileReleaseOperation409JSONResponse{
				Code: "release_operation_state_conflict", Message: "operation state does not allow reconciliation",
			}, nil
		case errors.Is(err, releaseoperation.ErrStaleObservation):
			return api.ReconcileReleaseOperation409JSONResponse{
				Code: "release_operation_changed", Message: "operation changed after external inspection",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.ReconcileReleaseOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	return api.ReconcileReleaseOperation200JSONResponse(releaseOperationResponse(updated)), nil
}

// ForceFailReleaseOperation 仅允许 owner 用人工原因解除未知结果对目标队列的阻塞。
func (s *Server) ForceFailReleaseOperation(
	ctx context.Context,
	request api.ForceFailReleaseOperationRequestObject,
) (api.ForceFailReleaseOperationResponseObject, error) {
	updated, err := s.releaseOperations.ForceFailAttention(
		httpRequestContext(ctx),
		releaseoperation.ForceFailCommand{
			ReleaseOperationID: request.ReleaseOperationId,
			ActorID:            s.localActorID,
			IdempotencyKey:     request.Params.IdempotencyKey,
			Reason:             request.Body.Reason,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, releaseoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ForceFailReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ForceFailReleaseOperation403JSONResponse{
				Code: "project_permission_denied", Message: "only the project owner can force an operation to fail",
			}, nil
		case errors.Is(err, releaseoperation.ErrInvalidTransition):
			return api.ForceFailReleaseOperation409JSONResponse{
				Code: "release_operation_state_conflict", Message: "operation state does not allow manual failure",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.ForceFailReleaseOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	s.setReleaseOperationProjectID(ctx, updated)
	return api.ForceFailReleaseOperation200JSONResponse(releaseOperationResponse(updated)), nil
}

// authorizeRecoveryCommand 在访问 Kubernetes 前完成项目授权，避免未授权请求触发外部读取。
func (s *Server) authorizeRecoveryCommand(
	ctx context.Context,
	record releaseoperation.Record,
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

func (s *Server) retryAuthorizationError(err error) (api.RetryReleaseOperationResponseObject, error) {
	switch {
	case errors.Is(err, projectauth.ErrNotMember):
		return api.RetryReleaseOperation404JSONResponse{
			Code: "release_operation_not_found", Message: "operation not found",
		}, nil
	case errors.Is(err, projectauth.ErrForbidden):
		return api.RetryReleaseOperation403JSONResponse{
			Code: "project_permission_denied", Message: "current project role cannot retry operations",
		}, nil
	default:
		return nil, err
	}
}

// retryIsSafe 只接受“尚未写入”或“目标仍由已知历史发布占用”的可覆盖事实。
func (s *Server) retryIsSafe(
	ctx context.Context,
	record releaseoperation.Record,
	observation releaseworker.RecoveryObservation,
) (bool, error) {
	switch observation.Action {
	case releaseworker.RecoveryApply:
		return true, nil
	case releaseworker.RecoveryReleaseObserved:
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
	record releaseoperation.Record,
	observation releaseworker.RecoveryObservation,
	evidence *releaseoperation.ReconcileEvidence,
) error {
	switch observation.Action {
	case releaseworker.RecoverySucceeded:
		evidence.Resolution = releaseoperation.ReconcileSucceeded
		evidence.ErrorCode = "release_ready"
		evidence.ErrorSummary = "Kubernetes resources are ready for this release"
	case releaseworker.RecoveryApply:
		evidence.Resolution = releaseoperation.ReconcileFailed
		evidence.ErrorCode = "release_not_applied"
		evidence.ErrorSummary = "Kubernetes resources do not show a completed application of this release"
	case releaseworker.RecoveryObserve:
		evidence.Resolution = releaseoperation.ReconcileUnclear
		evidence.ErrorCode = "release_still_progressing"
		evidence.ErrorSummary = "Kubernetes resources still show this release progressing"
	case releaseworker.RecoveryReleaseObserved:
		if observation.ObservedReleaseID == nil {
			evidence.Resolution = releaseoperation.ReconcileUnclear
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
			evidence.Resolution = releaseoperation.ReconcileFailed
			evidence.ErrorCode = "different_known_release_active"
			evidence.ErrorSummary = "Kubernetes resources still reference another known release"
		} else {
			evidence.Resolution = releaseoperation.ReconcileUnclear
			evidence.ErrorCode = "unexpected_release_observed"
			evidence.ErrorSummary = "Kubernetes resources reference a release outside this target history"
		}
	case releaseworker.RecoveryAttention:
		evidence.Resolution = releaseoperation.ReconcileUnclear
		evidence.ErrorCode = observation.ErrorCode
		evidence.ErrorSummary = observation.ErrorSummary
		if evidence.ErrorCode == "" {
			evidence.ErrorCode = "recovery_state_conflict"
		}
		if evidence.ErrorSummary == "" {
			evidence.ErrorSummary = "Kubernetes state cannot be reconciled safely"
		}
	default:
		evidence.Resolution = releaseoperation.ReconcileUnclear
		evidence.ErrorCode = "recovery_state_invalid"
		evidence.ErrorSummary = "Kubernetes returned an unsupported recovery observation"
	}
	return nil
}

func publishRequest(record releaseoperation.Record, release delivery.Release) releaseworker.PublishRequest {
	return releaseworker.PublishRequest{
		ReleaseOperationID: record.ID,
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

// CancelReleaseOperation 只停止当前 ReleaseOperation，不创建或隐式执行回滚。
func (s *Server) CancelReleaseOperation(
	ctx context.Context,
	request api.CancelReleaseOperationRequestObject,
) (api.CancelReleaseOperationResponseObject, error) {
	updated, err := s.releaseOperations.Cancel(httpRequestContext(ctx), releaseoperation.CancelCommand{
		ReleaseOperationID: request.ReleaseOperationId,
		ActorID:            s.localActorID,
		IdempotencyKey:     request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, releaseoperation.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CancelReleaseOperation404JSONResponse{
				Code: "release_operation_not_found", Message: "operation not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CancelReleaseOperation403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot cancel operations",
			}, nil
		case errors.Is(err, releaseoperation.ErrInvalidTransition):
			return api.CancelReleaseOperation409JSONResponse{
				Code: "release_operation_state_conflict", Message: "operation state does not allow cancellation",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CancelReleaseOperation409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	s.setReleaseOperationProjectID(ctx, updated)
	return api.CancelReleaseOperation200JSONResponse(releaseOperationResponse(updated)), nil
}

func (s *Server) setReleaseOperationProjectID(ctx context.Context, record releaseoperation.Record) {
	release, err := s.delivery.GetRelease(httpRequestContext(ctx), record.ReleaseID)
	if err == nil {
		observability.SetRequestProjectID(ctx, release.TargetSnapshot.ProjectID)
	}
}
