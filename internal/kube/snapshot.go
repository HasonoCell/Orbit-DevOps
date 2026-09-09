package kube

import (
	"context"

	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/runtimeview"
	appsv1 "k8s.io/api/apps/v1"
)

type ObserveRequest = runtimeview.Query
type Snapshot = runtimeview.Snapshot
type Condition = runtimeview.Condition
type PodSummary = runtimeview.PodSummary

const (
	SourceKubernetes     = runtimeview.SourceKubernetes
	FreshnessFresh       = runtimeview.FreshnessFresh
	FreshnessUnavailable = runtimeview.FreshnessUnavailable
)

// Observe 保留旧 RuntimeSnapshot 契约，但复用 Release 诊断的 Kubernetes 读取与字段解释。
func (a *Adapter) Observe(ctx context.Context, request ObserveRequest) Snapshot {
	observation := a.ObserveRelease(ctx, diagnostics.RuntimeQuery{
		ClusterRef: request.ClusterRef,
		Namespace:  request.Namespace,
		TargetID:   request.TargetID,
	})
	workload := observation.Workload
	snapshot := Snapshot{
		Source:         workload.Metadata.Source,
		ObservedAt:     workload.Metadata.ObservedAt,
		Freshness:      FreshnessFresh,
		DeploymentName: ResourceName(request.TargetID),
		Conditions:     []Condition{},
		Pods:           []PodSummary{},
	}
	if workload.Metadata.Status != diagnostics.ObservationComplete {
		snapshot.Freshness = FreshnessUnavailable
		if len(workload.Metadata.ErrorCategories) > 0 {
			category := workload.Metadata.ErrorCategories[0]
			snapshot.ErrorCategory = &category
		}
	}
	if deployment := workload.Deployment; deployment != nil {
		snapshot.DeploymentExists = true
		snapshot.DeploymentName = deployment.Name
		snapshot.ReleaseID = deployment.ReleaseID
		snapshot.DesiredReplicas = deployment.DesiredReplicas
		snapshot.UpdatedReplicas = deployment.UpdatedReplicas
		snapshot.ReadyReplicas = deployment.ReadyReplicas
		snapshot.AvailableReplicas = deployment.AvailableReplicas
		for _, condition := range deployment.Conditions {
			snapshot.Conditions = append(snapshot.Conditions, Condition{
				Type: condition.Type, Status: condition.Status,
				Reason: condition.Reason, Message: condition.Message,
			})
		}
	}
	for _, pod := range workload.Pods {
		snapshot.Pods = append(snapshot.Pods, PodSummary{
			Name: pod.Name, Phase: pod.Phase, Ready: pod.Ready, Reason: pod.Reason,
		})
	}
	return snapshot
}

func desiredReplicas(deployment *appsv1.Deployment) int32 {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return *deployment.Spec.Replicas
}
