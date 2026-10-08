package envconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
)

func TestCloudConnectionIsExplicitAndSharedByClusterProcesses(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_KUBERNETES_MODE", "in-cluster")
	t.Setenv("ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID", "")
	if _, err := envconfig.LoadAPI(); err == nil {
		t.Fatal("in-cluster mode accepted without pinned identity")
	}
	if _, err := envconfig.LoadBuildWorker(); err == nil {
		t.Fatal("build worker accepted missing cluster identity")
	}
	t.Setenv("ORBIT_DEVOPS_KUBERNETES_CLUSTER_UID", "cluster-one")
	api, err := envconfig.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	build, err := envconfig.LoadBuildWorker()
	if err != nil {
		t.Fatal(err)
	}
	if api.Kubernetes.Connection.Mode != "in-cluster" || api.Kubernetes.Connection != build.KubernetesConnection {
		t.Fatal("process connection identities differ")
	}
	t.Setenv("ORBIT_DEVOPS_KUBERNETES_MODE", "automatic")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestReleaseQueueDefaultsReserveBusinessCompletionTime(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_RELEASE_OPERATION_TIMEOUT", "3m")
	t.Setenv("ORBIT_DEVOPS_REDIS_ADDRESS", "")
	t.Setenv("ORBIT_DEVOPS_RELEASE_QUEUE_TASK_TIMEOUT", "")
	config, err := envconfig.LoadReleaseWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.ReleaseQueue.RedisAddress != "127.0.0.1:6379" || config.ReleaseQueue.TaskTimeout < 3*time.Minute+30*time.Second || config.ReleaseQueue.Concurrency != 4 {
		t.Fatalf("queue defaults do not reserve completion time")
	}
	t.Setenv("ORBIT_DEVOPS_RELEASE_QUEUE_TASK_TIMEOUT", "3m")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("queue timeout shorter than business envelope accepted")
	}
}

func TestLocalDefaultsBindProcessesAndKubernetesBoundary(t *testing.T) {
	for _, name := range []string{
		"ORBIT_DEVOPS_API_ADDRESS",
		"ORBIT_DEVOPS_RELEASE_WORKER_ADDRESS",
		"ORBIT_DEVOPS_KUBERNETES_CONTEXT",
		"ORBIT_DEVOPS_CLUSTER_REF",
		"ORBIT_DEVOPS_NAMESPACE",
		"ORBIT_DEVOPS_RELEASE_MAX_AUTOMATIC_RETRIES",
		"ORBIT_DEVOPS_RELEASE_RETRY_BASE_DELAY",
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
	if apiConfig.Browser.ExternalURL != "http://127.0.0.1:5173" ||
		apiConfig.OIDC.RedirectURL != "http://127.0.0.1:8080/api/v1/auth/oidc/callback" {
		t.Errorf("browser/OIDC defaults = %q / %q, want separate frontend and API callback URLs",
			apiConfig.Browser.ExternalURL, apiConfig.OIDC.RedirectURL)
	}
	if workerConfig.Address != "127.0.0.1:9091" {
		t.Errorf("ReleaseWorker address = %q, want loopback default", workerConfig.Address)
	}
	if apiConfig.Kubernetes.Connection.Context != "kind-orbit-devops-s1" ||
		apiConfig.Kubernetes.ClusterRef != "kind-orbit-devops-s1" ||
		apiConfig.Kubernetes.Namespace != "orbit-devops-s1" {
		t.Errorf("API Kubernetes defaults = %#v, want S1 local boundary", apiConfig.Kubernetes)
	}
	if workerConfig.MaximumAutomaticRetries != 2 || workerConfig.RetryBaseDelay.String() != "1s" {
		t.Errorf("ReleaseWorker retry defaults = %#v, want 2 retries with 1s base delay", workerConfig)
	}
}

func TestReleaseWorkerRejectsNegativeAutomaticRetryCount(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_RELEASE_MAX_AUTOMATIC_RETRIES", "-1")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("negative automatic retry count was accepted")
	}
}

