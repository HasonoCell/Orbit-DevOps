package kube

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/kubeconnection"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func NewVerifiedLocalAdapter(ctx context.Context, path, expectedContext string, config Config) (*Adapter, error) {
	return NewVerifiedAdapter(ctx, kubeconnection.Config{KubeconfigPath: path, Context: expectedContext}, config)
}

// NewVerifiedAdapter 在赋予写接口前核验部署者指定的集群身份和受管 Namespace；Worker 启动失败时不降级。
func NewVerifiedAdapter(ctx context.Context, connection kubeconnection.Config, config Config) (*Adapter, error) {
	adapter, restConfig, err := newConnectedAdapter(connection, config)
	if err != nil {
		return nil, err
	}
	if err := connection.Verify(ctx, adapter.client, config.Namespace); err != nil {
		return nil, err
	}
	adapter.restConfig = restConfig
	return adapter, nil
}

func newConnectedAdapter(connection kubeconnection.Config, config Config) (*Adapter, *rest.Config, error) {
	restConfig, err := connection.Load()
	if err != nil {
		return nil, nil, err
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := New(client, config)
	if err != nil {
		return nil, nil, err
	}
	return adapter, restConfig, nil
}

// DynamicClient 仅从在线身份检查成功的连接创建 Gateway API/cert-manager 客户端。
func (a *Adapter) DynamicClient() (dynamic.Interface, error) {
	if a.restConfig == nil {
		return nil, errors.New("dynamic client requires verified Kubernetes identity")
	}
	return dynamic.NewForConfig(a.restConfig)
}
