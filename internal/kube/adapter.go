package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	ManagedByLabel     = "app.kubernetes.io/managed-by"
	ProjectIDLabel     = "orbit-devops.dev/project-id"
	ApplicationIDLabel = "orbit-devops.dev/application-id"
	TargetIDLabel      = "orbit-devops.dev/target-id"
	ReleaseIDLabel     = "orbit-devops.dev/release-id"
	ManagedByValue     = "orbit-devops"
)

type Config struct {
	ClusterRef          string
	Namespace           string
	FieldManager        string
	PollInterval        time.Duration
	ReadFailureRecorder ReadFailureRecorder
}

type ReadFailureRecorder interface {
	RecordKubernetesReadFailure()
}

type Adapter struct {
	client kubernetes.Interface
	config Config
}

func New(client kubernetes.Interface, config Config) (*Adapter, error) {
	if client == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	if config.ClusterRef == "" {
		return nil, errors.New("cluster reference is required")
	}
	if config.Namespace == "" {
		return nil, errors.New("namespace is required")
	}
	if config.FieldManager == "" {
		return nil, errors.New("field manager is required")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 500 * time.Millisecond
	}
	return &Adapter{client: client, config: config}, nil
}

func (a *Adapter) Publish(ctx context.Context, request releaseworker.PublishRequest) error {
	if request.ClusterRef != a.config.ClusterRef || request.Namespace != a.config.Namespace {
		return releaseworker.NewFailure(
			"target_boundary_violation",
			"release target does not match the configured local Kubernetes boundary",
		)
	}

	name := ResourceName(request.DeploymentTargetID)
	ownerLabels := ownershipLabels(request)
	if err := a.checkOwnership(ctx, name, ownerLabels); err != nil {
		return err
	}

	selector := map[string]string{TargetIDLabel: request.DeploymentTargetID.String()}
	podLabels := copyLabels(ownerLabels)
	container := corev1ac.Container().
		WithName("application").
		WithImage(request.ImageReference).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithPorts(corev1ac.ContainerPort().
			WithName("http").
			WithContainerPort(int32(request.ContainerPort)).
			WithProtocol(corev1.ProtocolTCP),
		)
	podTemplate := corev1ac.PodTemplateSpec().
		WithLabels(podLabels).
		WithSpec(corev1ac.PodSpec().WithContainers(container))
	deployment := appsv1ac.Deployment(name, request.Namespace).
		WithLabels(ownerLabels).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(int32(request.Replicas)).
			WithProgressDeadlineSeconds(60).
			WithSelector(metav1ac.LabelSelector().WithMatchLabels(selector)).
			WithTemplate(podTemplate),
		)
	if _, err := a.client.AppsV1().Deployments(request.Namespace).Apply(
		ctx,
		deployment,
		metav1.ApplyOptions{FieldManager: a.config.FieldManager},
	); err != nil {
		return applyFailure("Deployment", err)
	}

	servicePort := corev1ac.ServicePort().
		WithName("http").
		WithProtocol(corev1.ProtocolTCP).
		WithPort(int32(request.ContainerPort)).
		WithTargetPort(intstr.FromString("http"))
	service := corev1ac.Service(name, request.Namespace).
		WithLabels(ownerLabels).
		WithSpec(corev1ac.ServiceSpec().
			WithType(corev1.ServiceTypeClusterIP).
			WithSelector(selector).
			WithPorts(servicePort),
		)
	if _, err := a.client.CoreV1().Services(request.Namespace).Apply(
		ctx,
		service,
		metav1.ApplyOptions{FieldManager: a.config.FieldManager},
	); err != nil {
		return applyFailure("Service", err)
	}

	return a.waitForRollout(ctx, request.Namespace, name, request.DeploymentTargetID, request.Replicas)
}

