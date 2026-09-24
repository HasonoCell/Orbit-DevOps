package kind_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kindClusterName = environmentOrDefault("ORBIT_DEVOPS_KIND_CLUSTER_NAME", "orbit-devops-s1")
	kindContext     = "kind-" + kindClusterName
	kindCluster     = kindContext
	kindNamespace   = environmentOrDefault("ORBIT_DEVOPS_NAMESPACE", "orbit-devops-s3")
)

const readyImage = "registry.k8s.io/pause@sha256:ee6521f290b2168b6e0935a181d4cff9be1ac3f505666ef0e3c98fae8199917a"

func TestKindDeliveryScenarios(t *testing.T) {
	adapter, client := newKindAdapter(t)

	t.Run("ready deployment and target observation", func(t *testing.T) {
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

		observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
			ClusterRef: kindCluster,
			Namespace:  kindNamespace,
			TargetID:   request.DeploymentTargetID,
		})
		if observation.Workload.Metadata.Source != diagnostics.SourceKubernetes ||
			observation.Workload.Metadata.Status != diagnostics.ObservationComplete {
			t.Errorf("observation metadata = %#v, want complete Kubernetes evidence", observation.Workload.Metadata)
		}
		deploymentEvidence := observation.Workload.Deployment
		if deploymentEvidence == nil || deploymentEvidence.ReadyReplicas != 1 || deploymentEvidence.AvailableReplicas != 1 {
			t.Errorf("deployment evidence = %#v, want one ready replica", deploymentEvidence)
		}
		if deploymentEvidence.ReleaseID == nil || *deploymentEvidence.ReleaseID != request.ReleaseID {
			t.Errorf("observed release id = %v, want %s", deploymentEvidence.ReleaseID, request.ReleaseID)
		}
	})

	t.Run("image pull failure", func(t *testing.T) {
		request := kindPublishRequest(
			"registry.invalid/orbit-devops/missing@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		)
		cleanupResources(t, client, request.DeploymentTargetID)

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		err := adapter.Publish(ctx, request)
		var failure *releaseworker.FailureError
		if !errors.As(err, &failure) {
			t.Fatalf("publish error = %v, want structured failure", err)
		}
		if failure.Code() != "image_pull_failed" {
			t.Errorf("failure code = %q, want image_pull_failed", failure.Code())
		}

		observation := adapter.ObserveTarget(context.Background(), diagnostics.TargetRuntimeQuery{
			ClusterRef: kindCluster,
			Namespace:  kindNamespace,
			TargetID:   request.DeploymentTargetID,
		})
		if observation.Workload.Metadata.Status != diagnostics.ObservationComplete ||
			observation.Workload.Deployment == nil {
			t.Errorf("failure observation = %#v, want complete Kubernetes evidence", observation.Workload)
		}
		if len(observation.Workload.Pods) == 0 || observation.Workload.Pods[0].Reason == "" {
			t.Errorf("failure pod evidence = %#v, want observed pull reason", observation.Workload.Pods)
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
		var failure *releaseworker.FailureError
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

func TestKindRuntimeLogsReadsPreviousContainerInstance(t *testing.T) {
	adapter, client := newKindAdapter(t)
	request := kindPublishRequest(readyImage)
	podName := "orbit-devops-log-" + request.ReleaseID.String()[:8]
	labels := map[string]string{
		kube.ManagedByLabel:     kube.ManagedByValue,
		kube.ProjectIDLabel:     request.ProjectID.String(),
		kube.ApplicationIDLabel: request.ApplicationID.String(),
		kube.TargetIDLabel:      request.DeploymentTargetID.String(),
		kube.ReleaseIDLabel:     request.ReleaseID.String(),
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: kindNamespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name: "application", Image: "registry.k8s.io/coredns/coredns:v1.14.2",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/coredns", "-version"},
			}},
		},
	}
	if _, err := client.CoreV1().Pods(kindNamespace).Create(
		context.Background(), pod, metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("create runtime log Pod: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CoreV1().Pods(kindNamespace).Delete(
			context.Background(), podName, metav1.DeleteOptions{},
		)
	})

	deadline := time.Now().Add(90 * time.Second)
	for {
		current, err := client.CoreV1().Pods(kindNamespace).Get(
			context.Background(), podName, metav1.GetOptions{},
		)
		if err != nil {
			t.Fatalf("get runtime log Pod: %v", err)
		}
		if len(current.Status.ContainerStatuses) == 1 &&
			current.Status.ContainerStatuses[0].LastTerminationState.Terminated != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime log Pod did not produce a previous container instance")
		}
		time.Sleep(250 * time.Millisecond)
	}

	result, err := adapter.ReadReleaseLogs(context.Background(), diagnostics.ReleaseRuntimeLogQuery{
		ProjectID: request.ProjectID, ApplicationID: request.ApplicationID,
		TargetID: request.DeploymentTargetID, ReleaseID: request.ReleaseID,
		ClusterRef: kindCluster, Namespace: kindNamespace,
		PodName: podName, Container: "application", TailLines: 20, Previous: true,
	})
	if err != nil {
		t.Fatalf("read previous runtime logs: %v", err)
	}
	if !strings.Contains(result.Content, "CoreDNS") {
		t.Fatalf("previous runtime logs = %q", result.Content)
	}
}

func newKindAdapter(t *testing.T) (*kube.Adapter, kubernetes.Interface) {
	t.Helper()
	if os.Getenv("ORBIT_DEVOPS_KIND_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_KIND_E2E=1 to run task-local Kind acceptance")
	}
	kubeconfigPath := kindKubeconfigPath()
	adapter, err := kube.NewVerifiedLocalAdapter(
		context.Background(),
		kubeconfigPath,
		kindContext,
		kube.Config{
			ClusterRef:   kindCluster,
			Namespace:    kindNamespace,
			FieldManager: "orbit-devops-delivery",
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

func kindPublishRequest(image string) releaseworker.PublishRequest {
	return releaseworker.PublishRequest{
		ReleaseOperationID: uuid.New(),
		ReleaseAttemptID:   uuid.New(),
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

func environmentOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// kindKubeconfigPath 优先使用本轮测试的隔离配置，避免读取用户当前的集群上下文。
func kindKubeconfigPath() string {
	return environmentOrDefault("ORBIT_DEVOPS_KUBECONFIG", clientcmd.RecommendedHomeFile)
}
