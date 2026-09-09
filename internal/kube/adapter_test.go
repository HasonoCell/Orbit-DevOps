package kube_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestRecoveryInspectionClassifiesStableKubernetesState(t *testing.T) {
	request := publishRequest()
	name := kube.ResourceName(request.DeploymentTargetID)
	newAdapter := func(objects ...runtime.Object) *kube.Adapter {
		t.Helper()
		adapter, err := kube.New(fake.NewClientset(objects...), kube.Config{
			ClusterRef: "kind-orbitops-s1", Namespace: "orbitops-s1",
			FieldManager: "orbitops-delivery", PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("create adapter: %v", err)
		}
		return adapter
	}

	absent, err := newAdapter().InspectRecovery(context.Background(), request)
	if err != nil || absent.Action != worker.RecoveryApply {
		t.Fatalf("absent recovery observation = %#v, error = %v", absent, err)
	}

	labels := recoveryLabels(request, request.ReleaseID)
	readyDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "orbitops-s1", Labels: labels, Generation: 1,
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
	readyService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "orbitops-s1", Labels: labels,
	}}
	ready, err := newAdapter(readyDeployment, readyService).InspectRecovery(
		context.Background(),
		request,
	)
	if err != nil || ready.Action != worker.RecoverySucceeded {
		t.Fatalf("ready recovery observation = %#v, error = %v", ready, err)
	}

	previousReleaseID := uuid.New()
	previousLabels := recoveryLabels(request, previousReleaseID)
	previousDeployment := readyDeployment.DeepCopy()
	previousDeployment.Labels = previousLabels
	previousService := readyService.DeepCopy()
	previousService.Labels = previousLabels
	previous, err := newAdapter(previousDeployment, previousService).InspectRecovery(
		context.Background(),
		request,
	)
	if err != nil || previous.Action != worker.RecoveryReleaseObserved ||
		previous.ObservedReleaseID == nil || *previous.ObservedReleaseID != previousReleaseID {
		t.Fatalf("previous recovery observation = %#v, error = %v", previous, err)
	}

	foreign := readyDeployment.DeepCopy()
	foreign.Labels = map[string]string{"owner": "outside-orbitops"}
	conflict, err := newAdapter(foreign).InspectRecovery(context.Background(), request)
	if err != nil || conflict.Action != worker.RecoveryAttention ||
		conflict.ErrorCode != "ownership_conflict" {
		t.Fatalf("conflict recovery observation = %#v, error = %v", conflict, err)
	}
}

func TestPublisherRefusesForeignResourceBeforeApply(t *testing.T) {
	request := publishRequest()
	name := kube.ResourceName(request.DeploymentTargetID)
	foreignDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "orbitops-s1",
			Labels:    map[string]string{"owner": "someone-else"},
		},
	}
	client := fake.NewClientset(foreignDeployment)
	adapter, err := kube.New(client, kube.Config{
		ClusterRef:   "kind-orbitops-s1",
		Namespace:    "orbitops-s1",
		FieldManager: "orbitops-delivery",
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	err = adapter.Publish(context.Background(), request)
	var failure *worker.FailureError
	if !errors.As(err, &failure) {
		t.Fatalf("publish error = %v, want structured failure", err)
	}
	if failure.Code() != "ownership_conflict" {
		t.Errorf("failure code = %q, want ownership_conflict", failure.Code())
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("unexpected apply action after ownership conflict: %v", action)
		}
	}

	preserved, err := client.AppsV1().Deployments("orbitops-s1").Get(
		context.Background(),
		name,
		metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get preserved deployment: %v", err)
	}
	if preserved.Labels["owner"] != "someone-else" {
		t.Errorf("foreign deployment labels changed: %#v", preserved.Labels)
	}
}

