// Package releaseworker 执行 ReleaseOperation，并把每次实际执行记录为 ReleaseAttempt。
// 它只通过 releaseoperation 模块取得业务执行权，不把队列消息视为执行授权。
package releaseworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const maxFailureSummaryLength = 512

type Config struct {
	WorkerID                string
	LeaseDuration           time.Duration
	ReleaseOperationTimeout time.Duration
	Logger                  *slog.Logger
	Recorder                ReleaseOperationRecorder
	Tracer                  trace.Tracer
	Propagator              propagation.TextMapPropagator
	DeliveryHook            DeliveryHook
}

type DeliveryCheckpoint string

const (
	DeliveryBeforePublish DeliveryCheckpoint = "before_publish"
	DeliveryBeforeCommit  DeliveryCheckpoint = "before_commit"
)

// DeliveryHook 暴露外部交付前与数据库提交前的进程边界，供故障注入和运行时观测使用。
type DeliveryHook func(DeliveryCheckpoint, PublishRequest)

type ReleaseOperationRecorder interface {
	RecordReleaseOperation(status string, category string, duration time.Duration)
}

// ReleaseOperationPhaseRecorder 可选地记录 Worker 各固定执行阶段耗时。
type ReleaseOperationPhaseRecorder interface {
	RecordReleaseOperationPhase(phase string, duration time.Duration)
}

type PublishRequest struct {
	ReleaseOperationID uuid.UUID
	ReleaseAttemptID   uuid.UUID
	ReleaseID          uuid.UUID
	ProjectID          uuid.UUID
	ApplicationID      uuid.UUID
	DeploymentTargetID uuid.UUID
	ImageReference     string
	Stage              string
	ClusterRef         string
	Namespace          string
	Replicas           int
	ContainerPort      int
}

type Publisher interface {
	Publish(ctx context.Context, request PublishRequest) error
}

type RecoveryAction string

const (
	RecoverySucceeded       RecoveryAction = "succeeded"
	RecoveryObserve         RecoveryAction = "observe"
	RecoveryApply           RecoveryAction = "apply"
	RecoveryReleaseObserved RecoveryAction = "release_observed"
	RecoveryAttention       RecoveryAction = "attention_required"
)

type RecoveryObservation struct {
	Action            RecoveryAction
	ObservedReleaseID *uuid.UUID
	ErrorCode         string
	ErrorSummary      string
}

// RecoveryPublisher 在任何恢复写入前给出 Kubernetes 权威事实，并支持纯观察已有 Rollout。
type RecoveryPublisher interface {
	InspectRecovery(ctx context.Context, request PublishRequest) (RecoveryObservation, error)
	ObserveRecovery(ctx context.Context, request PublishRequest) error
}

type FailureError struct {
	code             string
	summary          string
	disposition      string
	retryRecommended bool
}

// NewFailure 创建不可自动重试的稳定失败，适用于配置、资源或所有权问题。
func NewFailure(code string, summary string) *FailureError {
	return &FailureError{code: code, summary: summary, disposition: releaseoperation.NonRetryable}
}

// NewRetryableFailure 创建可由调度器自动重试的瞬时失败。
func NewRetryableFailure(code string, summary string) *FailureError {
	return &FailureError{code: code, summary: summary, disposition: releaseoperation.Retryable}
}

// NewUnknownOutcome 表示外部写入结果不能确认；retry 控制是否在预算内继续调和。
func NewUnknownOutcome(code string, summary string, retry bool) *FailureError {
	return &FailureError{
		code: code, summary: summary, disposition: releaseoperation.UnknownOutcome,
		retryRecommended: retry,
	}
}

func (e *FailureError) Error() string {
	return e.summary
}

func (e *FailureError) Code() string {
	return e.code
}

func (e *FailureError) Summary() string {
	return e.summary
}

func (e *FailureError) Disposition() string {
	return e.disposition
}

func (e *FailureError) RetryRecommended() bool {
	return e.retryRecommended
}

type Runner struct {
	config            Config
	releaseOperations *releaseoperation.Module
	releases          *delivery.Module
	publisher         Publisher
	logger            *slog.Logger
	recorder          ReleaseOperationRecorder
	tracer            trace.Tracer
	propagator        propagation.TextMapPropagator
}

type heartbeatResult struct {
	cancelRequested bool
	err             error
}

