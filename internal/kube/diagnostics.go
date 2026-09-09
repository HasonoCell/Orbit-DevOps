package kube

import (
	"context"
	"sort"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	maximumDiagnosticPods    = 20
	maximumDiagnosticEvents  = 50
	maximumEvidenceTextRunes = 512
)

// ObserveRelease 读取一个 Release 所需的有界 Kubernetes 证据；各子查询失败不会抹掉已取得的证据。
func (a *Adapter) ObserveRelease(
	ctx context.Context,
	query diagnostics.RuntimeQuery,
) diagnostics.RuntimeObservation {
	observedAt := time.Now().UTC()
	if query.ClusterRef != a.config.ClusterRef || query.Namespace != a.config.Namespace {
		return unavailableDiagnosticObservation(observedAt, "target_boundary_violation")
	}

	workload := diagnostics.WorkloadObservation{
		Metadata: diagnosticMetadata(observedAt, diagnostics.ObservationComplete),
		Pods:     []diagnostics.PodEvidence{},
	}
	relatedUIDs := make(map[string]struct{})
	name := ResourceName(query.TargetID)

	deployment, err := a.client.AppsV1().Deployments(query.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err == nil {
		workload.Deployment = projectDeployment(deployment, query)
		if workload.Deployment.OwnershipMatches {
			relatedUIDs[string(deployment.UID)] = struct{}{}
		} else {
			addObservationError(&workload.Metadata, "ownership_conflict")
		}
	} else if !apierrors.IsNotFound(err) {
		a.recordReadFailure()
		addObservationError(&workload.Metadata, "kubernetes_unavailable")
	}

	service, err := a.client.CoreV1().Services(query.Namespace).Get(
		ctx,
		name,
		metav1.GetOptions{},
	)
	if err == nil {
		workload.Service = projectService(service, query)
		if workload.Service.OwnershipMatches {
			relatedUIDs[string(service.UID)] = struct{}{}
		} else {
			addObservationError(&workload.Metadata, "ownership_conflict")
		}
	} else if !apierrors.IsNotFound(err) {
		a.recordReadFailure()
		addObservationError(&workload.Metadata, "kubernetes_unavailable")
	}

	pods, err := a.client.CoreV1().Pods(query.Namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: TargetIDLabel + "=" + query.TargetID.String()},
	)
	if err != nil {
		a.recordReadFailure()
		addObservationError(&workload.Metadata, "kubernetes_unavailable")
	} else {
		for index := range pods.Items {
			pod := &pods.Items[index]
			if !matchesDiagnosticOwnership(pod.Labels, query) {
				addObservationError(&workload.Metadata, "ownership_conflict")
				continue
			}
			workload.Pods = append(workload.Pods, projectPod(pod))
			relatedUIDs[string(pod.UID)] = struct{}{}
		}
		orderAndLimitPods(&workload.Pods)
	}
	finalizeObservationStatus(&workload.Metadata)

	events := a.observeRelatedEvents(ctx, query, observedAt, relatedUIDs)
	return diagnostics.RuntimeObservation{Workload: workload, Events: events}
}

func (a *Adapter) observeRelatedEvents(
	ctx context.Context,
	query diagnostics.RuntimeQuery,
	observedAt time.Time,
	relatedUIDs map[string]struct{},
) diagnostics.EventObservation {
	observation := diagnostics.EventObservation{
		Metadata: diagnosticMetadata(observedAt, diagnostics.ObservationComplete),
		Items:    []diagnostics.EventEvidence{},
	}

	replicaSets, err := a.client.AppsV1().ReplicaSets(query.Namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: TargetIDLabel + "=" + query.TargetID.String()},
	)
	if err != nil {
		a.recordReadFailure()
		addObservationError(&observation.Metadata, "kubernetes_unavailable")
	} else {
		for index := range replicaSets.Items {
			replicaSet := &replicaSets.Items[index]
			if matchesDiagnosticOwnership(replicaSet.Labels, query) {
				relatedUIDs[string(replicaSet.UID)] = struct{}{}
			} else {
				addObservationError(&observation.Metadata, "ownership_conflict")
			}
		}
	}

	events, err := a.client.CoreV1().Events(query.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		a.recordReadFailure()
		addObservationError(&observation.Metadata, "kubernetes_unavailable")
		finalizeObservationStatus(&observation.Metadata)
		if len(observation.Items) == 0 {
			observation.Metadata.Status = diagnostics.ObservationUnavailable
		}
		return observation
	}
	for index := range events.Items {
		event := &events.Items[index]
		if _, related := relatedUIDs[string(event.InvolvedObject.UID)]; !related {
			continue
		}
		observation.Items = append(observation.Items, projectEvent(event))
	}
	orderAndLimitEvents(&observation.Items)
	finalizeObservationStatus(&observation.Metadata)
	return observation
}

