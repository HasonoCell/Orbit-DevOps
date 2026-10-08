package kubeconnection_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/kubeconnection"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestInClusterVerificationRequiresBothIdentityAndManagedNamespace(t *testing.T) {
	for _, test := range []struct {
		name, uid, label string
		accepted         bool
	}{
		{"matched", "cluster-one", kubeconnection.ManagedByValue, true},
		{"different cluster", "cluster-two", kubeconnection.ManagedByValue, false},
		{"unmanaged namespace", "cluster-one", "other", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(test.uid)}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orbit-apps", Labels: map[string]string{kubeconnection.ManagedByLabel: test.label}}},
			)
			config := kubeconnection.Config{Mode: kubeconnection.InCluster, ClusterUID: "cluster-one"}
			if err := config.Verify(context.Background(), client, "orbit-apps"); (err == nil) != test.accepted {
				t.Fatalf("accepted=%v, error=%v", test.accepted, err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" || action.GetResource().Resource != "namespaces" {
					t.Fatalf("unexpected verification action: %v", action)
				}
			}
		})
	}
}

func TestInClusterConfigurationCannotFallBackToKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	for _, config := range []kubeconnection.Config{
		{Mode: "anything", KubeconfigPath: "valid-local-config", Context: "kind-local"},
		{Mode: kubeconnection.InCluster},
		{Mode: kubeconnection.InCluster, ClusterUID: "cluster-one", KubeconfigPath: "valid-local-config", Context: "kind-local"},
	} {
		if _, err := config.Load(); err == nil {
			t.Fatal("unsafe configuration or non-Pod fallback accepted")
		}
	}
}

func TestLocalConnectionStillRequiresKindAndLoopback(t *testing.T) {
	for _, test := range []struct {
		name, server, current string
		accepted              bool
	}{
		{"loopback", "https://127.0.0.1:6443", "kind-local", true},
		{"non-loopback", "https://example.com:6443", "kind-local", false},
		{"wrong context", "https://127.0.0.1:6443", "production", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := clientcmdapi.Config{CurrentContext: test.current,
				Clusters: map[string]*clientcmdapi.Cluster{"local": {Server: test.server}},
				Contexts: map[string]*clientcmdapi.Context{"kind-local": {Cluster: "local"}},
			}
			data, err := clientcmd.Write(raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			config := kubeconnection.Config{KubeconfigPath: path, Context: "kind-local"}
			if _, err := config.Load(); (err == nil) != test.accepted {
				t.Fatalf("accepted=%v, error=%v", test.accepted, err)
			}
		})
	}
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "foreign-control-plane"}})
	if err := (kubeconnection.Config{KubeconfigPath: "config", Context: "kind-local"}).Verify(context.Background(), client, "orbit-apps"); err == nil {
		t.Fatal("foreign Kind node accepted")
	}
}
