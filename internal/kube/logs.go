package kube

import (
	"context"
	"io"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReadReleaseLogs 在读取正文前重新验证边界、Pod 归属、Release 身份与 Container。
func (a *Adapter) ReadReleaseLogs(
	ctx context.Context,
	query diagnostics.ReleaseRuntimeLogQuery,
) (diagnostics.RuntimeLogResult, error) {
	if query.ClusterRef != a.config.ClusterRef || query.Namespace != a.config.Namespace {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrRuntimeLogSourceNotFound
	}
	if query.TailLines < 1 || query.TailLines > diagnostics.MaximumRuntimeLogTailLines {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrInvalidRuntimeLogQuery
	}

	pod, err := a.client.CoreV1().Pods(query.Namespace).Get(
		ctx,
		query.PodName,
		metav1.GetOptions{},
	)
	if apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrRuntimeLogSourceNotFound
	}
	if err != nil {
		a.recordReadFailure()
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrKubernetesUnavailable
	}
	if !matchesRuntimeLogOwnership(pod.Labels, query) || !podHasContainer(pod.Spec.Containers, query.Container) {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrRuntimeLogSourceNotFound
	}
	visible, err := a.runtimeLogPodVisible(ctx, query, string(pod.UID))
	if err != nil {
		return diagnostics.RuntimeLogResult{}, err
	}
	if !visible {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrRuntimeLogSourceNotFound
	}

	tailLines := int64(query.TailLines)
	stream, err := a.client.CoreV1().Pods(query.Namespace).GetLogs(
		query.PodName,
		&corev1.PodLogOptions{
			Container: query.Container,
			TailLines: &tailLines,
			Previous:  query.Previous,
			Follow:    false,
		},
	).Stream(ctx)
	if apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) {
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrRuntimeLogSourceNotFound
	}
	if err != nil {
		a.recordReadFailure()
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrKubernetesUnavailable
	}
	defer func() { _ = stream.Close() }()

	payload, err := io.ReadAll(io.LimitReader(stream, diagnostics.MaximumRuntimeLogBytes+1))
	if err != nil {
		a.recordReadFailure()
		return diagnostics.RuntimeLogResult{}, diagnostics.ErrKubernetesUnavailable
	}
	truncated := len(payload) > diagnostics.MaximumRuntimeLogBytes
	if truncated {
		payload = payload[:diagnostics.MaximumRuntimeLogBytes]
	}
	return diagnostics.RuntimeLogResult{
		Content: string(payload), ObservedAt: time.Now().UTC(), Truncated: truncated,
	}, nil
}

// runtimeLogPodVisible 复用诊断报告的归属、排序和上限，避免用已知 Pod 名称绕过可见证据集合。
func (a *Adapter) runtimeLogPodVisible(
	ctx context.Context,
	query diagnostics.ReleaseRuntimeLogQuery,
	podUID string,
) (bool, error) {
	pods, err := a.client.CoreV1().Pods(query.Namespace).List(
		ctx,
		metav1.ListOptions{LabelSelector: TargetIDLabel + "=" + query.TargetID.String()},
	)
	if err != nil {
		a.recordReadFailure()
		return false, diagnostics.ErrKubernetesUnavailable
	}
	projected := make([]diagnostics.PodEvidence, 0, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		if matchesDiagnosticOwnership(pod.Labels, diagnostics.TargetRuntimeQuery{
			ProjectID: query.ProjectID, ApplicationID: query.ApplicationID, TargetID: query.TargetID,
		}) {
			projected = append(projected, projectPod(pod))
		}
	}
	orderAndLimitPods(&projected)
	for _, pod := range projected {
		if pod.UID == podUID {
			return true, nil
		}
	}
	return false, nil
}

func matchesRuntimeLogOwnership(labels map[string]string, query diagnostics.ReleaseRuntimeLogQuery) bool {
	return matchesDiagnosticOwnership(labels, diagnostics.TargetRuntimeQuery{
		ProjectID: query.ProjectID, ApplicationID: query.ApplicationID,
		TargetID: query.TargetID,
	}) && labels[ReleaseIDLabel] == query.ReleaseID.String()
}

func podHasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}
