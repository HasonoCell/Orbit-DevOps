package envconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
)

func TestReleaseQueueDefaultsReserveBusinessCompletionTime(t *testing.T) {
	t.Setenv("ORBITOPS_RELEASE_OPERATION_TIMEOUT", "3m")
	t.Setenv("ORBITOPS_REDIS_ADDRESS", "")
	t.Setenv("ORBITOPS_RELEASE_QUEUE_TASK_TIMEOUT", "")
	config, err := envconfig.LoadReleaseWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.ReleaseQueue.RedisAddress != "127.0.0.1:6379" || config.ReleaseQueue.TaskTimeout < 3*time.Minute+30*time.Second || config.ReleaseQueue.Concurrency != 4 {
		t.Fatalf("queue defaults do not reserve completion time")
	}
	t.Setenv("ORBITOPS_RELEASE_QUEUE_TASK_TIMEOUT", "3m")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("queue timeout shorter than business envelope accepted")
	}
}

func TestLocalDefaultsBindProcessesAndKubernetesBoundary(t *testing.T) {
	for _, name := range []string{
		"ORBITOPS_API_ADDRESS",
		"ORBITOPS_RELEASE_WORKER_ADDRESS",
		"ORBITOPS_KUBERNETES_CONTEXT",
		"ORBITOPS_CLUSTER_REF",
		"ORBITOPS_NAMESPACE",
		"ORBITOPS_RELEASE_MAX_AUTOMATIC_RETRIES",
		"ORBITOPS_RELEASE_RETRY_BASE_DELAY",
	} {
		t.Setenv(name, "")
	}

	apiConfig, err := envconfig.LoadAPI()
	if err != nil {
		t.Fatalf("load API defaults: %v", err)
	}
	workerConfig, err := envconfig.LoadReleaseWorker()
	if err != nil {
		t.Fatalf("load ReleaseWorker defaults: %v", err)
	}
	if apiConfig.Address != "127.0.0.1:8080" {
		t.Errorf("API address = %q, want loopback default", apiConfig.Address)
	}
	if workerConfig.Address != "127.0.0.1:9091" {
		t.Errorf("ReleaseWorker address = %q, want loopback default", workerConfig.Address)
	}
	if apiConfig.Kubernetes.Context != "kind-orbitops-s1" ||
		apiConfig.Kubernetes.ClusterRef != "kind-orbitops-s1" ||
		apiConfig.Kubernetes.Namespace != "orbitops-s1" {
		t.Errorf("API Kubernetes defaults = %#v, want S1 local boundary", apiConfig.Kubernetes)
	}
	if workerConfig.MaximumAutomaticRetries != 2 || workerConfig.RetryBaseDelay.String() != "1s" {
		t.Errorf("ReleaseWorker retry defaults = %#v, want 2 retries with 1s base delay", workerConfig)
	}
}

func TestReleaseWorkerRejectsNegativeAutomaticRetryCount(t *testing.T) {
	t.Setenv("ORBITOPS_RELEASE_MAX_AUTOMATIC_RETRIES", "-1")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("negative automatic retry count was accepted")
	}
}

func TestReleaseWorkerRejectsInvalidDuration(t *testing.T) {
	t.Setenv("ORBITOPS_RELEASE_WORKER_LEASE_DURATION", "forever")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("invalid ReleaseWorker lease duration was accepted")
	}
}

func TestReleaseWorkerDoesNotReadLegacyEnvironmentNames(t *testing.T) {
	t.Setenv("ORBITOPS_WORKER_ADDRESS", "127.0.0.1:19091")
	t.Setenv("ORBITOPS_WORKER_ID", "legacy-worker")
	t.Setenv("ORBITOPS_RELEASE_WORKER_ADDRESS", "")
	t.Setenv("ORBITOPS_RELEASE_WORKER_ID", "")

	config, err := envconfig.LoadReleaseWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.Address != "127.0.0.1:9091" {
		t.Fatalf("ReleaseWorker address = %q, want the new default", config.Address)
	}
	if config.WorkerID == "legacy-worker" {
		t.Fatal("ReleaseWorker unexpectedly read the legacy worker ID")
	}
}

func TestBuildWorkerDefaultsUseIndependentQueueAndPinnedImages(t *testing.T) {
	for _, name := range []string{"ORBITOPS_BUILD_QUEUE_NAME", "ORBITOPS_BUILD_GIT_IMAGE", "ORBITOPS_BUILDKIT_IMAGE",
		"ORBITOPS_BUILD_OPERATION_TIMEOUT", "ORBITOPS_BUILD_QUEUE_TASK_TIMEOUT", "ORBITOPS_BUILD_NAMESPACE"} {
		t.Setenv(name, "")
	}
	config, err := envconfig.LoadBuildWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.BuildQueue.Name != "orbitops-build" || config.Namespace != "orbitops-s4-build" ||
		config.BuildQueue.TaskTimeout < config.BuildOperationTimeout+time.Minute {
		t.Fatalf("build worker defaults = %#v", config)
	}
	if !strings.Contains(config.GitImage, "@sha256:") || !strings.Contains(config.BuildkitImage, "@sha256:") {
		t.Fatalf("build images are not pinned: %q / %q", config.GitImage, config.BuildkitImage)
	}
}

func TestBuildWorkerRejectsMutableRuntimeImage(t *testing.T) {
	t.Setenv("ORBITOPS_BUILDKIT_IMAGE", "moby/buildkit:v0.33.0-rootless")
	if _, err := envconfig.LoadBuildWorker(); err == nil {
		t.Fatal("mutable BuildKit image was accepted")
	}
}