func New(
	config Config,
	releaseOperations *releaseoperation.Module,
	releases *delivery.Module,
	publisher Publisher,
) (*Runner, error) {
	if config.WorkerID == "" {
		return nil, errors.New("release worker ID is required")
	}
	if config.LeaseDuration <= 0 {
		return nil, errors.New("lease duration must be positive")
	}
	if config.ReleaseOperationTimeout <= 0 {
		return nil, errors.New("release operation timeout must be positive")
	}
	if releaseOperations == nil {
		return nil, errors.New("release operation module is required")
	}
	if releases == nil {
		return nil, errors.New("delivery module is required")
	}
	if publisher == nil {
		return nil, errors.New("publisher is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	tracer := config.Tracer
	if tracer == nil {
		tracer = otel.Tracer("github.com/HasonoCell/OrbitOps/internal/releaseworker")
	}
	propagator := config.Propagator
	if propagator == nil {
		propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)
	}

	return &Runner{
		config:            config,
		releaseOperations: releaseOperations,
		releases:          releases,
		publisher:         publisher,
		logger:            logger,
		recorder:          config.Recorder,
		tracer:            tracer,
		propagator:        propagator,
	}, nil
}

func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	claimStartedAt := time.Now()
	lease, claimed, err := r.releaseOperations.ClaimNext(ctx, releaseoperation.ClaimRequest{
		WorkerID:      r.config.WorkerID,
		LeaseDuration: r.config.LeaseDuration,
	})
	r.recordPhase("claim", claimStartedAt)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
	return r.runLease(ctx, lease)
}

// RunDispatch 是队列的唯一业务执行入口，只处理消息指定的有效意图，不扫描其他任务。
func (r *Runner) RunDispatch(ctx context.Context, ref releaseoperation.DispatchRef) (releaseoperation.ClaimOutcome, error) {
	started := time.Now()
	claimContext, cancelClaim := context.WithTimeout(ctx, 5*time.Second)
	claim, err := r.releaseOperations.ClaimDispatch(claimContext, ref, releaseoperation.ClaimRequest{
		WorkerID: r.config.WorkerID, LeaseDuration: r.config.LeaseDuration,
	})
	cancelClaim()
	r.recordPhase("claim", started)
	if err != nil {
		return "", err
	}
	if claim.Outcome != releaseoperation.ClaimOutcomeClaimed {
		return claim.Outcome, nil
	}
	_, err = r.runLease(ctx, claim.Lease)
	return claim.Outcome, err
}

