package kind_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kindContext   = "kind-orbitops-s1"
	kindCluster   = "kind-orbitops-s1"
	kindNamespace = "orbitops-s1"
	readyImage    = "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a"
)

func TestKindDeliveryScenarios(t *testing.T) {
	adapter, client := newKindAdapter(t)

	t.Run("ready deployment and runtime snapshot", func(t *testing.T) {
		request := kindPublishRequest(readyImage)
		cleanupResources(t, client, request.DeploymentTargetID)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := adapter.Publish(ctx, request); err != nil {
			t.Fatalf("publish ready release: %v", err)
		}

		name := kube.ResourceName(request.DeploymentTargetID)
		deployment, err := client.AppsV1().Deployments(kindNamespace).Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			t.Fatalf("get applied deployment: %v", err)
		}
		if deployment.Labels[kube.ReleaseIDLabel] != request.ReleaseID.String() {
			t.Errorf("deployment release label = %q, want %q", deployment.Labels[kube.ReleaseIDLabel], request.ReleaseID)
		}
		if len(deployment.Spec.Template.Spec.Containers) != 1 ||
			deployment.Spec.Template.Spec.Containers[0].Image != request.ImageReference {
			t.Errorf("deployment containers = %#v, want immutable release image", deployment.Spec.Template.Spec.Containers)
		}
		service, err := client.CoreV1().Services(kindNamespace).Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			t.Fatalf("get applied service: %v", err)
		}
		if service.Spec.Type != corev1.ServiceTypeClusterIP {
			t.Errorf("service type = %q, want ClusterIP", service.Spec.Type)
		}

		snapshot := adapter.Observe(context.Background(), kube.ObserveRequest{
			ClusterRef: kindCluster,
			Namespace:  kindNamespace,
			TargetID:   request.DeploymentTargetID,
		})
		if snapshot.Source != kube.SourceKubernetes || snapshot.Freshness != kube.FreshnessFresh {
			t.Errorf("snapshot source/freshness = %q/%q, want kubernetes/fresh", snapshot.Source, snapshot.Freshness)
		}
		if !snapshot.DeploymentExists || snapshot.ReadyReplicas != 1 || snapshot.AvailableReplicas != 1 {
			t.Errorf("snapshot readiness = %#v, want one ready replica", snapshot)
		}
		if snapshot.ReleaseID == nil || *snapshot.ReleaseID != request.ReleaseID {
			t.Errorf("snapshot release id = %v, want %s", snapshot.ReleaseID, request.ReleaseID)
		}
	})

	t.Run("image pull failure", func(t *testing.T) {
		request := kindPublishRequest(
			"registry.invalid/orbitops/missing@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		)
		cleanupResources(t, client, request.DeploymentTargetID)

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		err := adapter.Publish(ctx, request)
		var failure *worker.FailureError
		if !errors.As(err, &failure) {
			t.Fatalf("publish error = %v, want structured failure", err)
		}
		if failure.Code() != "image_pull_failed" {
			t.Errorf("failure code = %q, want image_pull_failed", failure.Code())
		}

		snapshot := adapter.Observe(context.Background(), kube.ObserveRequest{
			ClusterRef: kindCluster,
			Namespace:  kindNamespace,
			TargetID:   request.DeploymentTargetID,
		})
		if snapshot.Freshness != kube.FreshnessFresh || !snapshot.DeploymentExists {
			t.Errorf("failure snapshot = %#v, want fresh Kubernetes observation", snapshot)
		}
		if len(snapshot.Pods) == 0 || snapshot.Pods[0].Reason == "" {
			t.Errorf("failure pod summaries = %#v, want observed pull reason", snapshot.Pods)
		}
	})

	t.Run("foreign deployment ownership conflict", func(t *testing.T) {
		request := kindPublishRequest(readyImage)
		cleanupResources(t, client, request.DeploymentTargetID)
		name := kube.ResourceName(request.DeploymentTargetID)
		replicas := int32(1)
		foreign := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: kindNamespace,
				Labels:    map[string]string{"owner": "external-test"},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "external",
								Image: readyImage,
								Ports: []corev1.ContainerPort{{
									Name:          "http",
									ContainerPort: 80,
								}},
							},
						},
					},
				},
			},
		}
		if _, err := client.AppsV1().Deployments(kindNamespace).Create(
			context.Background(),
			foreign,
			metav1.CreateOptions{},
		); err != nil {
			t.Fatalf("create foreign deployment: %v", err)
		}

		err := adapter.Publish(context.Background(), request)
		var failure *worker.FailureError
		if !errors.As(err, &failure) || failure.Code() != "ownership_conflict" {
			t.Fatalf("publish error = %v, want ownership_conflict", err)
		}
		preserved, err := client.AppsV1().Deployments(kindNamespace).Get(
			context.Background(),
			name,
			metav1.GetOptions{},
		)
		if err != nil {
			t.Fatalf("get foreign deployment after rejection: %v", err)
		}
		if preserved.Labels["owner"] != "external-test" {
			t.Errorf("foreign deployment changed: %#v", preserved.Labels)
		}
	})
}

func newKindAdapter(t *testing.T) (*kube.Adapter, kubernetes.Interface) {
	t.Helper()
	if os.Getenv("ORBITOPS_KIND_E2E") != "1" {
		t.Skip("set ORBITOPS_KIND_E2E=1 to run task-local Kind acceptance")
	}
	kubeconfigPath := clientcmd.RecommendedHomeFile
	adapter, err := kube.NewVerifiedLocalAdapter(
		context.Background(),
		kubeconfigPath,
		kindContext,
		kube.Config{
			ClusterRef:   kindCluster,
			Namespace:    kindNamespace,
			FieldManager: "orbitops-delivery",
			PollInterval: 250 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatalf("create verified local adapter: %v", err)
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		t.Fatalf("build Kind REST config: %v", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create Kind client: %v", err)
	}
	return adapter, client
}

func kindPublishRequest(image string) worker.PublishRequest {
	return worker.PublishRequest{
		OperationID:        uuid.New(),
		AttemptID:          uuid.New(),
		ReleaseID:          uuid.New(),
		ProjectID:          uuid.New(),
		ApplicationID:      uuid.New(),
		DeploymentTargetID: uuid.New(),
		ImageReference:     image,
		Stage:              "development",
		ClusterRef:         kindCluster,
		Namespace:          kindNamespace,
		Replicas:           1,
		ContainerPort:      80,
	}
}

func cleanupResources(t *testing.T, client kubernetes.Interface, targetID uuid.UUID) {
	t.Helper()
	name := kube.ResourceName(targetID)
	cleanup := func() {
		background := metav1.DeletePropagationBackground
		_ = client.AppsV1().Deployments(kindNamespace).Delete(
			context.Background(),
			name,
			metav1.DeleteOptions{PropagationPolicy: &background},
		)
		_ = client.CoreV1().Services(kindNamespace).Delete(
			context.Background(),
			name,
			metav1.DeleteOptions{},
		)
	}
	cleanup()
	t.Cleanup(cleanup)
}
