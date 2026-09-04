package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/catalog"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"go.opentelemetry.io/otel/propagation"
)

const defaultReleaseHistoryPageSize = 20

func (s *Server) CreateRelease(
	ctx context.Context,
	request api.CreateReleaseRequestObject,
) (api.CreateReleaseResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	traceCarrier := propagation.MapCarrier{}
	s.propagator.Inject(requestContext, traceCarrier)
	acceptance, err := s.delivery.CreateRelease(
		requestContext,
		delivery.CreateReleaseCommand{
			DeploymentTargetID: request.DeploymentTargetId,
			ImageReference:     request.Body.ImageReference,
			ActorID:            s.localActorID,
			IdempotencyKey:     request.Params.IdempotencyKey,
			TraceParent:        traceCarrier.Get("traceparent"),
			TraceState:         traceCarrier.Get("tracestate"),
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, delivery.ErrInvalidImageReference):
			return api.CreateRelease400JSONResponse{
				Code:    "invalid_image_reference",
				Message: "image reference must contain a valid OCI digest",
			}, nil
		case errors.Is(err, delivery.ErrDeploymentTargetNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CreateRelease404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CreateRelease403JSONResponse{
				Code:    "project_permission_denied",
				Message: "current project role cannot create releases",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CreateRelease409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, acceptance.Release.TargetSnapshot.ProjectID)

	return api.CreateRelease201JSONResponse{
		Release:   releaseResponse(acceptance.Release),
		Operation: operationResponse(acceptance.Operation),
	}, nil
}

func (s *Server) GetRelease(
	ctx context.Context,
	request api.GetReleaseRequestObject,
) (api.GetReleaseResponseObject, error) {
	detail, err := s.delivery.GetDetail(httpRequestContext(ctx), request.ReleaseId)
	if err != nil {
		if errors.Is(err, delivery.ErrReleaseNotFound) {
			return api.GetRelease404JSONResponse{
				Code:    "release_not_found",
				Message: "release not found",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, detail.Release.TargetSnapshot.ProjectID)
	if err := s.authorizer.Require(
		httpRequestContext(ctx),
		detail.Release.TargetSnapshot.ProjectID,
		s.localActorID,
		projectauth.PermissionRead,
	); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.GetRelease404JSONResponse{
				Code:    "release_not_found",
				Message: "release not found",
			}, nil
		}
		return nil, err
	}

	response, err := releaseDetailResponse(detail)
	if err != nil {
		return nil, err
	}
	return api.GetRelease200JSONResponse(response), nil
}

// ListReleaseHistory 返回轻量历史摘要；完整 Attempt 与审计只在详情接口展开。
func (s *Server) ListReleaseHistory(
	ctx context.Context,
	request api.ListReleaseHistoryRequestObject,
) (api.ListReleaseHistoryResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	target, err := s.catalog.GetDeploymentTarget(requestContext, request.DeploymentTargetId)
	if err != nil {
		if errors.Is(err, catalog.ErrDeploymentTargetNotFound) {
			return api.ListReleaseHistory404JSONResponse{
				Code: "deployment_target_not_found", Message: "deployment target not found",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, target.ProjectID)
	if err := s.authorizer.Require(
		requestContext,
		target.ProjectID,
		s.localActorID,
		projectauth.PermissionRead,
	); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.ListReleaseHistory404JSONResponse{
				Code: "deployment_target_not_found", Message: "deployment target not found",
			}, nil
		}
		return nil, err
	}
	limit := defaultReleaseHistoryPageSize
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.delivery.ListHistory(requestContext, delivery.ListHistoryQuery{
		DeploymentTargetID: request.DeploymentTargetId,
		Limit:              limit,
		Cursor:             cursor,
	})
	if err != nil {
		if errors.Is(err, delivery.ErrInvalidCursor) {
			return api.ListReleaseHistory400JSONResponse{
				Code: "invalid_release_cursor", Message: "release history cursor is invalid",
			}, nil
		}
		return nil, err
	}
	items := make([]api.ReleaseHistoryItem, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, api.ReleaseHistoryItem{
			Release: releaseResponse(item.Release),
			Operation: api.OperationSummary{
				Id:           item.Operation.ID,
				Status:       api.OperationSummaryStatus(item.Operation.Status),
				AttemptCount: item.Operation.AttemptCount,
				ErrorCode:    item.Operation.ErrorCode,
				ErrorSummary: item.Operation.ErrorSummary,
				QueuedAt:     item.Operation.QueuedAt,
				StartedAt:    item.Operation.StartedAt,
				FinishedAt:   item.Operation.FinishedAt,
			},
		})
	}
	return api.ListReleaseHistory200JSONResponse{
		Items: items, NextCursor: page.NextCursor,
	}, nil
}

// RollbackRelease 将历史 Release 的完整快照作为新的、可审计的发布意图接纳。
func (s *Server) RollbackRelease(
	ctx context.Context,
	request api.RollbackReleaseRequestObject,
) (api.RollbackReleaseResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	traceCarrier := propagation.MapCarrier{}
	s.propagator.Inject(requestContext, traceCarrier)
	acceptance, err := s.delivery.Rollback(requestContext, delivery.RollbackCommand{
		SourceReleaseID: request.ReleaseId,
		ActorID:         s.localActorID,
		IdempotencyKey:  request.Params.IdempotencyKey,
		TraceParent:     traceCarrier.Get("traceparent"),
		TraceState:      traceCarrier.Get("tracestate"),
	})
	if err != nil {
		switch {
		case errors.Is(err, delivery.ErrReleaseNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.RollbackRelease404JSONResponse{
				Code: "release_not_found", Message: "release not found",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.RollbackRelease403JSONResponse{
				Code: "project_permission_denied", Message: "current project role cannot create rollbacks",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.RollbackRelease409JSONResponse{
				Code: "idempotency_conflict", Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, acceptance.Release.TargetSnapshot.ProjectID)
	return api.RollbackRelease201JSONResponse{
		Release: releaseResponse(acceptance.Release), Operation: operationResponse(acceptance.Operation),
	}, nil
}

func (s *Server) GetOperation(
	ctx context.Context,
	request api.GetOperationRequestObject,
) (api.GetOperationResponseObject, error) {
	operationRecord, err := s.operations.Get(httpRequestContext(ctx), request.OperationId)
	if err != nil {
		if errors.Is(err, operation.ErrNotFound) {
			return api.GetOperation404JSONResponse{
				Code:    "operation_not_found",
				Message: "operation not found",
			}, nil
		}
		return nil, err
	}
	release, err := s.delivery.GetRelease(httpRequestContext(ctx), operationRecord.ReleaseID)
	if err != nil {
		return nil, err
	}
	observability.SetRequestProjectID(ctx, release.TargetSnapshot.ProjectID)
	if err := s.authorizer.Require(
		httpRequestContext(ctx),
		release.TargetSnapshot.ProjectID,
		s.localActorID,
		projectauth.PermissionRead,
	); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.GetOperation404JSONResponse{
				Code:    "operation_not_found",
				Message: "operation not found",
			}, nil
		}
		return nil, err
	}

	return api.GetOperation200JSONResponse(operationResponse(operationRecord)), nil
}

func releaseResponse(release delivery.Release) api.Release {
	return api.Release{
		Id:                  release.ID,
		DeploymentTargetId:  release.DeploymentTargetID,
		ImageReference:      release.ImageReference,
		RollbackOfReleaseId: release.RollbackOfReleaseID,
		TargetSnapshot: api.ReleaseTargetSnapshot{
			ProjectId:     release.TargetSnapshot.ProjectID,
			ApplicationId: release.TargetSnapshot.ApplicationID,
			Stage:         api.ReleaseTargetSnapshotStage(release.TargetSnapshot.Stage),
			ClusterRef:    release.TargetSnapshot.ClusterRef,
			Namespace:     release.TargetSnapshot.Namespace,
			Replicas:      release.TargetSnapshot.Replicas,
			ContainerPort: release.TargetSnapshot.ContainerPort,
		},
		CreatedBy: release.CreatedBy,
		CreatedAt: release.CreatedAt,
	}
}

func releaseDetailResponse(detail delivery.Detail) (api.ReleaseDetail, error) {
	differences := make([]api.SnapshotDifference, 0, len(detail.SnapshotDifferences))
	for _, difference := range detail.SnapshotDifferences {
		differences = append(differences, api.SnapshotDifference{
			Field:        difference.Field,
			ReleaseValue: difference.ReleaseValue,
			CurrentValue: difference.CurrentValue,
		})
	}
	timeline := make([]api.AuditRecord, 0, len(detail.AuditTimeline))
	for _, record := range detail.AuditTimeline {
		var summary map[string]interface{}
		if err := json.Unmarshal(record.Summary, &summary); err != nil {
			return api.ReleaseDetail{}, fmt.Errorf("decode release audit summary: %w", err)
		}
		timeline = append(timeline, api.AuditRecord{
			Id:         record.ID,
			ActorId:    record.ActorID,
			ActorKind:  api.AuditRecordActorKind(record.ActorKind),
			Action:     record.Action,
			TargetType: record.TargetType,
			TargetId:   record.TargetID,
			Summary:    summary,
			CreatedAt:  record.CreatedAt,
		})
	}
	return api.ReleaseDetail{
		Release:             releaseResponse(detail.Release),
		SnapshotDifferences: differences,
		Operation:           operationResponse(detail.Operation),
		AuditTimeline:       timeline,
	}, nil
}

func operationResponse(record operation.Record) api.Operation {
	return api.Operation{
		Id:                  record.ID,
		Type:                api.OperationType(record.Type),
		ReleaseId:           record.ReleaseID,
		DeploymentTargetId:  record.DeploymentTargetID,
		CreatedBy:           record.CreatedBy,
		IdempotencyKey:      record.IdempotencyKey,
		Status:              api.OperationStatus(record.Status),
		AttemptCount:        record.AttemptCount,
		AutomaticRetryCount: record.AutomaticRetryCount,
		RecoveryRequired:    record.RecoveryRequired,
		ErrorCode:           record.ErrorCode,
		ErrorSummary:        record.ErrorSummary,
		RetryDisposition:    operationRetryDisposition(record.RetryDisposition),
		QueuedAt:            record.QueuedAt,
		AvailableAt:         record.AvailableAt,
		CreatedAt:           record.CreatedAt,
		UpdatedAt:           record.UpdatedAt,
		StartedAt:           record.StartedAt,
		FinishedAt:          record.FinishedAt,
		Attempts:            operationAttemptResponses(record.Attempts),
	}
}

func operationAttemptResponses(attempts []operation.Attempt) []api.OperationAttempt {
	responses := make([]api.OperationAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		responses = append(responses, api.OperationAttempt{
			Id:               attempt.ID,
			Number:           attempt.Number,
			WorkerId:         attempt.WorkerID,
			Status:           api.OperationAttemptStatus(attempt.Status),
			ErrorCode:        attempt.ErrorCode,
			ErrorSummary:     attempt.ErrorSummary,
			RetryDisposition: operationRetryDisposition(attempt.RetryDisposition),
			StartedAt:        attempt.StartedAt,
			FinishedAt:       attempt.FinishedAt,
		})
	}
	return responses
}

func operationRetryDisposition(value *string) *api.RetryDisposition {
	if value == nil {
		return nil
	}
	disposition := api.RetryDisposition(*value)
	return &disposition
}
