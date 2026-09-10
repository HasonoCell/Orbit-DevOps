package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/pipeline"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"go.opentelemetry.io/otel/propagation"
)

func (s *Server) CreateDeliveryPipeline(ctx context.Context, request api.CreateDeliveryPipelineRequestObject) (api.CreateDeliveryPipelineResponseObject, error) {
	created, err := s.pipelines.Create(httpRequestContext(ctx), pipeline.CreateCommand{
		ApplicationID: request.ApplicationId, Name: request.Body.Name,
		EndpointKey: request.Body.EndpointKey, RepositoryURL: request.Body.RepositoryUrl,
		Branch: request.Body.Branch, DockerfilePath: stringValue(request.Body.DockerfilePath),
		ContextPath: stringValue(request.Body.ContextPath), Mode: string(request.Body.Mode),
		DeploymentTargetID: request.Body.DeploymentTargetId, ActorID: s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, pipeline.ErrInvalidInput), errors.Is(err, pipeline.ErrSourceNotFound):
			return api.CreateDeliveryPipeline400JSONResponse{Code: "invalid_delivery_pipeline", Message: err.Error()}, nil
		case errors.Is(err, pipeline.ErrApplicationNotFound), errors.Is(err, pipeline.ErrTargetNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.CreateDeliveryPipeline404JSONResponse{Code: "delivery_pipeline_dependency_not_found", Message: "application or deployment target not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CreateDeliveryPipeline403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage delivery pipelines"}, nil
		case errors.Is(err, pipeline.ErrNameConflict), errors.Is(err, idempotency.ErrConflict):
			return api.CreateDeliveryPipeline409JSONResponse{Code: "idempotency_conflict", Message: "idempotency key was already used with a different request"}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, created.Pipeline.ProjectID)
	return api.CreateDeliveryPipeline201JSONResponse(pipelineResponse(created)), nil
}

func (s *Server) ListDeliveryPipelines(ctx context.Context, request api.ListDeliveryPipelinesRequestObject) (api.ListDeliveryPipelinesResponseObject, error) {
	items, err := s.pipelines.List(httpRequestContext(ctx), request.ApplicationId, s.localActorID)
	if err != nil {
		if errors.Is(err, pipeline.ErrApplicationNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.ListDeliveryPipelines404JSONResponse{Code: "application_not_found", Message: "application not found"}, nil
		}
		return nil, err
	}
	result := make(api.ListDeliveryPipelines200JSONResponse, 0, len(items))
	for _, item := range items {
		result = append(result, pipelineResponse(item))
	}
	return result, nil
}

func (s *Server) GetDeliveryPipeline(ctx context.Context, request api.GetDeliveryPipelineRequestObject) (api.GetDeliveryPipelineResponseObject, error) {
	detail, err := s.pipelines.Get(httpRequestContext(ctx), request.DeliveryPipelineId, s.localActorID)
	if err != nil {
		if errors.Is(err, pipeline.ErrNotFound) {
			return api.GetDeliveryPipeline404JSONResponse{Code: "delivery_pipeline_not_found", Message: "delivery pipeline not found"}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, detail.Pipeline.ProjectID)
	return api.GetDeliveryPipeline200JSONResponse(pipelineResponse(detail)), nil
}

func (s *Server) UpdateDeliveryPipeline(ctx context.Context, request api.UpdateDeliveryPipelineRequestObject) (api.UpdateDeliveryPipelineResponseObject, error) {
	updated, err := s.pipelines.Update(httpRequestContext(ctx), pipeline.UpdateCommand{
		PipelineID: request.DeliveryPipelineId, ExpectedRevision: request.Body.ExpectedRevision,
		EndpointKey: request.Body.EndpointKey, RepositoryURL: request.Body.RepositoryUrl,
		Branch: request.Body.Branch, DockerfilePath: stringValue(request.Body.DockerfilePath),
		ContextPath: stringValue(request.Body.ContextPath), Mode: string(request.Body.Mode),
		DeploymentTargetID: request.Body.DeploymentTargetId, ActorID: s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, pipeline.ErrInvalidInput), errors.Is(err, pipeline.ErrSourceNotFound):
			return api.UpdateDeliveryPipeline400JSONResponse{Code: "invalid_delivery_pipeline", Message: err.Error()}, nil
		case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, pipeline.ErrTargetNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.UpdateDeliveryPipeline404JSONResponse{Code: "delivery_pipeline_not_found", Message: "delivery pipeline or deployment target not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.UpdateDeliveryPipeline403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage delivery pipelines"}, nil
		case errors.Is(err, pipeline.ErrRevisionConflict), errors.Is(err, pipeline.ErrEnabledConflict), errors.Is(err, idempotency.ErrConflict):
			return api.UpdateDeliveryPipeline409JSONResponse{Code: "delivery_pipeline_conflict", Message: err.Error()}, nil
		default:
			return nil, err
		}
	}
	return api.UpdateDeliveryPipeline200JSONResponse(pipelineResponse(updated)), nil
}

func (s *Server) EnableDeliveryPipeline(ctx context.Context, request api.EnableDeliveryPipelineRequestObject) (api.EnableDeliveryPipelineResponseObject, error) {
	updated, err := s.pipelines.Enable(httpRequestContext(ctx), pipeline.StateCommand{PipelineID: request.DeliveryPipelineId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, pipeline.ErrInvalidInput), errors.Is(err, pipeline.ErrSourceNotFound), errors.Is(err, pipeline.ErrSourceOwnerChanged):
			return api.EnableDeliveryPipeline400JSONResponse{Code: "delivery_pipeline_source_invalid", Message: err.Error()}, nil
		case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.EnableDeliveryPipeline404JSONResponse{Code: "delivery_pipeline_not_found", Message: "delivery pipeline not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.EnableDeliveryPipeline403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage delivery pipelines"}, nil
		case errors.Is(err, pipeline.ErrRevisionConflict), errors.Is(err, pipeline.ErrEnabledConflict), errors.Is(err, idempotency.ErrConflict):
			return api.EnableDeliveryPipeline409JSONResponse{Code: "delivery_pipeline_conflict", Message: err.Error()}, nil
		default:
			return nil, err
		}
	}
	return api.EnableDeliveryPipeline200JSONResponse(pipelineResponse(updated)), nil
}

