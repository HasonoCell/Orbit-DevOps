package kube_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

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
	if failure.Category() != "ownership_conflict" {
		t.Errorf("failure category = %q, want ownership_conflict", failure.Category())
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
