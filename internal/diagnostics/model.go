// Package diagnostics 组合发布控制面事实、运行时观测与确定性诊断信号。
package diagnostics

import (
	"context"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
)

const SourceKubernetes = "kubernetes"

// ObservationStatus 描述一次外部读取的完整程度，不代表工作负载健康状态。
type ObservationStatus string

const (
	ObservationComplete    ObservationStatus = "complete"
	ObservationPartial     ObservationStatus = "partial"
	ObservationUnavailable ObservationStatus = "unavailable"
)

// ObservationMetadata 让每段外部证据独立表达来源、读取时刻与失败类别。
type ObservationMetadata struct {
	Source          string
	ObservedAt      time.Time
	Status          ObservationStatus
	ErrorCategories []string
}

type DeploymentEvidence struct {
	Name               string
	UID                string
	OwnershipMatches   bool
	ReleaseID          *uuid.UUID
	Generation         int64
	ObservedGeneration int64
	DesiredReplicas    int32
	UpdatedReplicas    int32
	ReadyReplicas      int32
	AvailableReplicas  int32
	Conditions         []ConditionEvidence
}

type ServiceEvidence struct {
	Name             string
	UID              string
	OwnershipMatches bool
	Ports            []ServicePortEvidence
}

type ConditionEvidence struct {
	Type    string
	Status  string
	Reason  string
	Message string
}

type ServicePortEvidence struct {
	Name     string
	Protocol string
	Port     int32
}

type PodEvidence struct {
	Name       string
	UID        string
	CreatedAt  time.Time
	Phase      string
	Ready      bool
	Reason     string
	Containers []ContainerEvidence
}

type ContainerEvidence struct {
	Name                string
	Ready               bool
	RestartCount        int32
	State               string
	Reason              string
	ExitCode            *int32
	PreviousTermination *TerminationEvidence
	Message             string
	StartedAt           *time.Time
	FinishedAt          *time.Time
}

type TerminationEvidence struct {
	Reason     string
	ExitCode   int32
	Message    string
	StartedAt  time.Time
	FinishedAt time.Time
}

type WorkloadObservation struct {
	Metadata   ObservationMetadata
	Deployment *DeploymentEvidence
	Service    *ServiceEvidence
	Pods       []PodEvidence
}

type EventEvidence struct {
	UID          string
	Type         string
	Reason       string
	Count        int32
	FirstSeen    time.Time
	LastSeen     time.Time
	ResourceKind string
	ResourceName string
	ResourceUID  string
	Message      string
}

type EventObservation struct {
	Metadata ObservationMetadata
	Items    []EventEvidence
}

// RuntimeObservation 是 Kubernetes 读取 seam 的完整返回值；分区段状态允许部分成功。
type RuntimeObservation struct {
	Workload WorkloadObservation
	Events   EventObservation
}

type RuntimeQuery struct {
	ProjectID     uuid.UUID
	ApplicationID uuid.UUID
	TargetID      uuid.UUID
	ReleaseID     uuid.UUID
	ClusterRef    string
	Namespace     string
}

// RuntimeSource 隐藏 Kubernetes 查询顺序、关联与裁剪规则。
type RuntimeSource interface {
	ObserveRelease(context.Context, RuntimeQuery) RuntimeObservation
}

// RuntimeLogSource 只暴露经过资源归属校验的单次日志读取，不允许 Follow 或任意 Namespace 浏览。
type RuntimeLogSource interface {
	ReadRuntimeLogs(context.Context, RuntimeLogQuery) (RuntimeLogResult, error)
}

type RuntimeLogQuery struct {
	ProjectID     uuid.UUID
	ApplicationID uuid.UUID
	TargetID      uuid.UUID
	ReleaseID     uuid.UUID
	ClusterRef    string
	Namespace     string
	PodName       string
	Container     string
	TailLines     int
	Previous      bool
}

// RuntimeLogResult 是 Kubernetes Adapter 返回的临时原文；诊断模块负责最终脱敏与响应上限。
type RuntimeLogResult struct {
	Content    string
	ObservedAt time.Time
	Truncated  bool
}

type LogExcerpt struct {
	Source     string
	ObservedAt time.Time
	ProjectID  uuid.UUID
	ReleaseID  uuid.UUID
	PodName    string
	Container  string
	TailLines  int
	Previous   bool
	Content    string
	Truncated  bool
}

type GetRuntimeLogsQuery struct {
	ReleaseID uuid.UUID
	ActorID   string
	PodName   string
	Container string
	TailLines int
	Previous  bool
}

type RuntimeReleaseRelation string

const (
	RuntimeReleaseMatches   RuntimeReleaseRelation = "matches"
	RuntimeReleaseDifferent RuntimeReleaseRelation = "different"
	RuntimeReleaseAbsent    RuntimeReleaseRelation = "absent"
	RuntimeReleaseUnknown   RuntimeReleaseRelation = "unknown"
)

type TargetDifference struct {
	Field        string
	ReleaseValue string
	CurrentValue string
}

type SignalCode string

const (
	SignalOperationAttentionRequired    SignalCode = "operation_attention_required"
	SignalOperationFailed               SignalCode = "operation_failed"
	SignalRuntimeObservationUnavailable SignalCode = "runtime_observation_unavailable"
	SignalDeploymentMissing             SignalCode = "deployment_missing"
	SignalRuntimeReleaseDifferent       SignalCode = "runtime_release_different"
	SignalResourceOwnershipConflict     SignalCode = "resource_ownership_conflict"
	SignalRolloutIncomplete             SignalCode = "rollout_incomplete"
	SignalPodWaiting                    SignalCode = "pod_waiting"
	SignalPodRestarting                 SignalCode = "pod_restarting"
	SignalWarningEventObserved          SignalCode = "warning_event_observed"
)

type EvidenceReference struct {
	Source string
	Kind   string
	ID     string
}

type Signal struct {
	Code         SignalCode
	Severity     string
	Summary      string
	EvidenceRefs []EvidenceReference
}

// Report 是面向 HTTP、CLI 与未来 Agent 的稳定发布诊断入口。
type Report struct {
	Release                delivery.Release
	Operation              operation.Record
	TargetDifferences      []TargetDifference
	RuntimeReleaseRelation RuntimeReleaseRelation
	Workload               WorkloadObservation
	Events                 EventObservation
	Signals                []Signal
	GeneratedAt            time.Time
}

type GetReleaseReportQuery struct {
	ReleaseID uuid.UUID
	ActorID   string
}
