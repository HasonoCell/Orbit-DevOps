package buildkube_test

import (
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/buildkube"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildJobUsesPinnedRootlessImagesAndIsolatedRuntime(t *testing.T) {
	adapter, err := buildkube.New(nil, buildkube.Config{
		Namespace: "orbitops-build", FieldManager: "orbitops-build-worker",
		GitImage:           "alpine/git:v2.49.1@sha256:" + strings.Repeat("a", 64),
		BuildkitImage:      "moby/buildkit:v0.33.0-rootless@sha256:" + strings.Repeat("b", 64),
		RegistrySecretName: "orbitops-registry", ActiveDeadline: 20 * time.Minute,
		TTL: time.Hour, CPU: "1", Memory: "1Gi",
	})
	if err == nil || adapter != nil {
		t.Fatal("nil Kubernetes client must be rejected")
	}
	adapter, err = buildkube.New(fake.NewClientset(), buildkube.Config{
		Namespace: "orbitops-build", FieldManager: "orbitops-build-worker",
		GitImage:           "alpine/git:v2.49.1@sha256:" + strings.Repeat("a", 64),
		BuildkitImage:      "moby/buildkit:v0.33.0-rootless@sha256:" + strings.Repeat("b", 64),
		RegistrySecretName: "orbitops-registry", ActiveDeadline: 20 * time.Minute,
		TTL: time.Hour, CPU: "1", Memory: "1Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	execution := buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(), RepositoryURL: "https://github.com/example/demo.git",
		SourceCommit: strings.Repeat("c", 40), DockerfilePath: "deploy/Dockerfile", ContextPath: "deploy",
		Platform: "linux/amd64", DestinationRepository: "registry.example/orbitops/demo", InputDigest: "sha256:" + strings.Repeat("d", 64),
	}
	job := adapter.RenderJob(execution)
	if job.Namespace != "orbitops-build" || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1200 {
		t.Fatalf("job boundary = %s/%v", job.Namespace, job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("pod identity boundary = %#v", pod)
	}
	if len(pod.InitContainers) != 1 || !strings.Contains(pod.InitContainers[0].Image, "@sha256:") ||
		len(pod.Containers) != 1 || !strings.Contains(pod.Containers[0].Image, "@sha256:") {
		t.Fatalf("unpinned build images = %#v / %#v", pod.InitContainers, pod.Containers)
	}
	if !hasEnvironment(pod.InitContainers[0].Env, "GIT_CONFIG_VALUE_0", "/workspace") {
		t.Fatalf("Git safe directory is not scoped to the workspace: %#v", pod.InitContainers[0].Env)
	}
	container := pod.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.Privileged == nil || *container.SecurityContext.Privileged ||
		container.SecurityContext.AllowPrivilegeEscalation == nil || !*container.SecurityContext.AllowPrivilegeEscalation ||
		container.SecurityContext.RunAsUser == nil || *container.SecurityContext.RunAsUser != 1000 {
		t.Fatalf("rootless security context = %#v", container.SecurityContext)
	}
	if pod.InitContainers[0].SecurityContext.Capabilities == nil ||
		len(pod.InitContainers[0].SecurityContext.Capabilities.Drop) != 1 ||
		pod.InitContainers[0].SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("Git init container capabilities = %#v", pod.InitContainers[0].SecurityContext.Capabilities)
	}
	if container.Resources.Limits.Cpu().String() != "1" || container.Resources.Limits.Memory().String() != "1Gi" {
		t.Fatalf("build resources = %#v", container.Resources)
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil {
			t.Fatalf("build job must not mount hostPath: %#v", volume)
		}
	}
}

func hasEnvironment(environment []corev1.EnvVar, name, value string) bool {
	for _, item := range environment {
		if item.Name == name && item.Value == value {
			return true
		}
	}
	return false
}

func TestBuildJobConfiguresExplicitInsecureRegistryWithoutExposingCredentials(t *testing.T) {
	adapter, err := buildkube.New(fake.NewClientset(), buildkube.Config{
		Namespace: "orbitops-build", FieldManager: "orbitops-build-worker",
		GitImage:         "alpine/git:v2.49.1@sha256:" + strings.Repeat("a", 64),
		BuildkitImage:    "moby/buildkit:v0.33.0-rootless@sha256:" + strings.Repeat("b", 64),
		RegistryInsecure: true, ActiveDeadline: 20 * time.Minute,
		TTL: time.Hour, CPU: "1", Memory: "1Gi",
	})
	if err != nil {
		t.Fatal(err)
	}
	job := adapter.RenderJob(buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(), RepositoryURL: "https://github.com/example/demo.git",
		SourceCommit: strings.Repeat("c", 40), DockerfilePath: "Dockerfile", ContextPath: ".",
		Platform: "linux/amd64", DestinationRepository: "registry.local:5000/orbitops/demo",
		InputDigest: "sha256:" + strings.Repeat("d", 64),
	})
	container := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(container.Args[0], `http = true`) || !strings.Contains(container.Args[0], `insecure = true`) {
		t.Fatalf("insecure registry configuration is absent: %s", container.Args[0])
	}
	for _, environment := range container.Env {
		if strings.Contains(strings.ToLower(environment.Name), "password") || strings.Contains(strings.ToLower(environment.Name), "token") {
			t.Fatalf("credential-like environment variable exposed: %s", environment.Name)
		}
	}
}