func projectDeployment(
	deployment *appsv1.Deployment,
	query diagnostics.RuntimeQuery,
) *diagnostics.DeploymentEvidence {
	evidence := &diagnostics.DeploymentEvidence{
		Name:             deployment.Name,
		OwnershipMatches: matchesDiagnosticOwnership(deployment.Labels, query),
	}
	if !evidence.OwnershipMatches {
		return evidence
	}
	evidence.UID = string(deployment.UID)
	evidence.Generation = deployment.Generation
	evidence.ObservedGeneration = deployment.Status.ObservedGeneration
	evidence.DesiredReplicas = desiredReplicas(deployment)
	evidence.UpdatedReplicas = deployment.Status.UpdatedReplicas
	evidence.ReadyReplicas = deployment.Status.ReadyReplicas
	evidence.AvailableReplicas = deployment.Status.AvailableReplicas
	evidence.Conditions = make([]diagnostics.ConditionEvidence, 0, len(deployment.Status.Conditions))
	if releaseID, err := uuid.Parse(deployment.Labels[ReleaseIDLabel]); err == nil {
		evidence.ReleaseID = &releaseID
	}
	for _, condition := range deployment.Status.Conditions {
		evidence.Conditions = append(evidence.Conditions, diagnostics.ConditionEvidence{
			Type: string(condition.Type), Status: string(condition.Status), Reason: condition.Reason,
			Message: diagnostics.SanitizeEvidenceText(condition.Message, maximumEvidenceTextRunes),
		})
	}
	sort.Slice(evidence.Conditions, func(i, j int) bool {
		return evidence.Conditions[i].Type < evidence.Conditions[j].Type
	})
	return evidence
}

func projectService(service *corev1.Service, query diagnostics.RuntimeQuery) *diagnostics.ServiceEvidence {
	evidence := &diagnostics.ServiceEvidence{
		Name:             service.Name,
		OwnershipMatches: matchesDiagnosticOwnership(service.Labels, query),
	}
	if !evidence.OwnershipMatches {
		return evidence
	}
	evidence.UID = string(service.UID)
	evidence.Ports = make([]diagnostics.ServicePortEvidence, 0, len(service.Spec.Ports))
	for _, port := range service.Spec.Ports {
		evidence.Ports = append(evidence.Ports, diagnostics.ServicePortEvidence{
			Name: port.Name, Protocol: string(port.Protocol), Port: port.Port,
		})
	}
	sort.Slice(evidence.Ports, func(i, j int) bool {
		if evidence.Ports[i].Port != evidence.Ports[j].Port {
			return evidence.Ports[i].Port < evidence.Ports[j].Port
		}
		return evidence.Ports[i].Name < evidence.Ports[j].Name
	})
	return evidence
}

func projectPod(pod *corev1.Pod) diagnostics.PodEvidence {
	evidence := diagnostics.PodEvidence{
		Name:       pod.Name,
		UID:        string(pod.UID),
		CreatedAt:  pod.CreationTimestamp.Time,
		Phase:      string(pod.Status.Phase),
		Containers: make([]diagnostics.ContainerEvidence, 0, len(pod.Status.ContainerStatuses)),
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			evidence.Ready = true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		container := projectContainer(status)
		evidence.Containers = append(evidence.Containers, container)
		if evidence.Reason == "" && container.Reason != "" {
			evidence.Reason = container.Reason
		}
	}
	sort.Slice(evidence.Containers, func(i, j int) bool {
		return evidence.Containers[i].Name < evidence.Containers[j].Name
	})
	return evidence
}

func projectContainer(status corev1.ContainerStatus) diagnostics.ContainerEvidence {
	evidence := diagnostics.ContainerEvidence{
		Name: status.Name, Ready: status.Ready, RestartCount: status.RestartCount,
	}
	switch {
	case status.State.Waiting != nil:
		evidence.State = "waiting"
		evidence.Reason = status.State.Waiting.Reason
		evidence.Message = diagnostics.SanitizeEvidenceText(status.State.Waiting.Message, maximumEvidenceTextRunes)
	case status.State.Running != nil:
		evidence.State = "running"
		startedAt := status.State.Running.StartedAt.Time
		evidence.StartedAt = &startedAt
	case status.State.Terminated != nil:
		evidence.State = "terminated"
		evidence.Reason = status.State.Terminated.Reason
		evidence.Message = diagnostics.SanitizeEvidenceText(status.State.Terminated.Message, maximumEvidenceTextRunes)
		exitCode := status.State.Terminated.ExitCode
		evidence.ExitCode = &exitCode
		startedAt := status.State.Terminated.StartedAt.Time
		finishedAt := status.State.Terminated.FinishedAt.Time
		evidence.StartedAt = &startedAt
		evidence.FinishedAt = &finishedAt
	default:
		evidence.State = "unknown"
	}
	if status.LastTerminationState.Terminated != nil {
		terminated := status.LastTerminationState.Terminated
		evidence.PreviousTermination = &diagnostics.TerminationEvidence{
			Reason: terminated.Reason, ExitCode: terminated.ExitCode,
			Message:   diagnostics.SanitizeEvidenceText(terminated.Message, maximumEvidenceTextRunes),
			StartedAt: terminated.StartedAt.Time, FinishedAt: terminated.FinishedAt.Time,
		}
	}
	return evidence
}

