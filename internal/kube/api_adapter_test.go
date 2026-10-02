package kube_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// 恢复后必须先重新检查节点、Namespace；错误集群不能获得运行时读权限。
func TestAPIAdapterReverifiesClusterAfterOutage(t *testing.T) {
	var mode atomic.Int32
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes":
			name := "api-test-control-plane"
			if mode.Load() == 1 {
				name = "foreign-control-plane"
			}
			_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
				Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: name}}}})
		case "/api/v1/namespaces/task-ns":
			_ = json.NewEncoder(w).Encode(corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
				ObjectMeta: metav1.ObjectMeta{Name: "task-ns", Labels: map[string]string{kube.ManagedByLabel: kube.ManagedByValue}}})
		default:
			reads.Add(1)
			if strings.HasSuffix(r.URL.Path, "/pods") {
				_ = json.NewEncoder(w).Encode(corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}, Items: []corev1.Pod{}})
			} else if strings.HasSuffix(r.URL.Path, "/events") {
				_ = json.NewEncoder(w).Encode(corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: []corev1.Event{}})
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	defer server.Close()
	adapter, err := kube.NewAPIAdapter(context.Background(), apiTestKubeconfig(t, server.URL), "kind-api-test",
		kube.Config{ClusterRef: "kind-api-test", Namespace: "task-ns", FieldManager: "test"}, "")
	if err != nil {
		t.Fatalf("temporary outage stopped API adapter: %v", err)
	}
	query := diagnostics.TargetRuntimeQuery{ClusterRef: "kind-api-test", Namespace: "task-ns"}
	if got := adapter.ObserveTarget(context.Background(), query); got.Workload.Metadata.Status != diagnostics.ObservationUnavailable {
		t.Fatalf("outage observation = %s", got.Workload.Metadata.Status)
	}
	mode.Store(1)
	if _, err := kube.NewAPIAdapter(context.Background(), apiTestKubeconfig(t, server.URL), "kind-api-test",
		kube.Config{ClusterRef: "kind-api-test", Namespace: "task-ns", FieldManager: "test"}, ""); err == nil {
		t.Fatal("foreign cluster was accepted during startup")
	}
	// 非预期集群恢复网络后仍拒绝读取业务资源。
	time.Sleep(1100 * time.Millisecond)
	if got := adapter.ObserveTarget(context.Background(), query); got.Workload.Metadata.Status != diagnostics.ObservationUnavailable || reads.Load() != 0 {
		t.Fatalf("foreign cluster read: status=%s reads=%d", got.Workload.Metadata.Status, reads.Load())
	}
	mode.Store(2)
	time.Sleep(1100 * time.Millisecond)
	if got := adapter.ObserveTarget(context.Background(), query); got.Workload.Metadata.Status != diagnostics.ObservationComplete || reads.Load() == 0 {
		t.Fatalf("verified recovery: status=%s reads=%d", got.Workload.Metadata.Status, reads.Load())
	}
}

func TestAPIAdapterRejectsUnsafeStartup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	untrustedTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer untrustedTLS.Close()
	for _, endpoint := range []string{server.URL, "https://production.example", untrustedTLS.URL} {
		if _, err := kube.NewAPIAdapter(context.Background(), apiTestKubeconfig(t, endpoint), "kind-api-test",
			kube.Config{ClusterRef: "kind-api-test", Namespace: "task-ns", FieldManager: "test"}, ""); err == nil {
			t.Fatalf("unsafe or forbidden endpoint was accepted: %s", endpoint)
		}
	}
	// Worker 保持原来的 fail-closed 构造器，不因 API 降级获得写权限。
	if _, err := kube.NewVerifiedLocalAdapter(context.Background(), apiTestKubeconfig(t, server.URL), "kind-api-test",
		kube.Config{ClusterRef: "kind-api-test", Namespace: "task-ns", FieldManager: "test"}); err == nil {
		t.Fatal("worker accepted unverified cluster")
	}
}

func TestAPIAdapterBoundsStartupProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := kube.NewAPIAdapter(ctx, apiTestKubeconfig(t, server.URL), "kind-api-test",
		kube.Config{ClusterRef: "kind-api-test", Namespace: "task-ns", FieldManager: "test"}, "")
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("canceled startup probe: error=%v duration=%s", err, time.Since(started))
	}
}

func apiTestKubeconfig(t *testing.T, endpoint string) string {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "kind-api-test"
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "local"}
	config.Clusters["local"] = &clientcmdapi.Cluster{Server: endpoint}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatal(err)
	}
	return path
}
