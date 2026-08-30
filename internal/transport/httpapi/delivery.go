package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/operation"
)

func (s *Server) CreateRelease(
	ctx context.Context,
	request api.CreateReleaseRequestObject,
) (api.CreateReleaseResponseObject, error) {
	acceptance, err := s.delivery.CreateRelease(
		httpRequestContext(ctx),
		delivery.CreateReleaseCommand{
			DeploymentTargetID: request.DeploymentTargetId,
			ImageReference:     request.Body.ImageReference,
			ActorID:            s.localActorID,
			IdempotencyKey:     request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, delivery.ErrInvalidImageReference):
			return api.CreateRelease400JSONResponse{
				Code:    "invalid_image_reference",
				Message: "image reference must contain a valid OCI digest",
			}, nil
		case errors.Is(err, delivery.ErrDeploymentTargetNotFound):
			return api.CreateRelease404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
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

	return api.CreateRelease201JSONResponse{
		Release:   releaseResponse(acceptance.Release),
		Operation: operationResponse(acceptance.Operation),
	}, nil
}

func (s *Server) GetRelease(
	ctx context.Context,
	request api.GetReleaseRequestObject,
) (api.GetReleaseResponseObject, error) {
	release, err := s.delivery.GetRelease(httpRequestContext(ctx), request.ReleaseId)
	if err != nil {
		if errors.Is(err, delivery.ErrReleaseNotFound) {
			return api.GetRelease404JSONResponse{
				Code:    "release_not_found",
				Message: "release not found",
			}, nil
		}
		return nil, err
	}

	return api.GetRelease200JSONResponse(releaseResponse(release)), nil
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

	return api.GetOperation200JSONResponse(operationResponse(operationRecord)), nil
}

func releaseResponse(release delivery.Release) api.Release {
	return api.Release{
		Id:                 release.ID,
		DeploymentTargetId: release.DeploymentTargetID,
		ImageReference:     release.ImageReference,
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

func operationResponse(record operation.Record) api.Operation {
	return api.Operation{
		Id:             record.ID,
		Type:           api.OperationType(record.Type),
		ReleaseId:      record.ReleaseID,
		CreatedBy:      record.CreatedBy,
		IdempotencyKey: record.IdempotencyKey,
		Status:         api.OperationStatus(record.Status),
		AttemptCount:   record.AttemptCount,
		ErrorCategory:  record.ErrorCategory,
		ErrorSummary:   record.ErrorSummary,
		CreatedAt:      record.CreatedAt,
		UpdatedAt:      record.UpdatedAt,
		StartedAt:      record.StartedAt,
		FinishedAt:     record.FinishedAt,
		Attempts:       operationAttemptResponses(record.Attempts),
	}
}

func operationAttemptResponses(attempts []operation.Attempt) []api.OperationAttempt {
	responses := make([]api.OperationAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		responses = append(responses, api.OperationAttempt{
			Id:            attempt.ID,
			Number:        attempt.Number,
			WorkerId:      attempt.WorkerID,
			Status:        api.OperationAttemptStatus(attempt.Status),
			ErrorCategory: attempt.ErrorCategory,
			ErrorSummary:  attempt.ErrorSummary,
			StartedAt:     attempt.StartedAt,
			FinishedAt:    attempt.FinishedAt,
		})
	}
	return responses
}
