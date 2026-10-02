package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// 集群暂时不可达不能阻止装配数据库模块；数据库错误应来自后续数据库启动，而不是 K8s 探测。
func TestAPIContinuesStartupWhenKubernetesUnavailable(t *testing.T) {
	cluster := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer cluster.Close()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "kind-api-startup-test"
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "local"}
	config.Clusters["local"] = &clientcmdapi.Cluster{Server: cluster.URL}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*config, kubeconfig); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORBIT_DEVOPS_KUBECONFIG", kubeconfig)
	t.Setenv("ORBIT_DEVOPS_KUBERNETES_CONTEXT", config.CurrentContext)
	t.Setenv("ORBIT_DEVOPS_DATABASE_URL", "postgres://test:test@127.0.0.1:1/test?sslmode=disable&connect_timeout=1")
	t.Setenv("ORBIT_DEVOPS_MIGRATE_ON_BOOT", "false")
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "ping database") {
		t.Fatalf("Kubernetes outage prevented database startup: %v", err)
	}
}
