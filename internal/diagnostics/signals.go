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
	if !workload.Deployment.OwnershipMatches {
		return RuntimeReleaseUnknown
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
	if containsCategory(report.Workload.Metadata.ErrorCategories, "ownership_conflict") ||
		containsCategory(report.Events.Metadata.ErrorCategories, "ownership_conflict") {
		appendSignal(
			SignalResourceOwnershipConflict,
			"error",
			"发现不属于当前 OrbitOps 目标的 Kubernetes 资源",
			EvidenceReference{Source: SourceKubernetes, Kind: "Observation", ID: "ownership"},
		)
	}
	if deployment := report.Workload.Deployment; deployment != nil &&
		deployment.OwnershipMatches &&
		(deployment.ObservedGeneration < deployment.Generation ||
			deployment.UpdatedReplicas != deployment.DesiredReplicas ||
			deployment.ReadyReplicas != deployment.DesiredReplicas ||
			deployment.AvailableReplicas != deployment.DesiredReplicas) {
		appendSignal(
			SignalRolloutIncomplete,
			"warning",
			"Deployment 尚未完成期望 Rollout",
			EvidenceReference{Source: SourceKubernetes, Kind: "Deployment", ID: deployment.UID},
		)
	}
	for _, pod := range report.Workload.Pods {
		for _, container := range pod.Containers {
			resourceID := pod.UID + "/" + container.Name
			if container.State == "waiting" && container.Reason != "" {
				appendSignal(
					SignalPodWaiting,
					"warning",
					"Pod 容器正在等待："+container.Reason,
					EvidenceReference{Source: SourceKubernetes, Kind: "Container", ID: resourceID},
				)
			}
			if container.RestartCount > 0 {
				appendSignal(
					SignalPodRestarting,
					"warning",
					"Pod 容器发生过重启",
					EvidenceReference{Source: SourceKubernetes, Kind: "Container", ID: resourceID},
				)
			}
		}
	}
	for _, event := range report.Events.Items {
		if event.Type == "Warning" {
			appendSignal(
				SignalWarningEventObserved,
				"warning",
				"Kubernetes 记录了 Warning Event："+event.Reason,
				EvidenceReference{Source: SourceKubernetes, Kind: "Event", ID: event.UID},
			)
		}
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

func containsCategory(categories []string, category string) bool {
	for _, existing := range categories {
		if existing == category {
			return true
		}
	}
	return false
}

func firstReferenceID(signal Signal) string {
	if len(signal.EvidenceRefs) == 0 {
		return ""
	}
	return signal.EvidenceRefs[0].ID
}