// runLease 复用已验证的续期、取消与读后写恢复；仅在业务领取事务提交后调用。
func (r *Runner) runLease(ctx context.Context, lease releaseoperation.Lease) (bool, error) {
	attemptStartedAt := time.Now()
	carrier := propagation.MapCarrier{}
	if lease.TraceParent != "" {
		carrier.Set("traceparent", lease.TraceParent)
	}
	if lease.TraceState != "" {
		carrier.Set("tracestate", lease.TraceState)
	}
	attemptContext := r.propagator.Extract(ctx, carrier)
	attemptContext, span := r.tracer.Start(
		attemptContext,
		"release delivery attempt",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("orbitops.release_operation.id", lease.ReleaseOperationID.String()),
			attribute.String("orbitops.release_attempt.id", lease.ReleaseAttemptID.String()),
			attribute.Int("orbitops.release_attempt.number", lease.ReleaseAttemptNumber),
			attribute.String("orbitops.release.id", lease.ReleaseID.String()),
		),
	)
	defer span.End()
	r.logger.InfoContext(attemptContext, "Worker 已领取发布操作",
		"release_operation_id", lease.ReleaseOperationID,
		"release_attempt_id", lease.ReleaseAttemptID,
		"release_attempt_number", lease.ReleaseAttemptNumber,
		"release_id", lease.ReleaseID,
		"release_worker_id", lease.WorkerID,
		"trace_id", span.SpanContext().TraceID(),
	)

	readContext, cancelRead := context.WithTimeout(attemptContext, min(r.config.LeaseDuration/3, 5*time.Second))
	release, err := r.releases.GetRelease(readContext, lease.ReleaseID)
	cancelRead()
	if err != nil {
		// 存储连接或读取超时不是发布失败，保留当前执行事实供租约恢复，交给运输层有限重投。
		if !errors.Is(err, delivery.ErrReleaseNotFound) {
			return true, err
		}
		failure := releaseoperation.Failure{
			Code:        "release_load_failed",
			Summary:     "accepted release could not be loaded for delivery",
			Disposition: releaseoperation.NonRetryable,
		}
		if _, completionErr := r.releaseOperations.Fail(attemptContext, lease, failure); completionErr != nil {
			return true, errors.Join(err, completionErr)
		}
		r.recordTerminal(releaseoperation.StatusFailed, failure.Code, attemptStartedAt)
		span.RecordError(errors.New(failure.Code))
		span.SetStatus(codes.Error, failure.Code)
		// 失败已经可靠落库，不再把业务失败返回为队列运输失败。
		return true, nil
	}

	request := PublishRequest{
		ReleaseOperationID: lease.ReleaseOperationID,
		ReleaseAttemptID:   lease.ReleaseAttemptID,
		ReleaseID:          release.ID,
		ProjectID:          release.TargetSnapshot.ProjectID,
		ApplicationID:      release.TargetSnapshot.ApplicationID,
		DeploymentTargetID: release.DeploymentTargetID,
		ImageReference:     release.ImageReference,
		Stage:              release.TargetSnapshot.Stage,
		ClusterRef:         release.TargetSnapshot.ClusterRef,
		Namespace:          release.TargetSnapshot.Namespace,
		Replicas:           release.TargetSnapshot.Replicas,
		ContainerPort:      release.TargetSnapshot.ContainerPort,
	}
	span.SetAttributes(
		attribute.String("orbitops.project.id", request.ProjectID.String()),
		attribute.String("orbitops.application.id", request.ApplicationID.String()),
		attribute.String("orbitops.deployment_target.id", request.DeploymentTargetID.String()),
	)
	r.logger.InfoContext(attemptContext, "Worker 开始发布 Kubernetes 资源",
		"release_operation_id", lease.ReleaseOperationID,
		"release_attempt_id", lease.ReleaseAttemptID,
		"release_id", release.ID,
		"deployment_target_id", release.DeploymentTargetID,
		"namespace", release.TargetSnapshot.Namespace,
	)
	if r.config.DeliveryHook != nil {
		r.config.DeliveryHook(DeliveryBeforePublish, request)
	}
	// 输入读取或进程调度可能消耗旧租约；外部调用前重新验证，避免等待期间失权后才开始 Apply。
	checkContext, cancelCheck := context.WithTimeout(attemptContext, min(r.config.LeaseDuration/3, 5*time.Second))
	renewal, err := r.releaseOperations.Renew(checkContext, lease, r.config.LeaseDuration)
	cancelCheck()
	if err != nil {
		return true, err
	}
	lease = renewal.Lease
	if renewal.CancelRequested {
		return true, r.finishCancellation(attemptContext, lease, release.ID, release.CreatedAt, attemptStartedAt)
	}

	executionContext, cancelExecution := context.WithTimeout(attemptContext, r.config.ReleaseOperationTimeout)
	heartbeatContext, stopHeartbeat := context.WithCancel(attemptContext)
	heartbeatDone := make(chan heartbeatResult, 1)
	go func() {
		cancelRequested, heartbeatErr := r.maintainLease(heartbeatContext, lease)
		if heartbeatErr != nil || cancelRequested {
			cancelExecution()
		}
		heartbeatDone <- heartbeatResult{
			cancelRequested: cancelRequested,
			err:             heartbeatErr,
		}
	}()

	deliveryStartedAt := time.Now()
	publishErr := r.executeDelivery(executionContext, lease, request)
	r.recordPhase("execution", deliveryStartedAt)
	if lease.Recovery {
		r.recordPhase("recovery", deliveryStartedAt)
	}
	if publishErr == nil && r.config.DeliveryHook != nil {
		// 此处外部系统已经确认交付完成，但 ReleaseOperation 终态尚未提交，是必须显式验证的崩溃窗口。
		r.config.DeliveryHook(DeliveryBeforeCommit, request)
	}
	executionErr := executionContext.Err()
	cancelExecution()
	stopHeartbeat()
	heartbeat := <-heartbeatDone
	if heartbeat.err != nil {
		return true, heartbeat.err
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if heartbeat.cancelRequested {
		return true, r.finishCancellation(
			attemptContext, lease, release.ID, release.CreatedAt, attemptStartedAt,
		)
	}

	if publishErr == nil && executionErr == nil {
		if err := r.releaseOperations.Succeed(attemptContext, lease); err != nil {
			if errors.Is(err, releaseoperation.ErrLeaseLost) {
				if cancelErr := r.finishCancellation(
					attemptContext,
					lease,
					release.ID,
					release.CreatedAt,
					attemptStartedAt,
				); cancelErr == nil {
					return true, nil
				}
			}
			return true, err
		}
		r.recordTerminal(releaseoperation.StatusSucceeded, "none", attemptStartedAt)
		r.recordPhase("end_to_end", release.CreatedAt)
		r.logger.InfoContext(attemptContext, "发布操作成功",
			"release_operation_id", lease.ReleaseOperationID,
			"release_attempt_id", lease.ReleaseAttemptID,
			"release_id", release.ID,
			"deployment_target_id", release.DeploymentTargetID,
		)
		return true, nil
	}

	failure := classifyFailure(publishErr, executionErr)
	if failure.Disposition == releaseoperation.UnknownOutcome {
		failureResult, err := r.releaseOperations.HandleUnknownOutcome(
			attemptContext,
			lease,
			failure,
			failure.RetryRecommended,
		)
		if err != nil {
			if errors.Is(err, releaseoperation.ErrLeaseLost) {
				if cancelErr := r.finishCancellation(
					attemptContext,
					lease,
					release.ID,
					release.CreatedAt,
					attemptStartedAt,
				); cancelErr == nil {
					return true, nil
				}
			}
			return true, err
		}
		if publishErr != nil {
			span.RecordError(publishErr)
		}
		span.SetStatus(codes.Error, failure.Code)
		if failureResult.RetryScheduled {
			r.logger.WarnContext(attemptContext, "发布结果未知，已安排调和重试",
				"release_operation_id", lease.ReleaseOperationID,
				"release_attempt_id", lease.ReleaseAttemptID,
				"error_code", failure.Code,
				"available_at", failureResult.AvailableAt,
			)
			return true, nil
		}
		r.logger.ErrorContext(attemptContext, "发布结果未知，需要人工处理",
			"release_operation_id", lease.ReleaseOperationID,
			"release_attempt_id", lease.ReleaseAttemptID,
			"error_code", failure.Code,
			"error_summary", failure.Summary,
		)
		return true, nil
	}
	failureResult, err := r.releaseOperations.Fail(attemptContext, lease, failure)
	if err != nil {
		if errors.Is(err, releaseoperation.ErrLeaseLost) {
			if cancelErr := r.finishCancellation(
				attemptContext,
				lease,
				release.ID,
				release.CreatedAt,
				attemptStartedAt,
			); cancelErr == nil {
				return true, nil
			}
		}
		return true, err
	}
	if publishErr != nil {
		span.RecordError(publishErr)
	}
	span.SetStatus(codes.Error, failure.Code)
	if failureResult.RetryScheduled {
		r.logger.WarnContext(attemptContext, "发布操作失败，已安排自动重试",
			"release_operation_id", lease.ReleaseOperationID,
			"release_attempt_id", lease.ReleaseAttemptID,
			"release_id", release.ID,
			"deployment_target_id", release.DeploymentTargetID,
			"error_code", failure.Code,
			"error_summary", failure.Summary,
			"available_at", failureResult.AvailableAt,
		)
		return true, nil
	}
	r.recordTerminal(releaseoperation.StatusFailed, failure.Code, attemptStartedAt)
	r.recordPhase("end_to_end", release.CreatedAt)
	r.logger.WarnContext(attemptContext, "发布操作失败",
		"release_operation_id", lease.ReleaseOperationID,
		"release_attempt_id", lease.ReleaseAttemptID,
		"release_id", release.ID,
		"deployment_target_id", release.DeploymentTargetID,
		"error_code", failure.Code,
		"error_summary", failure.Summary,
	)
	return true, nil
}

