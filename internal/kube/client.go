package kube

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

func NewVerifiedLocalAdapter(
	ctx context.Context,
	kubeconfigPath string,
	expectedContext string,
	config Config,
) (*Adapter, error) {
	if kubeconfigPath == "" {
		return nil, errors.New("kubeconfig path is required")
	}
	if !strings.HasPrefix(expectedContext, "kind-") {
		return nil, errors.New("expected Kubernetes context must be a Kind context")
	}

	rawConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if rawConfig.CurrentContext != expectedContext {
		return nil, fmt.Errorf(
			"current Kubernetes context %q does not match expected local context %q",
			rawConfig.CurrentContext,
			expectedContext,
		)
	}
	contextConfig, ok := rawConfig.Contexts[expectedContext]
	if !ok {
		return nil, fmt.Errorf("expected Kubernetes context %q does not exist", expectedContext)
	}
	clusterConfig, ok := rawConfig.Clusters[contextConfig.Cluster]
	if !ok {
		return nil, fmt.Errorf("cluster for Kubernetes context %q does not exist", expectedContext)
	}
	if err := requireLoopbackServer(clusterConfig.Server); err != nil {
		return nil, err
	}

	restConfig, err := clientcmd.NewNonInteractiveClientConfig(
		*rawConfig,
		expectedContext,
		&clientcmd.ConfigOverrides{},
		nil,
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes REST config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client: %w", err)
	}
	if err := verifyKindIdentity(ctx, client, expectedContext, config.Namespace); err != nil {
		return nil, err
	}
	return New(client, config)
}

func requireLoopbackServer(server string) error {
	parsed, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("parse Kubernetes API Server URL: %w", err)
	}
	hostname := parsed.Hostname()
	if hostname == "localhost" {
		return nil
	}
	address := net.ParseIP(hostname)
	if address == nil || !address.IsLoopback() {
		return fmt.Errorf("Kubernetes API Server %q is not on a loopback address", server)
	}
	return nil
}

func verifyKindIdentity(
	ctx context.Context,
	client kubernetes.Interface,
	expectedContext string,
	namespace string,
) error {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list local Kind nodes: %w", err)
	}
	expectedNodePrefix := strings.TrimPrefix(expectedContext, "kind-") + "-"
	if len(nodes.Items) == 0 {
		return errors.New("local Kind cluster has no nodes")
	}
	for _, node := range nodes.Items {
		if !strings.HasPrefix(node.Name, expectedNodePrefix) {
			return fmt.Errorf(
				"Kubernetes node %q does not belong to expected Kind cluster %q",
				node.Name,
				expectedContext,
			)
		}
	}

	existingNamespace, err := client.CoreV1().Namespaces().Get(
		ctx,
		namespace,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get task Kubernetes namespace %q: %w", namespace, err)
	}
	if existingNamespace.Labels[ManagedByLabel] != ManagedByValue {
		return fmt.Errorf(
			"Kubernetes namespace %q is not marked as Orbit-DevOps-managed",
			namespace,
		)
	}
	return nil
}