// InspectRecovery 只读取稳定资源与归属标签，在恢复 Attempt 的任何写入之前给出决策事实。
func (a *Adapter) InspectRecovery(
	ctx context.Context,
	request releaseworker.PublishRequest,
) (releaseworker.RecoveryObservation, error) {
	if request.ClusterRef != a.config.ClusterRef || request.Namespace != a.config.Namespace {
		return releaseworker.RecoveryObservation{
			Action:       releaseworker.RecoveryAttention,
			ErrorCode:    "target_boundary_violation",
			ErrorSummary: "release target does not match the configured local Kubernetes boundary",
		}, nil
	}
	name := ResourceName(request.DeploymentTargetID)
	deployment, err := a.client.AppsV1().Deployments(request.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return releaseworker.RecoveryObservation{}, a.unknownReadFailure("inspect recovery Deployment", err)
	}
	if apierrors.IsNotFound(err) {
		deployment = nil
	}
	service, err := a.client.CoreV1().Services(request.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err != nil && !apierrors.IsNotFound(err) {
		return releaseworker.RecoveryObservation{}, a.unknownReadFailure("inspect recovery Service", err)
	}
	if apierrors.IsNotFound(err) {
		service = nil
	}
	if deployment == nil && service == nil {
		return releaseworker.RecoveryObservation{Action: releaseworker.RecoveryApply}, nil
	}

	wantOwnership := ownershipLabels(request)
	for kind, labels := range map[string]map[string]string{
		"Deployment": labelsOfDeployment(deployment),
		"Service":    labelsOfService(service),
	} {
		if labels != nil && !hasOwnership(labels, wantOwnership) {
			return releaseworker.RecoveryObservation{
				Action:       releaseworker.RecoveryAttention,
				ErrorCode:    "ownership_conflict",
				ErrorSummary: fmt.Sprintf("%s %q exists without matching Orbit-DevOps ownership", kind, name),
			}, nil
		}
	}

	releaseLabels := make([]string, 0, 2)
	if deployment != nil {
		releaseLabels = append(releaseLabels, deployment.Labels[ReleaseIDLabel])
	}
	if service != nil {
		releaseLabels = append(releaseLabels, service.Labels[ReleaseIDLabel])
	}
	if releaseLabels[0] == "" ||
		(len(releaseLabels) == 2 && releaseLabels[0] != releaseLabels[1]) {
		return releaseworker.RecoveryObservation{
			Action:       releaseworker.RecoveryAttention,
			ErrorCode:    "release_identity_conflict",
			ErrorSummary: "Kubernetes resources do not expose one consistent Orbit-DevOps release identifier",
		}, nil
	}
	observedReleaseID, err := uuid.Parse(releaseLabels[0])
	if err != nil {
		return releaseworker.RecoveryObservation{
			Action:       releaseworker.RecoveryAttention,
			ErrorCode:    "release_identity_invalid",
			ErrorSummary: "Kubernetes resources expose an invalid Orbit-DevOps release identifier",
		}, nil
	}
	if observedReleaseID != request.ReleaseID {
		return releaseworker.RecoveryObservation{
			Action:            releaseworker.RecoveryReleaseObserved,
			ObservedReleaseID: &observedReleaseID,
		}, nil
	}
	if deployment == nil || service == nil {
		return releaseworker.RecoveryObservation{Action: releaseworker.RecoveryApply}, nil
	}
	if rolloutReady(deployment, int32(request.Replicas)) {
		return releaseworker.RecoveryObservation{Action: releaseworker.RecoverySucceeded}, nil
	}
	return releaseworker.RecoveryObservation{Action: releaseworker.RecoveryObserve}, nil
}

// ObserveRecovery 延续已属于目标 Release 的 Rollout 观察，不再次执行 Apply。
func (a *Adapter) ObserveRecovery(ctx context.Context, request releaseworker.PublishRequest) error {
	return a.waitForRollout(
		ctx,
		request.Namespace,
		ResourceName(request.DeploymentTargetID),
		request.DeploymentTargetID,
		request.Replicas,
	)
}

func (a *Adapter) checkOwnership(
	ctx context.Context,
	name string,
	wantLabels map[string]string,
) error {
	deployment, err := a.client.AppsV1().Deployments(a.config.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err == nil && !hasOwnership(deployment.Labels, wantLabels) {
		return ownershipFailure("Deployment", name)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return a.preflightFailure("inspect Deployment ownership", err)
	}

	service, err := a.client.CoreV1().Services(a.config.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err == nil && !hasOwnership(service.Labels, wantLabels) {
		return ownershipFailure("Service", name)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return a.preflightFailure("inspect Service ownership", err)
	}
	return nil
}

func (a *Adapter) waitForRollout(
	ctx context.Context,
	namespace string,
	name string,
	targetID uuid.UUID,
	replicas int,
) error {
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()

	for {
		deployment, err := a.client.AppsV1().Deployments(namespace).Get(
			ctx,
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			return a.observeFailure("observe Deployment rollout", err)
		}
		if rolloutReady(deployment, int32(replicas)) {
			return nil
		}
		if failure := deploymentFailure(deployment); failure != nil {
			return failure
		}

		pods, err := a.client.CoreV1().Pods(namespace).List(
			ctx,
			metav1.ListOptions{LabelSelector: TargetIDLabel + "=" + targetID.String()},
		)
		if err != nil {
			return a.observeFailure("observe rollout Pods", err)
		}
		if failure := podFailure(pods.Items); failure != nil {
			return failure
		}

		select {
		case <-ctx.Done():
			return releaseworker.NewUnknownOutcome(
				"rollout_observation_interrupted",
				"delivery stopped before the Kubernetes rollout outcome was observed",
				true,
			)
		case <-ticker.C:
		}
	}
}

func ResourceName(targetID uuid.UUID) string {
	return "orbit-devops-" + strings.ReplaceAll(targetID.String(), "-", "")
}

func ownershipLabels(request releaseworker.PublishRequest) map[string]string {
	return map[string]string{
		ManagedByLabel:     ManagedByValue,
		ProjectIDLabel:     request.ProjectID.String(),
		ApplicationIDLabel: request.ApplicationID.String(),
		TargetIDLabel:      request.DeploymentTargetID.String(),
		ReleaseIDLabel:     request.ReleaseID.String(),
	}
}

func hasOwnership(existing map[string]string, want map[string]string) bool {
	for _, key := range []string{ManagedByLabel, ProjectIDLabel, ApplicationIDLabel, TargetIDLabel} {
		if existing[key] != want[key] {
			return false
		}
	}
	return true
}

func copyLabels(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func rolloutReady(deployment *appsv1.Deployment, replicas int32) bool {
	return deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.UpdatedReplicas == replicas &&
		deployment.Status.AvailableReplicas == replicas &&
		deployment.Status.UnavailableReplicas == 0
}

func deploymentFailure(deployment *appsv1.Deployment) error {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing &&
			condition.Status == corev1.ConditionFalse &&
			condition.Reason == "ProgressDeadlineExceeded" {
			return releaseworker.NewFailure(
				"rollout_progress_deadline",
				"Deployment exceeded its Kubernetes progress deadline",
			)
		}
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return releaseworker.NewFailure(
				"replica_creation_failed",
				"Kubernetes could not create the desired workload replicas",
			)
		}
	}
	return nil
}

func podFailure(pods []corev1.Pod) error {
	for _, pod := range pods {
		for _, status := range pod.Status.ContainerStatuses {
			waiting := status.State.Waiting
			if waiting == nil {
				continue
			}
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return releaseworker.NewFailure(
					"image_pull_failed",
					"Kubernetes could not pull the immutable release image",
				)
			case "CreateContainerConfigError", "CreateContainerError":
				return releaseworker.NewFailure(
					"container_start_failed",
					"Kubernetes could not create the release container",
				)
			}
		}
	}
	return nil
}

func ownershipFailure(kind string, name string) error {
	return releaseworker.NewFailure(
		"ownership_conflict",
		fmt.Sprintf("%s %q exists without matching Orbit-DevOps ownership", kind, name),
	)
}

func applyFailure(kind string, err error) error {
	if apierrors.IsConflict(err) {
		return releaseworker.NewFailure(
			"apply_conflict",
			fmt.Sprintf("Server-Side Apply reported a field ownership conflict for %s", kind),
		)
	}
	return releaseworker.NewUnknownOutcome(
		"kubernetes_apply_failed",
		fmt.Sprintf("Kubernetes rejected the desired %s: %s", kind, apierrors.ReasonForError(err)),
		true,
	)
}

func (a *Adapter) observeFailure(action string, err error) error {
	a.recordReadFailure()
	return releaseworker.NewUnknownOutcome(
		"kubernetes_unavailable",
		fmt.Sprintf("%s failed: %s", action, apierrors.ReasonForError(err)),
		true,
	)
}

func (a *Adapter) preflightFailure(action string, err error) error {
	a.recordReadFailure()
	return releaseworker.NewRetryableFailure(
		"kubernetes_unavailable",
		fmt.Sprintf("%s failed: %s", action, apierrors.ReasonForError(err)),
	)
}

func (a *Adapter) unknownReadFailure(action string, err error) error {
	return a.observeFailure(action, err)
}

func labelsOfDeployment(deployment *appsv1.Deployment) map[string]string {
	if deployment == nil {
		return nil
	}
	return deployment.Labels
}

func labelsOfService(service *corev1.Service) map[string]string {
	if service == nil {
		return nil
	}
	return service.Labels
}

func (a *Adapter) recordReadFailure() {
	if a.config.ReadFailureRecorder != nil {
		a.config.ReadFailureRecorder.RecordKubernetesReadFailure()
	}
}