func (s *Server) DisableDeliveryPipeline(ctx context.Context, request api.DisableDeliveryPipelineRequestObject) (api.DisableDeliveryPipelineResponseObject, error) {
	updated, err := s.pipelines.Disable(httpRequestContext(ctx), pipeline.StateCommand{PipelineID: request.DeliveryPipelineId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, pipeline.ErrNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.DisableDeliveryPipeline404JSONResponse{Code: "delivery_pipeline_not_found", Message: "delivery pipeline not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.DisableDeliveryPipeline403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage delivery pipelines"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.DisableDeliveryPipeline409JSONResponse{Code: "idempotency_conflict", Message: err.Error()}, nil
		default:
			return nil, err
		}
	}
	return api.DisableDeliveryPipeline200JSONResponse(pipelineResponse(updated)), nil
}

func (s *Server) ListDeliveryRuns(ctx context.Context, request api.ListDeliveryRunsRequestObject) (api.ListDeliveryRunsResponseObject, error) {
	limit := 20
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.pipelines.ListRuns(httpRequestContext(ctx), pipeline.ListRunsQuery{PipelineID: request.DeliveryPipelineId, ActorID: s.localActorID, Limit: limit, Cursor: cursor})
	if err != nil {
		if errors.Is(err, pipeline.ErrInvalidCursor) {
			return api.ListDeliveryRuns400JSONResponse{Code: "invalid_delivery_run_cursor", Message: "delivery run cursor is invalid"}, nil
		}
		if errors.Is(err, pipeline.ErrNotFound) {
			return api.ListDeliveryRuns404JSONResponse{Code: "delivery_pipeline_not_found", Message: "delivery pipeline not found"}, nil
		}
		return nil, err
	}
	items := make([]api.DeliveryRunDetail, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, runResponse(item))
	}
	return api.ListDeliveryRuns200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) GetDeliveryRun(ctx context.Context, request api.GetDeliveryRunRequestObject) (api.GetDeliveryRunResponseObject, error) {
	detail, err := s.pipelines.GetRun(httpRequestContext(ctx), request.DeliveryRunId, s.localActorID)
	if err != nil {
		if errors.Is(err, pipeline.ErrRunNotFound) {
			return api.GetDeliveryRun404JSONResponse{Code: "delivery_run_not_found", Message: "delivery run not found"}, nil
		}
		return nil, err
	}
	return api.GetDeliveryRun200JSONResponse(runResponse(detail)), nil
}

