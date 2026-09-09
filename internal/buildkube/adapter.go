package buildkube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var artifactDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Start 以 Attempt ID 作为幂等名称创建 Job；同名资源必须完整匹配归属和输入摘要。
func (a *Adapter) Start(ctx context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	want := a.RenderJob(execution)
	existing, err := a.client.BatchV1().Jobs(a.config.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
	if err == nil {
		return a.identityForOwnedJob(existing, want)
	}
	if !apierrors.IsNotFound(err) {
		return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
			"build_job_read_failed", "Kubernetes could not be read before creating the build job", true, err)
	}
	created, err := a.client.BatchV1().Jobs(a.config.Namespace).Create(ctx, want, metav1.CreateOptions{FieldManager: a.config.FieldManager})
	if apierrors.IsAlreadyExists(err) {
		existing, readErr := a.client.BatchV1().Jobs(a.config.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
		if readErr != nil {
			return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
				"build_job_create_unknown", "build job creation raced and ownership could not be confirmed", true, readErr)
		}
		return a.identityForOwnedJob(existing, want)
	}
	if err != nil {
		return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
			"build_job_create_unknown", "Kubernetes did not confirm build job creation", true, err)
	}
	if created.UID == "" {
		return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
			"build_job_uid_missing", "Kubernetes created a build job without a stable UID", false, nil)
	}
	return buildworker.ExecutionIdentity{Name: created.Name, UID: string(created.UID)}, nil
}

func (a *Adapter) identityForOwnedJob(existing, want *batchv1.Job) (buildworker.ExecutionIdentity, error) {
	for key, value := range want.Labels {
		if existing.Labels[key] != value {
			return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
				"build_job_ownership_conflict", "a Kubernetes Job with the build attempt name has different ownership", false, nil)
		}
	}
	if existing.Annotations[InputDigestAnnotation] != want.Annotations[InputDigestAnnotation] || existing.UID == "" {
		return buildworker.ExecutionIdentity{}, buildworker.NewUnknownOutcome(
			"build_job_ownership_conflict", "the existing Kubernetes Job does not match the accepted build input", false, nil)
	}
	return buildworker.ExecutionIdentity{Name: existing.Name, UID: string(existing.UID)}, nil
}

// Observe 只读取指定 Job UID、Pod 终态和平台 termination message，不信任普通日志中的成功文本。
func (a *Adapter) Observe(ctx context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	job, err := a.client.BatchV1().Jobs(a.config.Namespace).Get(ctx, identity.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseMissing, Identity: identity}, nil
	}
	if err != nil {
		return buildworker.ExecutionObservation{}, buildworker.NewUnknownOutcome(
			"build_job_read_failed", "Kubernetes build job state could not be read", true, err)
	}
	if identity.UID != "" && string(job.UID) != identity.UID {
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseUnknown, Identity: identity,
			ErrorCode: "build_job_ownership_conflict", ErrorSummary: "build job UID changed while it was being observed"}, nil
	}
	if identity.UID == "" {
		identity.UID = string(job.UID)
	}
	pods, err := a.client.CoreV1().Pods(a.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + identity.Name,
	})
	if err != nil {
		return buildworker.ExecutionObservation{}, buildworker.NewUnknownOutcome(
			"build_pod_read_failed", "Kubernetes build pod state could not be read", true, err)
	}
	logExcerpt, truncated := podTerminationExcerpt(pods.Items)
	if jobCondition(job, batchv1.JobComplete) != nil || jobCondition(job, batchv1.JobFailed) != nil {
		if buildLog, buildLogTruncated := a.readBuildLog(ctx, pods.Items); buildLog != "" {
			logExcerpt, truncated = combineLogs(logExcerpt, buildLog, truncated || buildLogTruncated)
		}
	}
	if condition := jobCondition(job, batchv1.JobComplete); condition != nil && condition.Status == corev1.ConditionTrue {
		result, ok := successfulResult(pods.Items)
		if !ok {
			return buildworker.ExecutionObservation{Phase: buildworker.PhaseUnknown, Identity: identity,
				ErrorCode: "build_result_invalid", ErrorSummary: "completed build job did not contain a valid platform result",
				LogExcerpt: logExcerpt, LogTruncated: truncated}, nil
		}
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseSucceeded, Identity: identity,
			Repository: result.Repository, Digest: result.Digest, LogExcerpt: logExcerpt, LogTruncated: truncated}, nil
	}
	if condition := jobCondition(job, batchv1.JobFailed); condition != nil && condition.Status == corev1.ConditionTrue {
		code, disposition := "dockerfile_build_failed", buildoperation.NonRetryable
		if condition.Reason == "DeadlineExceeded" {
			code = "build_timeout"
		} else if podWasEvicted(pods.Items) {
			code, disposition = "executor_evicted", buildoperation.Retryable
		} else if sourceFetchFailed(pods.Items) {
			code, disposition = "source_fetch_failed", buildoperation.Retryable
		} else if executorImagePullFailed(pods.Items) {
			code, disposition = "executor_image_pull_failed", buildoperation.Retryable
		} else if platformFailure, ok := failedResult(pods.Items); ok {
			code = platformFailure.ErrorCode
			if code == "registry_unavailable" {
				disposition = buildoperation.Retryable
			}
		}
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseFailed, Identity: identity,
			ErrorCode: code, ErrorSummary: stableFailureSummary(condition), Disposition: disposition,
			LogExcerpt: logExcerpt, LogTruncated: truncated}, nil
	}
	return buildworker.ExecutionObservation{Phase: buildworker.PhaseRunning, Identity: identity,
		LogExcerpt: logExcerpt, LogTruncated: truncated}, nil
}

