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

type FailureError struct {
	category string
	summary  string
}

func NewFailure(category string, summary string) *FailureError {
	return &FailureError{category: category, summary: summary}
}

func (e *FailureError) Error() string {
	return e.summary
}

func (e *FailureError) Category() string {
	return e.category
}

func (e *FailureError) Summary() string {
	return e.summary
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
			Category: "release_load_failed",
			Summary:  "accepted release could not be loaded for delivery",
		}
		if completionErr := r.operations.Fail(attemptContext, lease, failure); completionErr != nil {
			return true, errors.Join(err, completionErr)
		}
		r.recordTerminal(operation.StatusFailed, failure.Category, attemptStartedAt)
		span.RecordError(err)
		span.SetStatus(codes.Error, failure.Category)
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
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := r.maintainLease(heartbeatContext, lease)
		if heartbeatErr != nil {
			cancelExecution()
		}
		heartbeatDone <- heartbeatErr
	}()

	publishErr := r.publisher.Publish(executionContext, request)
	executionErr := executionContext.Err()
	cancelExecution()
	stopHeartbeat()
	heartbeatErr := <-heartbeatDone
	if heartbeatErr != nil {
		return true, heartbeatErr
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}

	if publishErr == nil && executionErr == nil {
		if err := r.operations.Succeed(attemptContext, lease); err != nil {
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
	if err := r.operations.Fail(attemptContext, lease, failure); err != nil {
		return true, err
	}
	r.recordTerminal(operation.StatusFailed, failure.Category, attemptStartedAt)
	if publishErr != nil {
		span.RecordError(publishErr)
	}
	span.SetStatus(codes.Error, failure.Category)
	r.logger.WarnContext(attemptContext, "发布操作失败",
		"operation_id", lease.OperationID,
		"attempt_id", lease.AttemptID,
		"release_id", release.ID,
		"deployment_target_id", release.DeploymentTargetID,
		"error_category", failure.Category,
		"error_summary", failure.Summary,
	)
	return true, nil
}

func (r *Runner) recordTerminal(status string, category string, startedAt time.Time) {
	if r.recorder != nil {
		r.recorder.RecordOperation(status, category, time.Since(startedAt))
	}
}

func (r *Runner) maintainLease(ctx context.Context, lease operation.Lease) error {
	interval := r.config.LeaseDuration / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var err error
			lease, err = r.operations.Renew(ctx, lease, r.config.LeaseDuration)
			if err != nil {
				return fmt.Errorf("renew claimed operation: %w", err)
			}
		}
	}
}

func classifyFailure(publishErr error, executionErr error) operation.Failure {
	if errors.Is(executionErr, context.DeadlineExceeded) ||
		errors.Is(publishErr, context.DeadlineExceeded) {
		return operation.Failure{
			Category: "rollout_timeout",
			Summary:  "delivery did not reach a terminal result before the operation timeout",
		}
	}

	var failure *FailureError
	if errors.As(publishErr, &failure) && failure.category != "" && failure.summary != "" {
		return operation.Failure{
			Category: safeText(failure.category, 64),
			Summary:  safeText(failure.summary, maxFailureSummaryLength),
		}
	}

	return operation.Failure{
		Category: "delivery_failed",
		Summary:  "delivery publisher returned an unexpected error",
	}
}

func safeText(value string, maximumLength int) string {
	runes := []rune(value)
	if len(runes) <= maximumLength {
		return value
	}
	return string(runes[:maximumLength])
}