func TestRuntimeSnapshotReportsKubernetesUnavailable(t *testing.T) {
	metrics := observability.NewMetrics(nil)
	client := fake.NewClientset()
	client.PrependReactor(
		"get",
		"deployments",
		func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("API Server unavailable")
		},
	)
	adapter, err := kube.New(client, kube.Config{
		ClusterRef:          "kind-orbitops-s1",
		Namespace:           "orbitops-s1",
		FieldManager:        "orbitops-delivery",
		ReadFailureRecorder: metrics,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	targetID := uuid.New()

	snapshot := adapter.Observe(context.Background(), kube.ObserveRequest{
		ClusterRef: "kind-orbitops-s1",
		Namespace:  "orbitops-s1",
		TargetID:   targetID,
	})
	if snapshot.Source != kube.SourceKubernetes {
		t.Errorf("source = %q, want %q", snapshot.Source, kube.SourceKubernetes)
	}
	if snapshot.Freshness != kube.FreshnessUnavailable {
		t.Errorf("freshness = %q, want %q", snapshot.Freshness, kube.FreshnessUnavailable)
	}
	if snapshot.ObservedAt.IsZero() {
		t.Error("observedAt is zero")
	}
	if snapshot.ErrorCategory == nil || *snapshot.ErrorCategory != "kubernetes_unavailable" {
		t.Errorf("error category = %v, want kubernetes_unavailable", snapshot.ErrorCategory)
	}
	metricResponse := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricPayload, err := io.ReadAll(metricResponse.Result().Body)
	if err != nil {
		t.Fatalf("read Kubernetes metrics: %v", err)
	}
	if !strings.Contains(string(metricPayload), "orbitops_kubernetes_read_failures_total 1") {
		t.Error("Kubernetes read failure metric was not incremented")
	}
}

func TestDiagnosticObservationProjectsWorkloadAndRelatedEvents(t *testing.T) {
	request := publishRequest()
	name := kube.ResourceName(request.DeploymentTargetID)
	labels := recoveryLabels(request, request.ReleaseID)
	deploymentUID := "deployment-uid"
	podUID := "pod-uid"
	replicas := int32(2)
	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: request.Namespace, UID: types.UID(deploymentUID),
				Labels: labels, Generation: 3,
			},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
				Conditions: []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue,
					Reason: "ReplicaSetUpdated", Message: strings.Repeat("进", 600),
				}},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: request.Namespace, UID: "service-uid", Labels: labels,
			},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{
				Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080,
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "application-bad", Namespace: request.Namespace, UID: types.UID(podUID),
				Labels: labels, CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)),
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "application", RestartCount: 2,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "ImagePullBackOff", Message: "cannot pull image",
					}},
				}},
			},
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "pull-failed", Namespace: request.Namespace, UID: "event-uid"},
			InvolvedObject: corev1.ObjectReference{
				Kind: "Pod", Name: "application-bad", UID: types.UID(podUID),
			},
			Type: corev1.EventTypeWarning, Reason: "Failed", Message: "image pull failed",
			Count: 3, FirstTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)),
			LastTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 12, 1, 0, 0, time.UTC)),
		},
	}
	client := fake.NewClientset(objects...)
	adapter, err := kube.New(client, kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbitops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveRelease(context.Background(), diagnostics.RuntimeQuery{
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
	})
	if observation.Workload.Metadata.Status != diagnostics.ObservationComplete {
		t.Fatalf("workload status = %q, want complete: %#v", observation.Workload.Metadata.Status, observation.Workload)
	}
	if observation.Workload.Deployment == nil ||
		observation.Workload.Deployment.ObservedGeneration != 2 ||
		len(observation.Workload.Deployment.Conditions) != 1 ||
		len([]rune(observation.Workload.Deployment.Conditions[0].Message)) != 512 {
		t.Fatalf("deployment evidence = %#v", observation.Workload.Deployment)
	}
	if len(observation.Workload.Pods) != 1 ||
		len(observation.Workload.Pods[0].Containers) != 1 ||
		observation.Workload.Pods[0].Containers[0].Reason != "ImagePullBackOff" {
		t.Fatalf("pod evidence = %#v", observation.Workload.Pods)
	}
	if observation.Events.Metadata.Status != diagnostics.ObservationComplete ||
		len(observation.Events.Items) != 1 ||
		observation.Events.Items[0].UID != "event-uid" {
		t.Fatalf("event observation = %#v", observation.Events)
	}
}

