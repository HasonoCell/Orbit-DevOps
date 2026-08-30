package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/catalog"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
)

func (s *Server) CreateApplication(
	ctx context.Context,
	request api.CreateApplicationRequestObject,
) (api.CreateApplicationResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	created, err := s.catalog.CreateApplication(
		httpRequestContext(ctx),
		catalog.CreateApplicationCommand{
			ProjectID:      request.ProjectId,
			Name:           request.Body.Name,
			Slug:           request.Body.Slug,
			ActorID:        s.localActorID,
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrProjectNotFound):
			return api.CreateApplication404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CreateApplication409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}

	return api.CreateApplication201JSONResponse(applicationResponse(created)), nil
}

func (s *Server) GetApplication(
	ctx context.Context,
	request api.GetApplicationRequestObject,
) (api.GetApplicationResponseObject, error) {
	existing, err := s.catalog.GetApplication(
		httpRequestContext(ctx),
		request.ApplicationId,
	)
	if err != nil {
		if errors.Is(err, catalog.ErrApplicationNotFound) {
			return api.GetApplication404JSONResponse{
				Code:    "application_not_found",
				Message: "application not found",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, existing.ProjectID)

	return api.GetApplication200JSONResponse(applicationResponse(existing)), nil
}

func applicationResponse(application catalog.Application) api.Application {
	return api.Application{
		Id:        application.ID,
		ProjectId: application.ProjectID,
		Name:      application.Name,
		Slug:      application.Slug,
		CreatedBy: application.CreatedBy,
		CreatedAt: application.CreatedAt,
	}
}

func (s *Server) CreateDeploymentTarget(
	ctx context.Context,
	request api.CreateDeploymentTargetRequestObject,
) (api.CreateDeploymentTargetResponseObject, error) {
	created, err := s.catalog.CreateDeploymentTarget(
		httpRequestContext(ctx),
		catalog.CreateDeploymentTargetCommand{
			ApplicationID:  request.ApplicationId,
			Stage:          string(request.Body.Stage),
			Replicas:       request.Body.Replicas,
			ContainerPort:  request.Body.ContainerPort,
			ActorID:        s.localActorID,
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrApplicationNotFound):
			return api.CreateDeploymentTarget404JSONResponse{
				Code:    "application_not_found",
				Message: "application not found",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.CreateDeploymentTarget409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, created.ProjectID)

	return api.CreateDeploymentTarget201JSONResponse(deploymentTargetResponse(created)), nil
}

func (s *Server) GetDeploymentTarget(
	ctx context.Context,
	request api.GetDeploymentTargetRequestObject,
) (api.GetDeploymentTargetResponseObject, error) {
	existing, err := s.catalog.GetDeploymentTarget(
		httpRequestContext(ctx),
		request.DeploymentTargetId,
	)
	if err != nil {
		if errors.Is(err, catalog.ErrDeploymentTargetNotFound) {
			return api.GetDeploymentTarget404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, existing.ProjectID)

	return api.GetDeploymentTarget200JSONResponse(deploymentTargetResponse(existing)), nil
}

func (s *Server) UpdateDeploymentTarget(
	ctx context.Context,
	request api.UpdateDeploymentTargetRequestObject,
) (api.UpdateDeploymentTargetResponseObject, error) {
	updated, err := s.catalog.UpdateDeploymentTarget(
		httpRequestContext(ctx),
		catalog.UpdateDeploymentTargetCommand{
			ID:             request.DeploymentTargetId,
			Stage:          string(request.Body.Stage),
			Replicas:       request.Body.Replicas,
			ContainerPort:  request.Body.ContainerPort,
			ActorID:        s.localActorID,
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrDeploymentTargetNotFound):
			return api.UpdateDeploymentTarget404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
			}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.UpdateDeploymentTarget409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, updated.ProjectID)

	return api.UpdateDeploymentTarget200JSONResponse(deploymentTargetResponse(updated)), nil
}

func deploymentTargetResponse(target catalog.DeploymentTarget) api.DeploymentTarget {
	return api.DeploymentTarget{
		Id:            target.ID,
		ApplicationId: target.ApplicationID,
		Stage:         api.DeploymentTargetStage(target.Stage),
		ClusterRef:    target.ClusterRef,
		Namespace:     target.Namespace,
		Replicas:      target.Replicas,
		ContainerPort: target.ContainerPort,
		CreatedBy:     target.CreatedBy,
		CreatedAt:     target.CreatedAt,
		UpdatedAt:     target.UpdatedAt,
	}
}
