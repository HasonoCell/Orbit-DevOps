// Package kubeconnection 统一部署者选择的连接与身份检查；不接受业务请求中的集群地址或凭据。
package kubeconnection

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	LocalKind      = "local-kind"
	InCluster      = "in-cluster"
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "orbit-devops"
)

// Config 的零模式保留本地默认；集群内模式必须由部署者提供 kube-system Namespace 的 UID。
// UID 是身份锚点而非授权，实际资源权限由各进程独立的 ServiceAccount/RBAC 限定。
type Config struct {
	Mode           string
	KubeconfigPath string
	Context        string
	ClusterUID     string
}

func (c Config) Validate() error {
	switch c.Mode {
	case "", LocalKind:
		if c.KubeconfigPath == "" || !strings.HasPrefix(c.Context, "kind-") {
			return errors.New("local Kubernetes connection requires an explicit Kind context and kubeconfig")
		}
	case InCluster:
		if strings.TrimSpace(c.ClusterUID) == "" {
			return errors.New("in-cluster Kubernetes connection requires a pinned cluster UID")
		}
	default:
		return errors.New("unsupported Kubernetes connection mode")
	}
	return nil
}

// Load 仅核验静态连接配置。在线身份检查必须成功后，调用者才可交付可写 Adapter。
func (c Config) Load() (*rest.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Mode == InCluster {
		config, err := rest.InClusterConfig()
		if err != nil {
			return nil, errors.New("load in-cluster Kubernetes credentials failed")
		}
		parsed, err := url.Parse(config.Host)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || config.Insecure || (config.CAFile == "" && len(config.CAData) == 0) {
			return nil, errors.New("in-cluster Kubernetes connection requires HTTPS and a trusted CA")
		}
		return config, nil
	}
	raw, err := clientcmd.LoadFromFile(c.KubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("load local kubeconfig: %w", err)
	}
	if raw.CurrentContext != c.Context {
		return nil, errors.New("current Kubernetes context does not match the expected Kind context")
	}
	selected, ok := raw.Contexts[c.Context]
	if !ok {
		return nil, errors.New("expected Kind context does not exist")
	}
	cluster, ok := raw.Clusters[selected.Cluster]
	if !ok {
		return nil, errors.New("expected Kind cluster configuration does not exist")
	}
	parsed, err := url.Parse(cluster.Server)
	if err != nil {
		return nil, errors.New("invalid local Kubernetes API Server URL")
	}
	address := net.ParseIP(parsed.Hostname())
	if parsed.Hostname() != "localhost" && (address == nil || !address.IsLoopback()) {
		return nil, errors.New("local Kubernetes API Server must use a loopback address")
	}
	return clientcmd.NewNonInteractiveClientConfig(*raw, c.Context, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
}

// Verify 在同一连接上核验集群身份与目标 Namespace；不创建、不修复、不补标签。
// 集群内模式不依赖节点名称，不要求给运行进程授予全局节点读取权限。
func (c Config) Verify(ctx context.Context, client kubernetes.Interface, namespace string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if client == nil || namespace == "" {
		return errors.New("Kubernetes client and managed namespace are required")
	}
	if c.Mode == InCluster {
		system, err := client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read Kubernetes cluster identity: %w", err)
		}
		if string(system.UID) != c.ClusterUID {
			return errors.New("Kubernetes cluster UID does not match the pinned cluster identity")
		}
	} else {
		nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list local Kind nodes: %w", err)
		}
		if len(nodes.Items) == 0 {
			return errors.New("local Kind cluster has no nodes")
		}
		prefix := strings.TrimPrefix(c.Context, "kind-") + "-"
		for _, node := range nodes.Items {
			if !strings.HasPrefix(node.Name, prefix) {
				return errors.New("Kubernetes node does not belong to the expected Kind cluster")
			}
		}
	}
	target, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read managed Kubernetes namespace: %w", err)
	}
	if target.Labels[ManagedByLabel] != ManagedByValue {
		return errors.New("Kubernetes namespace is not marked as Orbit-DevOps-managed")
	}
	return nil
}
