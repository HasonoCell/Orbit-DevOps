package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
)

// GetReleaseDiagnostics 将稳定诊断模块投影为 OpenAPI 契约，不在 Handler 重复组合领域规则。
func (s *Server) GetReleaseDiagnostics(
	ctx context.Context,
	request api.GetReleaseDiagnosticsRequestObject,
) (api.GetReleaseDiagnosticsResponseObject, error) {
	report, err := s.diagnostics.GetReleaseReport(
		httpRequestContext(ctx),
		diagnostics.GetReleaseReportQuery{
			ReleaseID: request.ReleaseId,
			ActorID:   s.localActorID,
		},
	)
	if err != nil {
		if errors.Is(err, diagnostics.ErrReleaseNotFound) {
			return api.GetReleaseDiagnostics404JSONResponse{
				Code: "release_not_found", Message: "release not found",
			}, nil
		}
		return nil, err
	}
	observability.SetRequestProjectID(ctx, report.Release.TargetSnapshot.ProjectID)
	return api.GetReleaseDiagnostics200JSONResponse(diagnosticReportResponse(report)), nil
}

// GetReleaseRuntimeLogs 只转换查询参数和稳定错误；资源归属、权限及正文安全由诊断模块负责。
func (s *Server) GetReleaseRuntimeLogs(
	ctx context.Context,
	request api.GetReleaseRuntimeLogsRequestObject,
) (api.GetReleaseRuntimeLogsResponseObject, error) {
	tailLines := diagnostics.DefaultRuntimeLogTailLines
	if request.Params.TailLines != nil {
		tailLines = *request.Params.TailLines
	}
	previous := false
	if request.Params.Previous != nil {
		previous = *request.Params.Previous
	}
	excerpt, err := s.diagnostics.GetRuntimeLogs(
		httpRequestContext(ctx),
		diagnostics.GetRuntimeLogsQuery{
			ReleaseID: request.ReleaseId, ActorID: s.localActorID,
			PodName: request.Params.PodName, Container: request.Params.Container,
			TailLines: tailLines, Previous: previous,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, diagnostics.ErrInvalidRuntimeLogQuery):
			return api.GetReleaseRuntimeLogs400JSONResponse{
				Code: "invalid_runtime_log_query", Message: "runtime log query is invalid",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.GetReleaseRuntimeLogs403JSONResponse{
				Code: "runtime_logs_forbidden", Message: "current project role cannot read runtime logs",
			}, nil
		case errors.Is(err, diagnostics.ErrReleaseNotFound):
			return api.GetReleaseRuntimeLogs404JSONResponse{
				Code: "release_not_found", Message: "release not found",
			}, nil
		case errors.Is(err, diagnostics.ErrRuntimeLogSourceNotFound):
			return api.GetReleaseRuntimeLogs404JSONResponse{
				Code: "runtime_log_source_not_found", Message: "runtime log source not found",
			}, nil
		case errors.Is(err, diagnostics.ErrKubernetesUnavailable):
			return api.GetReleaseRuntimeLogs503JSONResponse{
				Code: "kubernetes_unavailable", Message: "Kubernetes is unavailable",
			}, nil
		default:
			return nil, err
		}
	}
	observability.SetRequestProjectID(ctx, excerpt.ProjectID)
	return api.GetReleaseRuntimeLogs200JSONResponse{
		Source:     api.RuntimeLogExcerptSource(excerpt.Source),
		ObservedAt: excerpt.ObservedAt,
		ReleaseId:  excerpt.ReleaseID,
		PodName:    excerpt.PodName,
		Container:  excerpt.Container,
		TailLines:  excerpt.TailLines,
		Previous:   excerpt.Previous,
		Content:    excerpt.Content,
		Truncated:  excerpt.Truncated,
	}, nil
}

func diagnosticReportResponse(report diagnostics.Report) api.ReleaseDiagnosticReport {
	differences := make([]api.SnapshotDifference, 0, len(report.TargetDifferences))
	for _, difference := range report.TargetDifferences {
		differences = append(differences, api.SnapshotDifference{
			Field: difference.Field, ReleaseValue: difference.ReleaseValue,
			CurrentValue: difference.CurrentValue,
		})
	}
	signals := make([]api.DiagnosticSignal, 0, len(report.Signals))
	for _, signal := range report.Signals {
		refs := make([]api.DiagnosticEvidenceReference, 0, len(signal.EvidenceRefs))
		for _, ref := range signal.EvidenceRefs {
			refs = append(refs, api.DiagnosticEvidenceReference{
				Source: ref.Source, Kind: ref.Kind, Id: ref.ID,
			})
		}
		signals = append(signals, api.DiagnosticSignal{
			Code:         api.DiagnosticSignalCode(signal.Code),
			Severity:     api.DiagnosticSignalSeverity(signal.Severity),
			Summary:      signal.Summary,
			EvidenceRefs: refs,
		})
	}
	return api.ReleaseDiagnosticReport{
		Release:           releaseResponse(report.Release),
		Operation:         operationResponse(report.Operation),
		TargetDifferences: differences,
		RuntimeReleaseRelation: api.ReleaseDiagnosticReportRuntimeReleaseRelation(
			report.RuntimeReleaseRelation,
		),
		WorkloadObservation: workloadObservationResponse(report.Workload),
		EventObservation:    eventObservationResponse(report.Events),
		Signals:             signals,
		GeneratedAt:         report.GeneratedAt,
	}
}