func (s *Server) ReconcileDeliveryRun(ctx context.Context, request api.ReconcileDeliveryRunRequestObject) (api.ReconcileDeliveryRunResponseObject, error) {
	carrier := propagation.MapCarrier{}
	s.propagator.Inject(httpRequestContext(ctx), carrier)
	detail, err := s.pipelines.ReconcileRun(httpRequestContext(ctx), pipeline.ReconcileRunCommand{RunID: request.DeliveryRunId, ActorID: s.localActorID, IdempotencyKey: request.Params.IdempotencyKey, TraceParent: carrier.Get("traceparent"), TraceState: carrier.Get("tracestate")})
	if err != nil {
		switch {
		case errors.Is(err, pipeline.ErrRunNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ReconcileDeliveryRun404JSONResponse{Code: "delivery_run_not_found", Message: "delivery run not found"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ReconcileDeliveryRun403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot reconcile delivery runs"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.ReconcileDeliveryRun409JSONResponse{Code: "idempotency_conflict", Message: err.Error()}, nil
		default:
			return nil, err
		}
	}
	return api.ReconcileDeliveryRun202JSONResponse(runResponse(detail)), nil
}

func pipelineResponse(detail pipeline.Detail) api.DeliveryPipelineDetail {
	return api.DeliveryPipelineDetail{Pipeline: api.DeliveryPipeline{Id: detail.Pipeline.ID, ProjectId: detail.Pipeline.ProjectID, ApplicationId: detail.Pipeline.ApplicationID, Name: detail.Pipeline.Name, CurrentRevision: detail.Pipeline.CurrentRevision, Enabled: detail.Pipeline.Enabled, ActivationGeneration: detail.Pipeline.ActivationGeneration, CreatedBy: detail.Pipeline.CreatedBy, CreatedAt: detail.Pipeline.CreatedAt, UpdatedAt: detail.Pipeline.UpdatedAt}, Revision: api.DeliveryPipelineRevision{Revision: detail.Revision.Revision, Provider: api.DeliveryPipelineRevisionProvider(detail.Revision.Provider), EndpointKey: detail.Revision.EndpointKey, RepositoryId: detail.Revision.RepositoryID, RepositoryOwnerId: detail.Revision.RepositoryOwnerID, RepositoryFullName: detail.Revision.RepositoryFullName, RepositoryUrl: detail.Revision.RepositoryURL, GitRef: detail.Revision.GitRef, DockerfilePath: detail.Revision.DockerfilePath, ContextPath: detail.Revision.ContextPath, Platform: detail.Revision.Platform, Mode: api.DeliveryMode(detail.Revision.Mode), DeploymentTargetId: detail.Revision.DeploymentTargetID, CreatedBy: detail.Revision.CreatedBy, CreatedAt: detail.Revision.CreatedAt}}
}

func runResponse(detail pipeline.RunDetail) api.DeliveryRunDetail {
	result := api.DeliveryRunDetail{Run: api.DeliveryRun{Id: detail.Run.ID, DeliveryPipelineId: detail.Run.PipelineID, PipelineRevision: detail.Run.PipelineRevision, ActivationGeneration: detail.Run.ActivationGeneration, SourceCommit: detail.Run.SourceCommit, RepositoryUrl: detail.Run.RepositoryURL, Phase: api.DeliveryRunPhase(detail.Run.Phase), PhaseVersion: detail.Run.PhaseVersion, BuildId: detail.Run.BuildID, ImageArtifactId: detail.Run.ImageArtifactID, ReleaseId: detail.Run.ReleaseID, ReasonCode: detail.Run.ReasonCode, CreatedAt: detail.Run.CreatedAt, UpdatedAt: detail.Run.UpdatedAt, FinishedAt: detail.Run.FinishedAt}, Status: api.DeliveryRunDetailStatus(detail.Status), Trigger: api.DeliveryRunTrigger{EventType: detail.Trigger.EventType, RepositoryFullName: detail.Trigger.RepositoryFullName, GitRef: detail.Trigger.GitRef, Forced: detail.Trigger.Forced, ReceivedAt: detail.Trigger.ReceivedAt}}
	if detail.ActiveStage != nil {
		stage := api.DeliveryRunDetailActiveStage(*detail.ActiveStage)
		result.ActiveStage = &stage
	}
	return result
}
