package kube

import (
	"context"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/runtimeview"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (a *Adapter) Observe(ctx context.Context, request ObserveRequest) Snapshot {
	observedAt := time.Now().UTC()
	name := ResourceName(request.TargetID)
	if request.ClusterRef != a.config.ClusterRef || request.Namespace != a.config.Namespace {
		return unavailableSnapshot(observedAt, name, "target_boundary_violation")
	}

	deployment, err := a.client.AppsV1().Deployments(request.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) {
		return Snapshot{
			Source:           SourceKubernetes,
			ObservedAt:       observedAt,
			Freshness:        FreshnessFresh,
			DeploymentName:   name,
			DeploymentExists: false,
			Conditions:       []Condition{},
			Pods:             []PodSummary{},
		}
	}
	if err != nil {
		a.recordReadFailure()
		return unavailableSnapshot(observedAt, name, "kubernetes_unavailable")
	}

	snapshot := deploymentSnapshot(deployment, observedAt)
	pods, err := a.client.CoreV1().Pods(request.Namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: TargetIDLabel + "=" + request.TargetID.String()},
	)
	if err != nil {
		a.recordReadFailure()
		snapshot.Freshness = FreshnessUnavailable
		category := "kubernetes_unavailable"
		snapshot.ErrorCategory = &category
		return snapshot
	}
	for _, pod := range pods.Items {
		snapshot.Pods = append(snapshot.Pods, summarizePod(pod))
	}
	return snapshot
}

func deploymentSnapshot(deployment *appsv1.Deployment, observedAt time.Time) Snapshot {
	snapshot := Snapshot{
		Source:            SourceKubernetes,
		ObservedAt:        observedAt,
		Freshness:         FreshnessFresh,
		DeploymentName:    deployment.Name,
		DeploymentExists:  true,
		DesiredReplicas:   desiredReplicas(deployment),
		UpdatedReplicas:   deployment.Status.UpdatedReplicas,
		ReadyReplicas:     deployment.Status.ReadyReplicas,
		AvailableReplicas: deployment.Status.AvailableReplicas,
		Conditions:        make([]Condition, 0, len(deployment.Status.Conditions)),
		Pods:              []PodSummary{},
	}
	if releaseID, err := uuid.Parse(deployment.Labels[ReleaseIDLabel]); err == nil {
		snapshot.ReleaseID = &releaseID
	}
	for _, condition := range deployment.Status.Conditions {
		snapshot.Conditions = append(snapshot.Conditions, Condition{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: boundedText(condition.Message, 256),
		})
	}
	return snapshot
}

func summarizePod(pod corev1.Pod) PodSummary {
	summary := PodSummary{
		Name:  pod.Name,
		Phase: string(pod.Status.Phase),
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			summary.Ready = true
		}
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.State.Waiting != nil {
			summary.Reason = container.State.Waiting.Reason
			break
		}
		if container.State.Terminated != nil {
			summary.Reason = container.State.Terminated.Reason
			break
		}
	}
	return summary
}

func desiredReplicas(deployment *appsv1.Deployment) int32 {
	if deployment.Spec.Replicas == nil {
		return 1
	}
	return *deployment.Spec.Replicas
}

func unavailableSnapshot(observedAt time.Time, name string, category string) Snapshot {
	return Snapshot{
		Source:           SourceKubernetes,
		ObservedAt:       observedAt,
		Freshness:        FreshnessUnavailable,
		DeploymentName:   name,
		DeploymentExists: false,
		Conditions:       []Condition{},
		Pods:             []PodSummary{},
		ErrorCategory:    &category,
	}
}

func boundedText(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
