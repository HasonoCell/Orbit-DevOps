package envconfig_test

import (
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
)

func TestQueueDefaultsReserveBusinessCompletionTime(t *testing.T) {
	t.Setenv("ORBITOPS_OPERATION_TIMEOUT", "3m")
	t.Setenv("ORBITOPS_REDIS_ADDRESS", "")
	t.Setenv("ORBITOPS_QUEUE_TASK_TIMEOUT", "")
	config, err := envconfig.LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.Queue.RedisAddress != "127.0.0.1:6379" || config.Queue.TaskTimeout < 3*time.Minute+30*time.Second || config.Queue.Concurrency != 4 {
		t.Fatalf("queue defaults do not reserve completion time")
	}
	t.Setenv("ORBITOPS_QUEUE_TASK_TIMEOUT", "3m")
	if _, err := envconfig.LoadWorker(); err == nil {
		t.Fatal("queue timeout shorter than business envelope accepted")
	}
}

func TestLocalDefaultsBindProcessesAndKubernetesBoundary(t *testing.T) {
	for _, name := range []string{
		"ORBITOPS_API_ADDRESS",
		"ORBITOPS_WORKER_ADDRESS",
		"ORBITOPS_KUBERNETES_CONTEXT",
		"ORBITOPS_CLUSTER_REF",
		"ORBITOPS_NAMESPACE",
		"ORBITOPS_MAX_AUTOMATIC_RETRIES",
		"ORBITOPS_RETRY_BASE_DELAY",
	} {
		t.Setenv(name, "")
	}

	apiConfig, err := envconfig.LoadAPI()
	if err != nil {
		t.Fatalf("load API defaults: %v", err)
	}
	workerConfig, err := envconfig.LoadWorker()
	if err != nil {
		t.Fatalf("load Worker defaults: %v", err)
	}
	if apiConfig.Address != "127.0.0.1:8080" {
		t.Errorf("API address = %q, want loopback default", apiConfig.Address)
	}
	if workerConfig.Address != "127.0.0.1:9091" {
		t.Errorf("Worker address = %q, want loopback default", workerConfig.Address)
	}
	if apiConfig.Kubernetes.Context != "kind-orbitops-s1" ||
		apiConfig.Kubernetes.ClusterRef != "kind-orbitops-s1" ||
		apiConfig.Kubernetes.Namespace != "orbitops-s1" {
		t.Errorf("API Kubernetes defaults = %#v, want S1 local boundary", apiConfig.Kubernetes)
	}
	if workerConfig.MaximumAutomaticRetries != 2 || workerConfig.RetryBaseDelay.String() != "1s" {
		t.Errorf("Worker retry defaults = %#v, want 2 retries with 1s base delay", workerConfig)
	}
}

func TestWorkerRejectsNegativeAutomaticRetryCount(t *testing.T) {
	t.Setenv("ORBITOPS_MAX_AUTOMATIC_RETRIES", "-1")
	if _, err := envconfig.LoadWorker(); err == nil {
		t.Fatal("negative automatic retry count was accepted")
	}
}

func TestWorkerRejectsInvalidDuration(t *testing.T) {
	t.Setenv("ORBITOPS_WORKER_LEASE_DURATION", "forever")
	if _, err := envconfig.LoadWorker(); err == nil {
		t.Fatal("invalid Worker lease duration was accepted")
	}
}
