package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestRecoveryInspectionClassifiesStableKubernetesState(t *testing.T) {
	request := publishRequest()
	name := kube.ResourceName(request.DeploymentTargetID)
	newAdapter := func(objects ...runtime.Object) *kube.Adapter {
		t.Helper()
		adapter, err := kube.New(fake.NewClientset(objects...), kube.Config{
			ClusterRef: "kind-orbit-devops-s1", Namespace: "orbit-devops-s1",
			FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
		})
		if err != nil {
			t.Fatalf("create adapter: %v", err)
		}
		return adapter
	}

	absent, err := newAdapter().InspectRecovery(context.Background(), request)
	if err != nil || absent.Action != releaseworker.RecoveryApply {
		t.Fatalf("absent recovery observation = %#v, error = %v", absent, err)
	}

	labels := recoveryLabels(request, request.ReleaseID)
	readyDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "orbit-devops-s1", Labels: labels, Generation: 1,
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
	readyService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "orbit-devops-s1", Labels: labels,
	}}
	ready, err := newAdapter(readyDeployment, readyService).InspectRecovery(
		context.Background(),
		request,
	)
	if err != nil || ready.Action != releaseworker.RecoverySucceeded {
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
	if err != nil || previous.Action != releaseworker.RecoveryReleaseObserved ||
		previous.ObservedReleaseID == nil || *previous.ObservedReleaseID != previousReleaseID {
		t.Fatalf("previous recovery observation = %#v, error = %v", previous, err)
	}

	foreign := readyDeployment.DeepCopy()
	foreign.Labels = map[string]string{"owner": "outside-orbit-devops"}
	conflict, err := newAdapter(foreign).InspectRecovery(context.Background(), request)
	if err != nil || conflict.Action != releaseworker.RecoveryAttention ||
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
			Namespace: "orbit-devops-s1",
			Labels:    map[string]string{"owner": "someone-else"},
		},
	}
	client := fake.NewClientset(foreignDeployment)
	adapter, err := kube.New(client, kube.Config{
		ClusterRef:   "kind-orbit-devops-s1",
		Namespace:    "orbit-devops-s1",
		FieldManager: "orbit-devops-delivery",
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	err = adapter.Publish(context.Background(), request)
	var failure *releaseworker.FailureError
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

	preserved, err := client.AppsV1().Deployments("orbit-devops-s1").Get(
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

func TestDiagnosticObservationReportsKubernetesReadFailure(t *testing.T) {
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
		ClusterRef:          "kind-orbit-devops-s1",
		Namespace:           "orbit-devops-s1",
		FieldManager:        "orbit-devops-delivery",
		ReadFailureRecorder: metrics,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}
	targetID := uuid.New()

	observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
		ClusterRef: "kind-orbit-devops-s1",
		Namespace:  "orbit-devops-s1",
		TargetID:   targetID,
	})
	if observation.Workload.Metadata.Source != diagnostics.SourceKubernetes {
		t.Errorf("source = %q, want %q", observation.Workload.Metadata.Source, diagnostics.SourceKubernetes)
	}
	if observation.Workload.Metadata.Status != diagnostics.ObservationPartial {
		t.Errorf("status = %q, want %q", observation.Workload.Metadata.Status, diagnostics.ObservationPartial)
	}
	if observation.Workload.Metadata.ObservedAt.IsZero() {
		t.Error("observedAt is zero")
	}
	if len(observation.Workload.Metadata.ErrorCategories) != 1 ||
		observation.Workload.Metadata.ErrorCategories[0] != "kubernetes_unavailable" {
		t.Errorf("error categories = %v, want kubernetes_unavailable", observation.Workload.Metadata.ErrorCategories)
	}
	metricResponse := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricPayload, err := io.ReadAll(metricResponse.Result().Body)
	if err != nil {
		t.Fatalf("read Kubernetes metrics: %v", err)
	}
	if !strings.Contains(string(metricPayload), "orbit_devops_kubernetes_read_failures_total 1") {
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
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
		TargetID:  request.DeploymentTargetID,
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
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID:   request.DeploymentTargetID,
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
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID:   request.DeploymentTargetID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
	})
	if len(observation.Workload.Pods) != 20 {
		t.Fatalf("pod count = %d, want 20", len(observation.Workload.Pods))
	}
	if observation.Workload.Pods[0].Name != "abnormal-oldest" {
		t.Fatalf("first pod = %q, want abnormal-oldest", observation.Workload.Pods[0].Name)
	}
}