// Cancel 使用已记录 UID 防止删除同名替代资源；删除被 API Server 接受后由 Worker 收束取消。
func (a *Adapter) Cancel(ctx context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	propagation := metav1.DeletePropagationForeground
	options := metav1.DeleteOptions{PropagationPolicy: &propagation}
	if identity.UID != "" {
		uid := types.UID(identity.UID)
		options.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	err := a.client.BatchV1().Jobs(a.config.Namespace).Delete(ctx, identity.Name, options)
	if err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return buildworker.ExecutionObservation{Phase: buildworker.PhaseUnknown, Identity: identity,
				ErrorCode: "build_job_ownership_conflict", ErrorSummary: "build job UID changed before cancellation"}, nil
		}
		return buildworker.ExecutionObservation{}, buildworker.NewUnknownOutcome(
			"build_cancel_unknown", "Kubernetes did not confirm build job cancellation", true, err)
	}
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()
	for {
		_, jobErr := a.client.BatchV1().Jobs(a.config.Namespace).Get(ctx, identity.Name, metav1.GetOptions{})
		pods, podErr := a.client.CoreV1().Pods(a.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + identity.Name})
		if apierrors.IsNotFound(jobErr) && podErr == nil && len(pods.Items) == 0 {
			return buildworker.ExecutionObservation{Phase: buildworker.PhaseCanceled, Identity: identity}, nil
		}
		if (jobErr != nil && !apierrors.IsNotFound(jobErr)) || podErr != nil {
			return buildworker.ExecutionObservation{}, buildworker.NewUnknownOutcome(
				"build_cancel_observation_failed", "Kubernetes could not confirm that the build job stopped", true, errors.Join(jobErr, podErr))
		}
		select {
		case <-ctx.Done():
			return buildworker.ExecutionObservation{}, buildworker.NewUnknownOutcome(
				"build_cancel_unknown", "build job cancellation did not reach a confirmed stopped state", false, ctx.Err())
		case <-ticker.C:
		}
	}
}

type buildResult struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

type buildFailureResult struct {
	ErrorCode string `json:"error_code"`
}