func TestReleaseWorkerRejectsInvalidDuration(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_RELEASE_WORKER_LEASE_DURATION", "forever")
	if _, err := envconfig.LoadReleaseWorker(); err == nil {
		t.Fatal("invalid ReleaseWorker lease duration was accepted")
	}
}

func TestReleaseWorkerDoesNotReadLegacyEnvironmentNames(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_WORKER_ADDRESS", "127.0.0.1:19091")
	t.Setenv("ORBIT_DEVOPS_WORKER_ID", "legacy-worker")
	t.Setenv("ORBIT_DEVOPS_RELEASE_WORKER_ADDRESS", "")
	t.Setenv("ORBIT_DEVOPS_RELEASE_WORKER_ID", "")

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
	for _, name := range []string{"ORBIT_DEVOPS_BUILD_QUEUE_NAME", "ORBIT_DEVOPS_BUILD_GIT_IMAGE", "ORBIT_DEVOPS_BUILDKIT_IMAGE",
		"ORBIT_DEVOPS_BUILD_OPERATION_TIMEOUT", "ORBIT_DEVOPS_BUILD_QUEUE_TASK_TIMEOUT", "ORBIT_DEVOPS_BUILD_NAMESPACE",
		"ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR", "ORBIT_DEVOPS_BUILD_DOCKERHUB_MIRROR_INSECURE"} {
		t.Setenv(name, "")
	}
	config, err := envconfig.LoadBuildWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.BuildQueue.Name != "orbit-devops-build" || config.Namespace != "orbit-devops-s4-build" ||
		config.BuildQueue.TaskTimeout < config.BuildOperationTimeout+time.Minute {
		t.Fatalf("build worker defaults = %#v", config)
	}
	if !strings.Contains(config.GitImage, "@sha256:") || !strings.Contains(config.BuildkitImage, "@sha256:") {
		t.Fatalf("build images are not pinned: %q / %q", config.GitImage, config.BuildkitImage)
	}
}

func TestBuildWorkerRejectsMutableRuntimeImage(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_BUILDKIT_IMAGE", "moby/buildkit:v0.33.0-rootless")
	if _, err := envconfig.LoadBuildWorker(); err == nil {
		t.Fatal("mutable BuildKit image was accepted")
	}
}

func TestAPILoadsWebhookSecretRotationWithoutDefaultSecret(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_GITHUB_WEBHOOK_ENDPOINTS", `{"public":{"currentSecret":"current","previousSecret":"previous"}}`)
	t.Setenv("ORBIT_DEVOPS_GITHUB_WEBHOOK_MAX_BODY_BYTES", "2048")
	config, err := envconfig.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := config.GitHubWebhook.Endpoints["public"]
	if endpoint.CurrentSecret != "current" || endpoint.PreviousSecret != "previous" || config.GitHubWebhook.MaxBodyBytes != 2048 {
		t.Fatalf("webhook config = %#v", config.GitHubWebhook)
	}

	t.Setenv("ORBIT_DEVOPS_GITHUB_WEBHOOK_ENDPOINTS", `{"public":{"previousSecret":"previous"}}`)
	if _, err := envconfig.LoadAPI(); err == nil {
		t.Fatal("endpoint without current secret was accepted")
	}
}

func TestPipelineWorkerDefaultsKeepIndependentQueue(t *testing.T) {
	for _, name := range []string{"ORBIT_DEVOPS_PIPELINE_WORKER_ADDRESS", "ORBIT_DEVOPS_PIPELINE_QUEUE_NAME", "ORBIT_DEVOPS_PIPELINE_SOURCE_RECOVERY_WINDOW"} {
		t.Setenv(name, "")
	}
	config, err := envconfig.LoadPipelineWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.Address != "127.0.0.1:9093" || config.Queue.Name != "orbit-devops-pipeline" || config.SourceRecoveryWindow != 15*time.Minute {
		t.Fatalf("pipeline worker defaults = %#v", config)
	}
}