func TestDiagnosticObservationKeepsWorkloadWhenEventReadFails(t *testing.T) {
	request := publishRequest()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: kube.ResourceName(request.DeploymentTargetID), Namespace: request.Namespace,
		UID: "deployment-uid", Labels: recoveryLabels(request, request.ReleaseID),
	}}
	client := fake.NewClientset(deployment)
	client.PrependReactor("list", "events", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("events temporarily unavailable")
	})
	adapter, err := kube.New(client, kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbitops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveRelease(context.Background(), diagnostics.RuntimeQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
	})
	if observation.Workload.Metadata.Status != diagnostics.ObservationComplete ||
		observation.Workload.Deployment == nil {
		t.Fatalf("workload observation = %#v, want complete Deployment evidence", observation.Workload)
	}
	if observation.Events.Metadata.Status != diagnostics.ObservationUnavailable ||
		len(observation.Events.Metadata.ErrorCategories) != 1 ||
		observation.Events.Metadata.ErrorCategories[0] != "kubernetes_unavailable" {
		t.Fatalf("event observation = %#v, want unavailable", observation.Events)
	}
}

func TestDiagnosticObservationPrioritizesAbnormalPodsBeforeLimiting(t *testing.T) {
	request := publishRequest()
	labels := recoveryLabels(request, request.ReleaseID)
	objects := make([]runtime.Object, 0, 22)
	objects = append(objects, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: kube.ResourceName(request.DeploymentTargetID), Namespace: request.Namespace,
		UID: "deployment-uid", Labels: labels,
	}})
	for index := 0; index < 20; index++ {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("ready-%02d", index), Namespace: request.Namespace,
				UID: types.UID(fmt.Sprintf("ready-uid-%02d", index)), Labels: labels,
				CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 13, index, 0, 0, time.UTC)),
			},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "application", Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		})
	}
	objects = append(objects, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "abnormal-oldest", Namespace: request.Namespace,
			UID: "abnormal-uid", Labels: labels,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 11, 0, 0, 0, time.UTC)),
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	})
	client := fake.NewClientset(objects...)
	adapter, err := kube.New(client, kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbitops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveRelease(context.Background(), diagnostics.RuntimeQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
	})
	if len(observation.Workload.Pods) != 20 {
		t.Fatalf("pod count = %d, want 20", len(observation.Workload.Pods))
	}
	if observation.Workload.Pods[0].Name != "abnormal-oldest" {
		t.Fatalf("first pod = %q, want abnormal-oldest", observation.Workload.Pods[0].Name)
	}
}

func publishRequest() worker.PublishRequest {
	return worker.PublishRequest{
		OperationID:        uuid.New(),
		AttemptID:          uuid.New(),
		ReleaseID:          uuid.New(),
		ProjectID:          uuid.New(),
		ApplicationID:      uuid.New(),
		DeploymentTargetID: uuid.New(),
		ImageReference:     "registry.example/orbitops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Stage:              "development",
		ClusterRef:         "kind-orbitops-s1",
		Namespace:          "orbitops-s1",
		Replicas:           1,
		ContainerPort:      8080,
	}
}

func recoveryLabels(request worker.PublishRequest, releaseID uuid.UUID) map[string]string {
	return map[string]string{
		kube.ManagedByLabel:     kube.ManagedByValue,
		kube.ProjectIDLabel:     request.ProjectID.String(),
		kube.ApplicationIDLabel: request.ApplicationID.String(),
		kube.TargetIDLabel:      request.DeploymentTargetID.String(),
		kube.ReleaseIDLabel:     releaseID.String(),
	}
}
