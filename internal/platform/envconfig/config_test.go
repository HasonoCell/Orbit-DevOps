package envconfig_test

import (
	"testing"

	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
)

func TestLocalDefaultsBindProcessesAndKubernetesBoundary(t *testing.T) {
	for _, name := range []string{
		"ORBITOPS_API_ADDRESS",
		"ORBITOPS_WORKER_ADDRESS",
		"ORBITOPS_KUBERNETES_CONTEXT",
		"ORBITOPS_CLUSTER_REF",
		"ORBITOPS_NAMESPACE",
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
}

func TestWorkerRejectsInvalidDuration(t *testing.T) {
	t.Setenv("ORBITOPS_WORKER_LEASE_DURATION", "forever")
	if _, err := envconfig.LoadWorker(); err == nil {
		t.Fatal("invalid Worker lease duration was accepted")
	}
}
