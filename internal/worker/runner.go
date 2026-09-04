package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const maxFailureSummaryLength = 512

type Config struct {
	WorkerID         string
	LeaseDuration    time.Duration
	OperationTimeout time.Duration
	Logger           *slog.Logger
	Recorder         OperationRecorder
	Tracer           trace.Tracer
	Propagator       propagation.TextMapPropagator
}

type OperationRecorder interface {
	RecordOperation(status string, category string, duration time.Duration)
}

type PublishRequest struct {
	OperationID        uuid.UUID
	AttemptID          uuid.UUID
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
	return &FailureError{code: code, summary: summary, disposition: operation.NonRetryable}
}

// NewRetryableFailure 创建可由调度器自动重试的瞬时失败。
func NewRetryableFailure(code string, summary string) *FailureError {
	return &FailureError{code: code, summary: summary, disposition: operation.Retryable}
}

// NewUnknownOutcome 表示外部写入结果不能确认；retry 控制是否在预算内继续调和。
func NewUnknownOutcome(code string, summary string, retry bool) *FailureError {
	return &FailureError{
		code: code, summary: summary, disposition: operation.UnknownOutcome,
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
	config     Config
	operations *operation.Module
	releases   *delivery.Module
	publisher  Publisher
	logger     *slog.Logger
	recorder   OperationRecorder
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

type heartbeatResult struct {
	cancelRequested bool
	err             error
}

func New(
	config Config,
	operations *operation.Module,
	releases *delivery.Module,
	publisher Publisher,
) (*Runner, error) {
	if config.WorkerID == "" {
		return nil, errors.New("worker ID is required")
	}
	if config.LeaseDuration <= 0 {
		return nil, errors.New("lease duration must be positive")
	}
	if config.OperationTimeout <= 0 {
		return nil, errors.New("operation timeout must be positive")
	}
	if operations == nil {
		return nil, errors.New("operation module is required")
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
		tracer = otel.Tracer("github.com/HasonoCell/OrbitOps/internal/worker")
	}
	propagator := config.Propagator
	if propagator == nil {
		propagator = propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		)
	}

	return &Runner{
		config:     config,
		operations: operations,
		releases:   releases,
		publisher:  publisher,
		logger:     logger,
		recorder:   config.Recorder,
		tracer:     tracer,
		propagator: propagator,
	}, nil
}

func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	lease, claimed, err := r.operations.ClaimNext(ctx, operation.ClaimRequest{
		WorkerID:      r.config.WorkerID,
		LeaseDuration: r.config.LeaseDuration,
	})
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}
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
			attribute.String("orbitops.operation.id", lease.OperationID.String()),
			attribute.String("orbitops.attempt.id", lease.AttemptID.String()),
			attribute.Int("orbitops.attempt.number", lease.AttemptNumber),
			attribute.String("orbitops.release.id", lease.ReleaseID.String()),
		),
	)
	defer span.End()
	r.logger.InfoContext(attemptContext, "Worker 已领取发布操作",
		"operation_id", lease.OperationID,
		"attempt_id", lease.AttemptID,
		"attempt_number", lease.AttemptNumber,
		"release_id", lease.ReleaseID,
		"worker_id", lease.WorkerID,
		"trace_id", span.SpanContext().TraceID(),
	)

	release, err := r.releases.GetRelease(attemptContext, lease.ReleaseID)
	if err != nil {
		failure := operation.Failure{
			Code:        "release_load_failed",
			Summary:     "accepted release could not be loaded for delivery",
			Disposition: operation.NonRetryable,
		}
		if _, completionErr := r.operations.Fail(attemptContext, lease, failure); completionErr != nil {
			return true, errors.Join(err, completionErr)
		}
		r.recordTerminal(operation.StatusFailed, failure.Code, attemptStartedAt)
		span.RecordError(err)
		span.SetStatus(codes.Error, failure.Code)
		return true, fmt.Errorf("load claimed release: %w", err)
	}

	request := PublishRequest{
		OperationID:        lease.OperationID,
		AttemptID:          lease.AttemptID,
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
		"operation_id", lease.OperationID,
		"attempt_id", lease.AttemptID,
		"release_id", release.ID,
		"deployment_target_id", release.DeploymentTargetID,
		"namespace", release.TargetSnapshot.Namespace,
	)

	executionContext, cancelExecution := context.WithTimeout(attemptContext, r.config.OperationTimeout)
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

	publishErr := r.executeDelivery(executionContext, lease, request)
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
		return true, r.finishCancellation(attemptContext, lease, release.ID, attemptStartedAt)
	}

	if publishErr == nil && executionErr == nil {
		if err := r.operations.Succeed(attemptContext, lease); err != nil {
			if errors.Is(err, operation.ErrLeaseLost) {
				if cancelErr := r.finishCancellation(
					attemptContext,
					lease,
					release.ID,
					attemptStartedAt,
				); cancelErr == nil {
					return true, nil
				}
			}
			return true, err
		}
		r.recordTerminal(operation.StatusSucceeded, "none", attemptStartedAt)
		r.logger.InfoContext(attemptContext, "发布操作成功",
			"operation_id", lease.OperationID,
			"attempt_id", lease.AttemptID,
			"release_id", release.ID,
			"deployment_target_id", release.DeploymentTargetID,
		)
		return true, nil
	}

	failure := classifyFailure(publishErr, executionErr)
	if failure.Disposition == operation.UnknownOutcome {
		failureResult, err := r.operations.HandleUnknownOutcome(
			attemptContext,
			lease,
			failure,
			failure.RetryRecommended,
		)
		if err != nil {
			if errors.Is(err, operation.ErrLeaseLost) {
				if cancelErr := r.finishCancellation(
					attemptContext,
					lease,
					release.ID,
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
				"operation_id", lease.OperationID,
				"attempt_id", lease.AttemptID,
				"error_code", failure.Code,
				"available_at", failureResult.AvailableAt,
			)
			return true, nil
		}
		r.logger.ErrorContext(attemptContext, "发布结果未知，需要人工处理",
			"operation_id", lease.OperationID,
			"attempt_id", lease.AttemptID,
			"error_code", failure.Code,
			"error_summary", failure.Summary,
		)
		return true, nil
	}
	failureResult, err := r.operations.Fail(attemptContext, lease, failure)
	if err != nil {
		if errors.Is(err, operation.ErrLeaseLost) {
			if cancelErr := r.finishCancellation(
				attemptContext,
				lease,
				release.ID,
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
			"operation_id", lease.OperationID,
			"attempt_id", lease.AttemptID,
			"release_id", release.ID,
			"deployment_target_id", release.DeploymentTargetID,
			"error_code", failure.Code,
			"error_summary", failure.Summary,
			"available_at", failureResult.AvailableAt,
		)
		return true, nil
	}
	r.recordTerminal(operation.StatusFailed, failure.Code, attemptStartedAt)
	r.logger.WarnContext(attemptContext, "发布操作失败",
		"operation_id", lease.OperationID,
		"attempt_id", lease.AttemptID,
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
	lease operation.Lease,
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

func (r *Runner) recordTerminal(status string, category string, startedAt time.Time) {
	if r.recorder != nil {
		r.recorder.RecordOperation(status, category, time.Since(startedAt))
	}
}

func (r *Runner) finishCancellation(
	ctx context.Context,
	lease operation.Lease,
	releaseID uuid.UUID,
	startedAt time.Time,
) error {
	if err := r.operations.ConfirmCanceled(ctx, lease); err != nil {
		return err
	}
	r.recordTerminal(operation.StatusCanceled, "none", startedAt)
	r.logger.InfoContext(ctx, "发布操作已响应取消请求",
		"operation_id", lease.OperationID,
		"attempt_id", lease.AttemptID,
		"release_id", releaseID,
	)
	return nil
}

func (r *Runner) maintainLease(ctx context.Context, lease operation.Lease) (bool, error) {
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
			renewal, err := r.operations.Renew(ctx, lease, r.config.LeaseDuration)
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

func classifyFailure(publishErr error, executionErr error) operation.Failure {
	var failure *FailureError
	if errors.As(publishErr, &failure) && failure.code != "" && failure.summary != "" {
		return operation.Failure{
			Code:             safeText(failure.code, 64),
			Summary:          safeText(failure.summary, maxFailureSummaryLength),
			Disposition:      failure.disposition,
			RetryRecommended: failure.retryRecommended,
		}
	}

	if errors.Is(executionErr, context.DeadlineExceeded) ||
		errors.Is(publishErr, context.DeadlineExceeded) {
		return operation.Failure{
			Code:        "rollout_timeout",
			Summary:     "delivery did not reach a terminal result before the operation timeout",
			Disposition: operation.NonRetryable,
		}
	}

	return operation.Failure{
		Code:        "delivery_failed",
		Summary:     "delivery publisher returned an unexpected error",
		Disposition: operation.NonRetryable,
	}
}

func safeText(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
