package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/catalog"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/runtimeview"
)

func (s *Server) GetRuntimeSnapshot(
	ctx context.Context,
	request api.GetRuntimeSnapshotRequestObject,
) (api.GetRuntimeSnapshotResponseObject, error) {
	requestContext := httpRequestContext(ctx)
	target, err := s.catalog.GetDeploymentTarget(requestContext, request.DeploymentTargetId)
	if err != nil {
		if errors.Is(err, catalog.ErrDeploymentTargetNotFound) {
			return api.GetRuntimeSnapshot404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
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
			return api.GetRuntimeSnapshot404JSONResponse{
				Code:    "deployment_target_not_found",
				Message: "deployment target not found",
			}, nil
		}
		return nil, err
	}

	snapshot := s.observer.Observe(requestContext, runtimeview.Query{
		ClusterRef: target.ClusterRef,
		Namespace:  target.Namespace,
		TargetID:   target.ID,
	})
	conditions := make([]api.RuntimeCondition, 0, len(snapshot.Conditions))
	for _, condition := range snapshot.Conditions {
		conditions = append(conditions, api.RuntimeCondition{
			Type:    condition.Type,
			Status:  condition.Status,
			Reason:  condition.Reason,
			Message: condition.Message,
		})
	}
	pods := make([]api.RuntimePod, 0, len(snapshot.Pods))
	for _, pod := range snapshot.Pods {
		pods = append(pods, api.RuntimePod{
			Name:   pod.Name,
			Phase:  pod.Phase,
			Ready:  pod.Ready,
			Reason: pod.Reason,
		})
	}

	return api.GetRuntimeSnapshot200JSONResponse{
		DeploymentTargetId: target.ID,
		Source:             api.RuntimeSnapshotSource(snapshot.Source),
		ObservedAt:         snapshot.ObservedAt,
		Freshness:          api.RuntimeSnapshotFreshness(snapshot.Freshness),
		DeploymentName:     snapshot.DeploymentName,
		DeploymentExists:   snapshot.DeploymentExists,
		ReleaseId:          snapshot.ReleaseID,
		DesiredReplicas:    int(snapshot.DesiredReplicas),
		UpdatedReplicas:    int(snapshot.UpdatedReplicas),
		ReadyReplicas:      int(snapshot.ReadyReplicas),
		AvailableReplicas:  int(snapshot.AvailableReplicas),
		Conditions:         conditions,
		Pods:               pods,
		ErrorCategory:      snapshot.ErrorCategory,
	}, nil
}