func workloadObservationResponse(observation diagnostics.WorkloadObservation) api.WorkloadObservation {
	pods := make([]api.DiagnosticPod, 0, len(observation.Pods))
	for _, pod := range observation.Pods {
		containers := make([]api.DiagnosticContainer, 0, len(pod.Containers))
		for _, container := range pod.Containers {
			containers = append(containers, diagnosticContainerResponse(container))
		}
		pods = append(pods, api.DiagnosticPod{
			Name: pod.Name, Uid: pod.UID, CreatedAt: pod.CreatedAt,
			Phase: pod.Phase, Ready: pod.Ready, Reason: pod.Reason,
			Containers: containers,
		})
	}
	response := api.WorkloadObservation{
		Metadata: observationMetadataResponse(observation.Metadata),
		Pods:     pods,
	}
	if observation.Deployment != nil {
		response.Deployment = diagnosticDeploymentResponse(*observation.Deployment)
	}
	if observation.Service != nil {
		response.Service = diagnosticServiceResponse(*observation.Service)
	}
	return response
}

func diagnosticDeploymentResponse(deployment diagnostics.DeploymentEvidence) *api.DiagnosticDeployment {
	conditions := make([]api.DiagnosticCondition, 0, len(deployment.Conditions))
	for _, condition := range deployment.Conditions {
		conditions = append(conditions, api.DiagnosticCondition{
			Type: condition.Type, Status: condition.Status,
			Reason: condition.Reason, Message: condition.Message,
		})
	}
	return &api.DiagnosticDeployment{
		Name: deployment.Name, Uid: deployment.UID,
		OwnershipMatches: deployment.OwnershipMatches, ReleaseId: deployment.ReleaseID,
		Generation: deployment.Generation, ObservedGeneration: deployment.ObservedGeneration,
		DesiredReplicas:   int(deployment.DesiredReplicas),
		UpdatedReplicas:   int(deployment.UpdatedReplicas),
		ReadyReplicas:     int(deployment.ReadyReplicas),
		AvailableReplicas: int(deployment.AvailableReplicas),
		Conditions:        conditions,
	}
}

func diagnosticServiceResponse(service diagnostics.ServiceEvidence) *api.DiagnosticService {
	ports := make([]api.DiagnosticServicePort, 0, len(service.Ports))
	for _, port := range service.Ports {
		ports = append(ports, api.DiagnosticServicePort{
			Name: port.Name, Protocol: port.Protocol, Port: int(port.Port),
		})
	}
	return &api.DiagnosticService{
		Name: service.Name, Uid: service.UID,
		OwnershipMatches: service.OwnershipMatches, Ports: ports,
	}
}

func diagnosticContainerResponse(container diagnostics.ContainerEvidence) api.DiagnosticContainer {
	response := api.DiagnosticContainer{
		Name: container.Name, Ready: container.Ready,
		RestartCount: int(container.RestartCount),
		State:        api.DiagnosticContainerState(container.State),
		Reason:       container.Reason, Message: container.Message,
		StartedAt: container.StartedAt, FinishedAt: container.FinishedAt,
	}
	if container.ExitCode != nil {
		exitCode := int(*container.ExitCode)
		response.ExitCode = &exitCode
	}
	if container.PreviousTermination != nil {
		previous := container.PreviousTermination
		response.PreviousTermination = &api.DiagnosticTermination{
			Reason: previous.Reason, ExitCode: int(previous.ExitCode), Message: previous.Message,
			StartedAt: previous.StartedAt, FinishedAt: previous.FinishedAt,
		}
	}
	return response
}

func eventObservationResponse(observation diagnostics.EventObservation) api.EventObservation {
	items := make([]api.DiagnosticEvent, 0, len(observation.Items))
	for _, event := range observation.Items {
		items = append(items, api.DiagnosticEvent{
			Uid: event.UID, Type: event.Type, Reason: event.Reason, Count: int(event.Count),
			FirstSeen: event.FirstSeen, LastSeen: event.LastSeen,
			ResourceKind: event.ResourceKind, ResourceName: event.ResourceName,
			ResourceUid: event.ResourceUID, Message: event.Message,
		})
	}
	return api.EventObservation{
		Metadata: observationMetadataResponse(observation.Metadata),
		Items:    items,
	}
}

func observationMetadataResponse(metadata diagnostics.ObservationMetadata) api.ObservationMetadata {
	categories := append([]string(nil), metadata.ErrorCategories...)
	if categories == nil {
		categories = []string{}
	}
	return api.ObservationMetadata{
		Source:          api.ObservationMetadataSource(metadata.Source),
		ObservedAt:      metadata.ObservedAt,
		Status:          api.ObservationMetadataStatus(metadata.Status),
		ErrorCategories: categories,
	}
}
