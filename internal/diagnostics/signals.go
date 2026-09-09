package diagnostics

import (
	"sort"

	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
)

func relateRuntimeRelease(releaseID uuid.UUID, workload WorkloadObservation) RuntimeReleaseRelation {
	if workload.Metadata.Status == ObservationUnavailable {
		return RuntimeReleaseUnknown
	}
	if workload.Deployment == nil {
		return RuntimeReleaseAbsent
	}
	if workload.Deployment.ReleaseID == nil {
		return RuntimeReleaseUnknown
	}
	if *workload.Deployment.ReleaseID == releaseID {
		return RuntimeReleaseMatches
	}
	return RuntimeReleaseDifferent
}

// deriveSignals 只根据可验证证据产生稳定信号，并固定排序以消除来源返回顺序的影响。
func deriveSignals(report Report) []Signal {
	signals := make([]Signal, 0)
	appendSignal := func(code SignalCode, severity string, summary string, refs ...EvidenceReference) {
		signals = append(signals, Signal{
			Code: code, Severity: severity, Summary: summary, EvidenceRefs: refs,
		})
	}

	switch report.Operation.Status {
	case operation.StatusAttentionRequired:
		appendSignal(
			SignalOperationAttentionRequired,
			"error",
			"发布操作需要人工处理",
			EvidenceReference{Source: "postgresql", Kind: "Operation", ID: report.Operation.ID.String()},
		)
	case operation.StatusFailed:
		appendSignal(
			SignalOperationFailed,
			"error",
			"发布操作执行失败",
			EvidenceReference{Source: "postgresql", Kind: "Operation", ID: report.Operation.ID.String()},
		)
	}

	if report.Workload.Metadata.Status == ObservationUnavailable {
		appendSignal(
			SignalRuntimeObservationUnavailable,
			"warning",
			"Kubernetes 工作负载观测不可用",
			EvidenceReference{Source: SourceKubernetes, Kind: "Observation", ID: "workload"},
		)
	} else if report.Workload.Deployment == nil {
		appendSignal(
			SignalDeploymentMissing,
			"error",
			"目标 Deployment 不存在",
			EvidenceReference{Source: SourceKubernetes, Kind: "Deployment", ID: ""},
		)
	}
	if report.RuntimeReleaseRelation == RuntimeReleaseDifferent {
		appendSignal(
			SignalRuntimeReleaseDifferent,
			"warning",
			"集群当前运行的是另一个 Release",
			EvidenceReference{Source: SourceKubernetes, Kind: "Deployment", ID: report.Workload.Deployment.UID},
		)
	}

	severityRank := map[string]int{"error": 0, "warning": 1, "info": 2}
	sort.Slice(signals, func(i, j int) bool {
		if severityRank[signals[i].Severity] != severityRank[signals[j].Severity] {
			return severityRank[signals[i].Severity] < severityRank[signals[j].Severity]
		}
		if signals[i].Code != signals[j].Code {
			return signals[i].Code < signals[j].Code
		}
		return firstReferenceID(signals[i]) < firstReferenceID(signals[j])
	})
	return signals
}

func firstReferenceID(signal Signal) string {
	if len(signal.EvidenceRefs) == 0 {
		return ""
	}
	return signal.EvidenceRefs[0].ID
}
