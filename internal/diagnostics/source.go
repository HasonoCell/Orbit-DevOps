package diagnostics

import (
	"context"
	"time"
)

// UnavailableSource 保证尚未配置或无法连接 Kubernetes 时仍返回诚实的数据库诊断证据。
type UnavailableSource struct{}

func (UnavailableSource) ObserveRelease(context.Context, RuntimeQuery) RuntimeObservation {
	now := time.Now().UTC()
	metadata := ObservationMetadata{
		Source:          SourceKubernetes,
		ObservedAt:      now,
		Status:          ObservationUnavailable,
		ErrorCategories: []string{"kubernetes_unavailable"},
	}
	return RuntimeObservation{
		Workload: WorkloadObservation{Metadata: metadata},
		Events:   EventObservation{Metadata: metadata, Items: []EventEvidence{}},
	}
}

func (UnavailableSource) ReadRuntimeLogs(
	context.Context,
	RuntimeLogQuery,
) (RuntimeLogResult, error) {
	return RuntimeLogResult{}, ErrKubernetesUnavailable
}