func successfulResult(pods []corev1.Pod) (buildResult, bool) {
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].CreationTimestamp.Time.Before(pods[j].CreationTimestamp.Time) })
	for index := len(pods) - 1; index >= 0; index-- {
		for _, status := range pods[index].Status.ContainerStatuses {
			if status.Name != buildContainerName || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0 {
				continue
			}
			var result buildResult
			if json.Unmarshal([]byte(status.State.Terminated.Message), &result) == nil &&
				strings.TrimSpace(result.Repository) != "" && artifactDigestPattern.MatchString(result.Digest) {
				return result, true
			}
		}
	}
	return buildResult{}, false
}

// failedResult 只接纳平台脚本允许产生的固定错误码，不把任意容器输出提升为领域状态。
func failedResult(pods []corev1.Pod) (buildFailureResult, bool) {
	allowed := map[string]bool{"dockerfile_build_failed": true, "registry_unavailable": true}
	for _, pod := range pods {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != buildContainerName || status.State.Terminated == nil || status.State.Terminated.ExitCode == 0 {
				continue
			}
			var result buildFailureResult
			if json.Unmarshal([]byte(status.State.Terminated.Message), &result) == nil && allowed[result.ErrorCode] {
				return result, true
			}
		}
	}
	return buildFailureResult{}, false
}

func jobCondition(job *batchv1.Job, conditionType batchv1.JobConditionType) *batchv1.JobCondition {
	for index := range job.Status.Conditions {
		if job.Status.Conditions[index].Type == conditionType {
			return &job.Status.Conditions[index]
		}
	}
	return nil
}

func podWasEvicted(pods []corev1.Pod) bool {
	for _, pod := range pods {
		if pod.Status.Reason == "Evicted" {
			return true
		}
	}
	return false
}

func sourceFetchFailed(pods []corev1.Pod) bool {
	for _, pod := range pods {
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name == "source" && status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
				return true
			}
		}
	}
	return false
}

func executorImagePullFailed(pods []corev1.Pod) bool {
	for _, pod := range pods {
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, status := range statuses {
			if status.State.Waiting != nil && (status.State.Waiting.Reason == "ErrImagePull" || status.State.Waiting.Reason == "ImagePullBackOff") {
				return true
			}
		}
	}
	return false
}

func stableFailureSummary(condition *batchv1.JobCondition) string {
	if condition.Reason == "DeadlineExceeded" {
		return "build job exceeded its server-controlled deadline"
	}
	return "build job finished without producing an image artifact"
}

// podTerminationExcerpt 仅保存终态诊断文本；普通日志通过受限读取接口补充，绝不参与成功判断。
func podTerminationExcerpt(pods []corev1.Pod) (string, bool) {
	parts := make([]string, 0)
	for _, pod := range pods {
		if pod.Status.Reason != "" || pod.Status.Message != "" {
			parts = append(parts, fmt.Sprintf("pod %s: %s %s", pod.Name, pod.Status.Reason, pod.Status.Message))
		}
	}
	value := strings.Join(parts, "\n")
	if len(value) <= 64*1024 {
		return value, false
	}
	return value[:32*1024] + "\n... truncated ...\n" + value[len(value)-32*1024:], true
}

func (a *Adapter) readBuildLog(ctx context.Context, pods []corev1.Pod) (string, bool) {
	if len(pods) == 0 {
		return "", false
	}
	sort.SliceStable(pods, func(i, j int) bool { return pods[i].CreationTimestamp.Time.Before(pods[j].CreationTimestamp.Time) })
	limit := int64(64*1024 + 1)
	stream, err := a.client.CoreV1().Pods(a.config.Namespace).GetLogs(pods[len(pods)-1].Name, &corev1.PodLogOptions{
		Container: buildContainerName, LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		return "", false
	}
	defer stream.Close()
	payload, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		return "", false
	}
	return diagnostics.SanitizeRuntimeLog(string(payload), 64*1024)
}

func combineLogs(diagnostic, buildLog string, truncated bool) (string, bool) {
	value := buildLog
	if diagnostic != "" {
		value += "\n" + diagnostic
	}
	sanitized, limited := diagnostics.SanitizeRuntimeLog(value, 64*1024)
	return sanitized, truncated || limited
}

var _ buildworker.Executor = (*Adapter)(nil)
