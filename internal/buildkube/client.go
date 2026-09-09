package buildkube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewVerifiedLocalAdapter 在创建写权限 Adapter 前核验当前 Context、回环 API Server、Kind 节点与专用 Namespace。
func NewVerifiedLocalAdapter(ctx context.Context, kubeconfigPath, expectedContext string, config Config) (*Adapter, error) {
	if kubeconfigPath == "" || !strings.HasPrefix(expectedContext, "kind-") {
		return nil, errors.New("build worker requires an explicit local Kind context")
	}
	rawConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load build kubeconfig: %w", err)
	}
	if rawConfig.CurrentContext != expectedContext {
		return nil, fmt.Errorf("current Kubernetes context %q does not match expected local context %q", rawConfig.CurrentContext, expectedContext)
	}
	contextConfig, ok := rawConfig.Contexts[expectedContext]
	if !ok {
		return nil, fmt.Errorf("expected Kubernetes context %q does not exist", expectedContext)
	}
	clusterConfig, ok := rawConfig.Clusters[contextConfig.Cluster]
	if !ok {
		return nil, errors.New("expected local Kind cluster configuration is missing")
	}
	parsed, err := url.Parse(clusterConfig.Server)
	if err != nil {
		return nil, fmt.Errorf("parse Kubernetes API Server URL: %w", err)
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if host != "localhost" && (address == nil || !address.IsLoopback()) {
		return nil, fmt.Errorf("Kubernetes API Server %q is not on a loopback address", clusterConfig.Server)
	}
	restConfig, err := clientcmd.NewNonInteractiveClientConfig(*rawConfig, expectedContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes REST config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client: %w", err)
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		return nil, errors.New("local Kind cluster identity could not be verified")
	}
	prefix := strings.TrimPrefix(expectedContext, "kind-") + "-"
	for _, node := range nodes.Items {
		if !strings.HasPrefix(node.Name, prefix) {
			return nil, fmt.Errorf("Kubernetes node %q does not belong to expected Kind cluster", node.Name)
		}
	}
	namespace, err := client.CoreV1().Namespaces().Get(ctx, config.Namespace, metav1.GetOptions{})
	if err != nil || namespace.Labels[ManagedByLabel] != ManagedByValue {
		return nil, fmt.Errorf("build namespace %q is unavailable or not OrbitOps-managed", config.Namespace)
	}
	return New(client, config)
}