func TestRuntimeLogsRefusesPodFromDifferentRelease(t *testing.T) {
	request := publishRequest()
	labels := recoveryLabels(request, uuid.New())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "application-pod", Namespace: request.Namespace, Labels: labels,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "application"}}},
	}
	adapter, err := kube.New(fake.NewClientset(pod), kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	_, err = adapter.ReadReleaseLogs(context.Background(), diagnostics.ReleaseRuntimeLogQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		PodName: "application-pod", Container: "application", TailLines: 200,
	})
	if !errors.Is(err, diagnostics.ErrRuntimeLogSourceNotFound) {
		t.Fatalf("cross-release log error = %v, want ErrRuntimeLogSourceNotFound", err)
	}
}

func TestRuntimeLogsReadsTheVerifiedContainerWithBoundedOptions(t *testing.T) {
	request := publishRequest()
	labels := recoveryLabels(request, request.ReleaseID)
	var observedTailLines string
	var observedPrevious string
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "application-pod", Namespace: request.Namespace,
			UID: "application-pod-uid", Labels: labels,
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "application"}}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, incoming *http.Request) {
		switch incoming.URL.Path {
		case "/api/v1/namespaces/orbit-devops-s1/pods/application-pod":
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&pod); err != nil {
				t.Errorf("encode Pod response: %v", err)
			}
		case "/api/v1/namespaces/orbit-devops-s1/pods":
			response.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(response).Encode(&corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
				Items:    []corev1.Pod{pod},
			}); err != nil {
				t.Errorf("encode Pod list response: %v", err)
			}
		case "/api/v1/namespaces/orbit-devops-s1/pods/application-pod/log":
			observedTailLines = incoming.URL.Query().Get("tailLines")
			observedPrevious = incoming.URL.Query().Get("previous")
			_, _ = io.WriteString(response, "line one\nline two\n")
		default:
			http.NotFound(response, incoming)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("create Kubernetes HTTP client: %v", err)
	}
	adapter, err := kube.New(client, kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	result, err := adapter.ReadReleaseLogs(context.Background(), diagnostics.ReleaseRuntimeLogQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		PodName: "application-pod", Container: "application", TailLines: 37, Previous: true,
	})
	if err != nil {
		t.Fatalf("read runtime logs: %v", err)
	}
	if result.Content != "line one\nline two\n" || result.Truncated || result.ObservedAt.IsZero() {
		t.Fatalf("runtime log result = %#v", result)
	}
	if observedTailLines != "37" || observedPrevious != "true" {
		t.Fatalf("log query tailLines=%q previous=%q", observedTailLines, observedPrevious)
	}
}

func TestRuntimeLogsRefusesOwnedPodOutsideTheDiagnosticProjection(t *testing.T) {
	request := publishRequest()
	labels := recoveryLabels(request, request.ReleaseID)
	objects := make([]runtime.Object, 0, 21)
	for index := 0; index < 21; index++ {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("ready-%02d", index), Namespace: request.Namespace,
				UID: types.UID(fmt.Sprintf("ready-uid-%02d", index)), Labels: labels,
				CreationTimestamp: metav1.NewTime(time.Date(2026, 9, 9, 17, index, 0, 0, time.UTC)),
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "application"}}},
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
	adapter, err := kube.New(fake.NewClientset(objects...), kube.Config{
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		FieldManager: "orbit-devops-delivery", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create adapter: %v", err)
	}

	_, err = adapter.ReadReleaseLogs(context.Background(), diagnostics.ReleaseRuntimeLogQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: request.ClusterRef, Namespace: request.Namespace,
		PodName: "ready-00", Container: "application", TailLines: 200,
	})
	if !errors.Is(err, diagnostics.ErrRuntimeLogSourceNotFound) {
		t.Fatalf("hidden Pod log error = %v, want ErrRuntimeLogSourceNotFound", err)
	}
}

func publishRequest() releaseworker.PublishRequest {
	return releaseworker.PublishRequest{
		ReleaseOperationID: uuid.New(),
		ReleaseAttemptID:   uuid.New(),
		ReleaseID:          uuid.New(),
		ProjectID:          uuid.New(),
		ApplicationID:      uuid.New(),
		DeploymentTargetID: uuid.New(),
		ImageReference:     "registry.example/orbit-devops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Stage:              "development",
		ClusterRef:         "kind-orbit-devops-s1",
		Namespace:          "orbit-devops-s1",
		Replicas:           1,
		ContainerPort:      8080,
	}
}

func recoveryLabels(request releaseworker.PublishRequest, releaseID uuid.UUID) map[string]string {
	return map[string]string{
		kube.ManagedByLabel:     kube.ManagedByValue,
		kube.ProjectIDLabel:     request.ProjectID.String(),
		kube.ApplicationIDLabel: request.ApplicationID.String(),
		kube.TargetIDLabel:      request.DeploymentTargetID.String(),
		kube.ReleaseIDLabel:     releaseID.String(),
	}
}