func projectEvent(event *corev1.Event) diagnostics.EventEvidence {
	count := event.Count
	lastSeen := event.LastTimestamp.Time
	if event.Series != nil {
		count = event.Series.Count
		lastSeen = event.Series.LastObservedTime.Time
	} else if !event.EventTime.IsZero() {
		lastSeen = event.EventTime.Time
	}
	if lastSeen.IsZero() {
		lastSeen = event.CreationTimestamp.Time
	}
	firstSeen := event.FirstTimestamp.Time
	if firstSeen.IsZero() {
		firstSeen = event.CreationTimestamp.Time
	}
	return diagnostics.EventEvidence{
		UID: string(event.UID), Type: event.Type, Reason: event.Reason, Count: count,
		FirstSeen: firstSeen, LastSeen: lastSeen,
		ResourceKind: event.InvolvedObject.Kind, ResourceName: event.InvolvedObject.Name,
		ResourceUID: string(event.InvolvedObject.UID),
		Message:     diagnostics.SanitizeEvidenceText(event.Message, maximumEvidenceTextRunes),
	}
}

func matchesDiagnosticOwnership(labels map[string]string, query diagnostics.RuntimeQuery) bool {
	if labels[ManagedByLabel] != ManagedByValue || labels[TargetIDLabel] != query.TargetID.String() {
		return false
	}
	if query.ProjectID != uuid.Nil && labels[ProjectIDLabel] != query.ProjectID.String() {
		return false
	}
	if query.ApplicationID != uuid.Nil && labels[ApplicationIDLabel] != query.ApplicationID.String() {
		return false
	}
	return true
}

func orderAndLimitPods(pods *[]diagnostics.PodEvidence) {
	sort.Slice(*pods, func(i, j int) bool {
		left, right := (*pods)[i], (*pods)[j]
		if podAbnormal(left) != podAbnormal(right) {
			return podAbnormal(left)
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		return left.UID < right.UID
	})
	if len(*pods) > maximumDiagnosticPods {
		*pods = (*pods)[:maximumDiagnosticPods]
	}
}

func podAbnormal(pod diagnostics.PodEvidence) bool {
	if !pod.Ready || pod.Phase != string(corev1.PodRunning) {
		return true
	}
	for _, container := range pod.Containers {
		if container.State == "waiting" || container.State == "terminated" || container.RestartCount > 0 {
			return true
		}
	}
	return false
}

func orderAndLimitEvents(events *[]diagnostics.EventEvidence) {
	sort.Slice(*events, func(i, j int) bool {
		left, right := (*events)[i], (*events)[j]
		if !left.LastSeen.Equal(right.LastSeen) {
			return left.LastSeen.After(right.LastSeen)
		}
		return left.UID < right.UID
	})
	if len(*events) > maximumDiagnosticEvents {
		*events = (*events)[:maximumDiagnosticEvents]
	}
}

func diagnosticMetadata(observedAt time.Time, status diagnostics.ObservationStatus) diagnostics.ObservationMetadata {
	return diagnostics.ObservationMetadata{
		Source: SourceKubernetes, ObservedAt: observedAt, Status: status, ErrorCategories: []string{},
	}
}

func addObservationError(metadata *diagnostics.ObservationMetadata, category string) {
	for _, existing := range metadata.ErrorCategories {
		if existing == category {
			return
		}
	}
	metadata.ErrorCategories = append(metadata.ErrorCategories, category)
	sort.Strings(metadata.ErrorCategories)
}

func finalizeObservationStatus(metadata *diagnostics.ObservationMetadata) {
	if len(metadata.ErrorCategories) > 0 {
		metadata.Status = diagnostics.ObservationPartial
	}
}

func unavailableDiagnosticObservation(observedAt time.Time, category string) diagnostics.RuntimeObservation {
	metadata := diagnosticMetadata(observedAt, diagnostics.ObservationUnavailable)
	metadata.ErrorCategories = []string{category}
	return diagnostics.RuntimeObservation{
		Workload: diagnostics.WorkloadObservation{Metadata: metadata, Pods: []diagnostics.PodEvidence{}},
		Events:   diagnostics.EventObservation{Metadata: metadata, Items: []diagnostics.EventEvidence{}},
	}
}
