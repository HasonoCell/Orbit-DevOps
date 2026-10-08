package buildkube

import (
	"context"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/kubeconnection"
	"k8s.io/client-go/kubernetes"
)

func NewVerifiedLocalAdapter(ctx context.Context, path, expectedContext string, config Config) (*Adapter, error) {
	return NewVerifiedAdapter(ctx, kubeconnection.Config{KubeconfigPath: path, Context: expectedContext}, config)
}

// NewVerifiedAdapter 只在集群身份与构建 Namespace 核验成功后交付 Job 执行接口。
func NewVerifiedAdapter(ctx context.Context, connection kubeconnection.Config, config Config) (*Adapter, error) {
	restConfig, err := connection.Load()
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	if err := connection.Verify(ctx, client, config.Namespace); err != nil {
		return nil, err
	}
	return New(client, config)
}