// executeDelivery 将租约接管与普通发布分流；恢复路径没有权威读结论前不会调用 Apply。
func (r *Runner) executeDelivery(
	ctx context.Context,
	lease releaseoperation.Lease,
	request PublishRequest,
) error {
	if !lease.Recovery {
		return r.publisher.Publish(ctx, request)
	}
	recoveryPublisher, ok := r.publisher.(RecoveryPublisher)
	if !ok {
		return NewUnknownOutcome(
			"recovery_inspection_unavailable",
			"delivery publisher cannot inspect external state before recovery",
			false,
		)
	}
	observation, err := recoveryPublisher.InspectRecovery(ctx, request)
	if err != nil {
		return err
	}
	switch observation.Action {
	case RecoverySucceeded:
		return nil
	case RecoveryObserve:
		return recoveryPublisher.ObserveRecovery(ctx, request)
	case RecoveryApply:
		return r.publisher.Publish(ctx, request)
	case RecoveryReleaseObserved:
		if observation.ObservedReleaseID == nil {
			return NewUnknownOutcome(
				"recovery_state_invalid",
				"recovery observation omitted the observed release identifier",
				false,
			)
		}
		known, err := r.releases.IsKnownReleaseForTarget(
			ctx,
			*observation.ObservedReleaseID,
			request.DeploymentTargetID,
		)
		if err != nil {
			return NewUnknownOutcome(
				"release_history_unavailable",
				"release history could not be checked during recovery",
				true,
			)
		}
		if known {
			return r.publisher.Publish(ctx, request)
		}
		return NewUnknownOutcome(
			"unexpected_release_observed",
			"Kubernetes resources reference a release outside this deployment target history",
			false,
		)
	case RecoveryAttention:
		code := observation.ErrorCode
		if code == "" {
			code = "recovery_state_conflict"
		}
		summary := observation.ErrorSummary
		if summary == "" {
			summary = "Kubernetes state cannot be safely reconciled with this release"
		}
		return NewUnknownOutcome(code, summary, false)
	default:
		return NewUnknownOutcome(
			"recovery_state_invalid",
			"delivery publisher returned an unsupported recovery decision",
			false,
		)
	}
}

