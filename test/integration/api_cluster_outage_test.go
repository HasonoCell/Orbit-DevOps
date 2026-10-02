package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// 使用真实 PostgreSQL/Gin/Session：K8s 503 时仍能登录和查询历史，诊断明确标记 unavailable。
func TestAPIClusterOutageKeepsDatabaseRoutesAvailable(t *testing.T) {
	cluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer cluster.Close()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "kind-orbit-devops-s1"
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "local"}
	config.Clusters["local"] = &clientcmdapi.Cluster{Server: cluster.URL}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	adapter, err := kube.NewAPIAdapter(context.Background(), path, config.CurrentContext,
		kube.Config{ClusterRef: config.CurrentContext, Namespace: "orbit-devops-s1", FieldManager: "test"}, "test-gateway")
	if err != nil {
		t.Fatal(err)
	}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{RuntimeSource: adapter, SecretVerifier: adapter,
		AccessObserver: adapter, RecoveryPublisher: adapter})
	target := createDeploymentTargetWithSuffix(t, environment, "cluster-outage")
	acceptance := createReleaseForTarget(t, environment, target.ID, "cluster-outage")
	for _, path := range []string{"/healthz", "/api/v1/users/me", "/api/v1/projects", "/api/v1/releases/" + acceptance.Release.ID} {
		response := requestJSON(t, environment.server, http.MethodGet, path, "", "")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("database route %s status = %d", path, response.StatusCode)
		}
	}
	response := requestJSON(t, environment.server, http.MethodGet, "/api/v1/releases/"+acceptance.Release.ID+"/diagnostics", "", "")
	defer response.Body.Close()
	var report api.ReleaseDiagnosticReport
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&report) != nil || report.WorkloadObservation.Metadata.Status != "unavailable" {
		t.Fatalf("outage diagnostics: HTTP %d workload=%s", response.StatusCode, report.WorkloadObservation.Metadata.Status)
	}
	response = environment.postJSON(t, "/api/v1/platform/access-secret-bindings", "outage-secret",
		`{"projectId":"`+target.ProjectID+`","hostname":"outage.orbit.test","secretName":"outage-tls"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unverified TLS registration status = %d", response.StatusCode)
	}
}