func (r *Runner) recordTerminal(status releaseoperation.ReleaseOperationStatus, category string, startedAt time.Time) {
	if r.recorder != nil {
		r.recorder.RecordReleaseOperation(string(status), category, time.Since(startedAt))
	}
}

func (r *Runner) recordPhase(phase string, startedAt time.Time) {
	if recorder, ok := r.recorder.(ReleaseOperationPhaseRecorder); ok {
		recorder.RecordReleaseOperationPhase(phase, time.Since(startedAt))
	}
}

func (r *Runner) finishCancellation(
	ctx context.Context,
	lease releaseoperation.Lease,
	releaseID uuid.UUID,
	releaseCreatedAt time.Time,
	startedAt time.Time,
) error {
	if err := r.releaseOperations.ConfirmCanceled(ctx, lease); err != nil {
		return err
	}
	r.recordTerminal(releaseoperation.StatusCanceled, "none", startedAt)
	r.recordPhase("end_to_end", releaseCreatedAt)
	r.logger.InfoContext(ctx, "发布操作已响应取消请求",
		"release_operation_id", lease.ReleaseOperationID,
		"release_attempt_id", lease.ReleaseAttemptID,
		"release_id", releaseID,
	)
	return nil
}

func (r *Runner) maintainLease(ctx context.Context, lease releaseoperation.Lease) (bool, error) {
	interval := r.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, nil
		case <-ticker.C:
			// 续期失联必须在原租约到期前停止外部调用，不能等待队列的长任务截止时间。
			renewContext, cancelRenew := context.WithTimeout(ctx, interval)
			renewal, err := r.releaseOperations.Renew(renewContext, lease, r.config.LeaseDuration)
			cancelRenew()
			if err != nil {
				return false, fmt.Errorf("renew claimed operation: %w", err)
			}
			lease = renewal.Lease
			if renewal.CancelRequested {
				return true, nil
			}
		}
	}
}

func classifyFailure(publishErr error, executionErr error) releaseoperation.Failure {
	var failure *FailureError
	if errors.As(publishErr, &failure) && failure.code != "" && failure.summary != "" {
		return releaseoperation.Failure{
			Code:             safeText(failure.code, 64),
			Summary:          safeText(failure.summary, maxFailureSummaryLength),
			Disposition:      failure.disposition,
			RetryRecommended: failure.retryRecommended,
		}
	}

	if errors.Is(executionErr, context.DeadlineExceeded) ||
		errors.Is(publishErr, context.DeadlineExceeded) {
		return releaseoperation.Failure{
			Code:        "rollout_timeout",
			Summary:     "delivery did not reach a terminal result before the operation timeout",
			Disposition: releaseoperation.NonRetryable,
		}
	}

	return releaseoperation.Failure{
		Code:        "delivery_failed",
		Summary:     "delivery publisher returned an unexpected error",
		Disposition: releaseoperation.NonRetryable,
	}
}

func safeText(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
