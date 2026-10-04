// Package buildworker 驱动一次 BuildOperation，并通过小型 Executor 接口隔离 Kubernetes 实现。
package buildworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Phase string

const (
	PhaseMissing   Phase = "missing"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
	PhaseCanceled  Phase = "canceled"
	PhaseUnknown   Phase = "unknown"
)

type BuildExecution struct {
	BuildOperationID      uuid.UUID
	BuildAttemptID        uuid.UUID
	BuildID               uuid.UUID
	ProjectID             uuid.UUID
	ApplicationID         uuid.UUID
	RepositoryURL         string
	SourceCommit          string
	DockerfilePath        string
	ContextPath           string
	Platform              string
	DestinationRepository string
	InputDigest           string
}

type ExecutionIdentity struct {
	Name string
	UID  string
}

type ExecutionObservation struct {
	Phase        Phase
	Identity     ExecutionIdentity
	Repository   string
	Digest       string
	ErrorCode    string
	ErrorSummary string
	Disposition  string
	LogExcerpt   string
	LogTruncated bool
}

// Executor 返回 Orbit-DevOps 构建语义，必须响应 Context 取消；Cancel 只接收 Start 或 Observe 已确认的身份。
type Executor interface {
	Start(context.Context, BuildExecution) (ExecutionIdentity, error)
	Observe(context.Context, BuildExecution, ExecutionIdentity) (ExecutionObservation, error)
	Cancel(context.Context, ExecutionIdentity) (ExecutionObservation, error)
}

// preparedExecution 同时保存当前 Attempt 的执行输入与实际外部 Job 的期望归属；恢复时两者的 Attempt ID 不同。
type preparedExecution struct {
	execution      BuildExecution
	identity       ExecutionIdentity
	observation    ExecutionObservation
	recordIdentity bool
}

type Config struct {
	Tracer        trace.Tracer
	Propagator    propagation.TextMapPropagator
	WorkerID      string
	LeaseDuration time.Duration
	BuildTimeout  time.Duration
	PollInterval  time.Duration
	Logger        *slog.Logger
}

type Runner struct {
	config     Config
	operations *buildoperation.Module
	builds     *build.Module
	executor   Executor
	logger     *slog.Logger
}

func New(config Config, operations *buildoperation.Module, builds *build.Module, executor Executor) (*Runner, error) {
	if strings.TrimSpace(config.WorkerID) == "" || config.LeaseDuration <= 0 || config.BuildTimeout <= 0 ||
		config.PollInterval <= 0 || operations == nil || builds == nil || executor == nil {
		return nil, errors.New("invalid build worker configuration")
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if config.Tracer == nil {
		config.Tracer = otel.Tracer("orbit-devops-build-worker")
	}
	if config.Propagator == nil {
		config.Propagator = propagation.TraceContext{}
	}
	return &Runner{config: config, operations: operations, builds: builds, executor: executor, logger: config.Logger}, nil
}

// RunDispatch 是构建队列唯一业务入口；只有 ClaimDispatch 成功后才接触外部 Executor。
func (r *Runner) RunDispatch(ctx context.Context, ref buildoperation.DispatchRef) (buildoperation.ClaimOutcome, error) {
	claimContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	claim, err := r.operations.ClaimDispatch(claimContext, ref, buildoperation.ClaimRequest{
		WorkerID: r.config.WorkerID, LeaseDuration: r.config.LeaseDuration,
	})
	cancel()
	if err != nil || claim.Outcome != buildoperation.ClaimOutcomeClaimed {
		return claim.Outcome, err
	}
	return claim.Outcome, r.runLease(ctx, claim.Lease)
}

func (r *Runner) runLease(ctx context.Context, lease buildoperation.Lease) (resultErr error) {
	ctx = observability.RestoreTrace(ctx, lease.TraceParent, lease.TraceState, r.config.Propagator)
	ctx, span := r.config.Tracer.Start(ctx, "build execution attempt", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("orbit-devops.build_operation.id", lease.BuildOperationID.String()),
			attribute.String("orbit-devops.build_attempt.id", lease.BuildAttemptID.String()),
			attribute.Int("orbit-devops.build_attempt.number", lease.BuildAttemptNumber)))
	defer func() {
		if resultErr != nil {
			span.SetStatus(codes.Error, "build_attempt_interrupted")
		}
		span.End()
	}()
	// 在任何输入读取或外部调用前确认执行权；心跳不与 Start、Observe、Cancel 共用调用栈。
	renewContext, cancelRenew := context.WithTimeout(ctx, r.renewInterval())
	renewal, err := r.operations.Renew(renewContext, lease, r.config.LeaseDuration)
	cancelRenew()
	if err != nil {
		return err
	}
	lease = renewal.Lease
	leaseContext, cancelLease := context.WithCancel(ctx)
	defer cancelLease()
	executionContext, cancelExecution := context.WithTimeout(leaseContext, r.config.BuildTimeout)
	defer cancelExecution()
	workContext, cancelWork := context.WithCancel(executionContext)
	defer cancelWork()
	var cancelRequested atomic.Bool
	cancelRequested.Store(lease.CancelRequested)
	heartbeatContext, stopHeartbeat := context.WithCancel(leaseContext)
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatErr := r.maintainLease(heartbeatContext, lease, func() {
			cancelRequested.Store(true)
			cancelWork()
		})
		if heartbeatErr != nil {
			// 失权立即中断外部调用；业务取消只中断观察，不中断后续安全删除及其心跳。
			cancelLease()
		}
		heartbeatDone <- heartbeatErr
	}()
	defer func() {
		stopHeartbeat()
		heartbeatErr := <-heartbeatDone
		// 已提交的终态是权威结果；其后并发 Renew 被围栏拒绝不应把成功变成失败。
		if resultErr != nil && heartbeatErr != nil {
			resultErr = heartbeatErr
		}
	}()

	record, err := r.builds.GetForExecution(leaseContext, lease.BuildID)
	if err != nil {
		if errors.Is(err, build.ErrNotFound) {
			_, completionErr := r.operations.Fail(ctx, lease, buildoperation.Failure{
				Code: "build_load_failed", Summary: "accepted build could not be loaded", Disposition: buildoperation.NonRetryable,
			})
			return completionErr
		}
		return err
	}
	execution := BuildExecution{
		BuildOperationID: lease.BuildOperationID, BuildAttemptID: lease.BuildAttemptID, BuildID: record.ID,
		ProjectID: record.ProjectID, ApplicationID: record.ApplicationID, RepositoryURL: record.RepositoryURL,
		SourceCommit: record.SourceCommit, DockerfilePath: record.DockerfilePath, ContextPath: record.ContextPath,
		Platform: record.Platform, DestinationRepository: record.DestinationRepository, InputDigest: record.InputDigest,
	}
	prepareContext := workContext
	if lease.CancelRequested {
		prepareContext = executionContext
	}
	prepared, err := r.prepareExecution(prepareContext, lease, execution)
	if err != nil {
		if leaseContext.Err() != nil {
			return leaseContext.Err()
		}
		if cancelRequested.Load() && executionContext.Err() == nil {
			return r.cancelExecution(executionContext, lease, execution, prepared)
		}
		return r.finishExecutorError(ctx, lease, err)
	}
	if prepared.recordIdentity {
		if err := r.operations.RecordExecutorIdentity(ctx, lease, buildoperation.ExecutorIdentity{
			Name: prepared.identity.Name, UID: prepared.identity.UID,
		}); err != nil {
			return err
		}
	}
	observation := prepared.observation
	for {
		if leaseContext.Err() != nil {
			return leaseContext.Err()
		}
		if cancelRequested.Load() && observation.Phase != PhaseCanceled && observation.ErrorCode != "build_job_ownership_conflict" {
			return r.cancelExecution(executionContext, lease, execution, prepared)
		}
		if observation.Phase != "" && observation.Phase != PhaseRunning {
			// 快速终态可能先于下一次心跳，提交前同步读取取消标记，避免绕过刚受理的取消。
			checkContext, cancelCheck := context.WithTimeout(leaseContext, r.renewInterval())
			renewal, err := r.operations.Renew(checkContext, lease, r.config.LeaseDuration)
			cancelCheck()
			if err != nil {
				return err
			}
			if renewal.CancelRequested && observation.Phase != PhaseCanceled && observation.ErrorCode != "build_job_ownership_conflict" {
				return r.cancelExecution(executionContext, lease, execution, prepared)
			}
			return r.finishObservation(ctx, lease, observation)
		}
		select {
		case <-workContext.Done():
			if cancelRequested.Load() && executionContext.Err() == nil {
				continue
			}
			if leaseContext.Err() != nil {
				return leaseContext.Err()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return r.finishExecutorError(ctx, lease, newFailure("build_timeout", "build execution timed out", buildoperation.NonRetryable, nil))
		case <-time.After(r.config.PollInterval):
		}
		observation, err = r.executor.Observe(workContext, prepared.execution, prepared.identity)
		if err != nil {
			if leaseContext.Err() != nil {
				return leaseContext.Err()
			}
			if cancelRequested.Load() && executionContext.Err() == nil {
				continue
			}
			return r.finishExecutorError(ctx, lease, err)
		}
	}
}

func (r *Runner) renewInterval() time.Duration {
	return max(time.Nanosecond, min(r.config.LeaseDuration/3, 5*time.Second))
}

// maintainLease 持续覆盖输入读取、恢复、执行和取消；每次数据库调用有短预算，失败即停止执行。
func (r *Runner) maintainLease(ctx context.Context, lease buildoperation.Lease, onCancel func()) error {
	ticker := time.NewTicker(r.renewInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			renewContext, cancel := context.WithTimeout(ctx, r.renewInterval())
			renewal, err := r.operations.Renew(renewContext, lease, r.config.LeaseDuration)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				return fmt.Errorf("renew claimed build operation: %w", err)
			}
			lease = renewal.Lease
			if renewal.CancelRequested {
				onCancel()
			}
		}
	}
}

// cancelExecution 在独立于被打断观察的 Context 中取消 Job。Start 响应丢失时先回读确定性名称，绝不盲删。
func (r *Runner) cancelExecution(ctx context.Context, lease buildoperation.Lease, execution BuildExecution, prepared preparedExecution) error {
	// 即使 Start 响应失败，也保留实际调用的 Attempt；恢复中重建的新 Job 不属于旧 Attempt。
	if prepared.execution.BuildAttemptID != uuid.Nil {
		execution = prepared.execution
	}
	identity := prepared.identity
	if identity.Name == "" || identity.UID == "" {
		if prepared.execution.BuildAttemptID == uuid.Nil && lease.RecoveredFromAttemptID != nil {
			execution.BuildAttemptID = *lease.RecoveredFromAttemptID
		}
		identity.Name = "orbit-devops-build-" + execution.BuildAttemptID.String()
		if lease.RecoveredFromAttemptID != nil && execution.BuildAttemptID == *lease.RecoveredFromAttemptID {
			if lease.PreviousExecutorName != nil {
				identity.Name = *lease.PreviousExecutorName
			}
			if lease.PreviousExecutorUID != nil {
				identity.UID = *lease.PreviousExecutorUID
			}
		}
		observation, err := r.executor.Observe(ctx, execution, identity)
		if err != nil {
			return r.finishExecutorError(ctx, lease, err)
		}
		if observation.Phase == PhaseMissing {
			return r.operations.ConfirmCanceled(ctx, lease, "", false)
		}
		if observation.ErrorCode == "build_job_ownership_conflict" {
			return r.finishObservation(ctx, lease, observation)
		}
		identity = observation.Identity
		if identity.Name == "" || identity.UID == "" {
			return r.finishExecutorError(ctx, lease, newFailure("build_job_ownership_conflict",
				"the canceled build job could not be identified safely", buildoperation.UnknownOutcome, nil))
		}
		if err := r.operations.RecordExecutorIdentity(ctx, lease, buildoperation.ExecutorIdentity{Name: identity.Name, UID: identity.UID}); err != nil {
			return err
		}
	}
	observation, err := r.executor.Cancel(ctx, identity)
	if err != nil {
		return r.finishExecutorError(ctx, lease, err)
	}
	return r.finishObservation(ctx, lease, observation)
}

// prepareExecution 在恢复窗口中优先核验旧 Job；取消恢复绝不创建新 Job，普通恢复仅在安全条件下重建。
func (r *Runner) prepareExecution(ctx context.Context, lease buildoperation.Lease, execution BuildExecution) (preparedExecution, error) {
	ctx, span := r.config.Tracer.Start(ctx, "build prepare execution")
	defer span.End()
	if lease.Recovery && lease.RecoveredFromAttemptID != nil {
		previousExecution := execution
		previousExecution.BuildAttemptID = *lease.RecoveredFromAttemptID
		name := "orbit-devops-build-" + lease.RecoveredFromAttemptID.String()
		if lease.PreviousExecutorName != nil {
			name = *lease.PreviousExecutorName
		}
		uid := ""
		if lease.PreviousExecutorUID != nil {
			uid = *lease.PreviousExecutorUID
		}
		previous := ExecutionIdentity{Name: name, UID: uid}
		observation, err := r.executor.Observe(ctx, previousExecution, previous)
		if err != nil {
			return preparedExecution{execution: previousExecution, identity: previous}, err
		}
		// 取消请求在领取事务中已经确定。旧 Job 缺失即表示没有需要重新启动的执行；存在时只取消已核验身份。
		if lease.CancelRequested {
			if observation.Phase == PhaseMissing {
				return preparedExecution{execution: previousExecution, identity: previous,
					observation: ExecutionObservation{Phase: PhaseCanceled, Identity: previous}}, nil
			}
			if observation.ErrorCode == "build_job_ownership_conflict" {
				return preparedExecution{execution: previousExecution, identity: previous, observation: observation}, nil
			}
			identity := observation.Identity
			if identity.Name == "" || identity.UID == "" {
				return preparedExecution{execution: previousExecution}, newFailure("build_job_ownership_conflict",
					"the recovered build job could not be identified safely for cancellation", buildoperation.UnknownOutcome, nil)
			}
			canceled, err := r.executor.Cancel(ctx, identity)
			if err != nil {
				return preparedExecution{execution: previousExecution, identity: identity}, err
			}
			return preparedExecution{execution: previousExecution, identity: identity,
				observation: canceled, recordIdentity: true}, nil
		}
		if observation.Phase != PhaseMissing {
			// 新 Attempt 只是在观察旧 Job，仍记录同一外部身份以保留清晰证据链。
			trusted := observation.ErrorCode != "build_job_ownership_conflict" &&
				observation.Identity.Name != "" && observation.Identity.UID != ""
			if trusted {
				previous = observation.Identity
			}
			return preparedExecution{execution: previousExecution, identity: previous,
				observation: observation, recordIdentity: trusted}, nil
		}
		if uid != "" {
			return preparedExecution{execution: previousExecution, identity: previous},
				newFailure("build_job_missing", "previous build job disappeared after its UID was recorded", buildoperation.UnknownOutcome, nil)
		}
	}
	if lease.CancelRequested {
		return preparedExecution{}, newFailure("build_job_ownership_conflict",
			"the canceled build did not contain a recoverable executor identity", buildoperation.UnknownOutcome, nil)
	}
	identity, err := r.executor.Start(ctx, execution)
	if err != nil {
		return preparedExecution{execution: execution}, err
	}
	return preparedExecution{execution: execution, identity: identity,
		observation: ExecutionObservation{Phase: PhaseRunning, Identity: identity}, recordIdentity: true}, nil
}

func (r *Runner) finishObservation(ctx context.Context, lease buildoperation.Lease, observation ExecutionObservation) error {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("orbit-devops.build.phase", string(observation.Phase)))
	switch observation.Phase {
	case PhaseSucceeded:
		span.SetStatus(codes.Ok, "")
		_, err := r.operations.Succeed(ctx, lease, buildoperation.ArtifactResult{
			Repository: observation.Repository, Digest: observation.Digest,
			LogExcerpt: observation.LogExcerpt, LogTruncated: observation.LogTruncated,
		})
		return err
	case PhaseFailed:
		span.SetStatus(codes.Error, "build_execution_failed")
		disposition := observation.Disposition
		if disposition == "" {
			disposition = buildoperation.NonRetryable
		}
		_, err := r.operations.Fail(ctx, lease, buildoperation.Failure{
			Code: observation.ErrorCode, Summary: observation.ErrorSummary, Disposition: disposition,
			LogExcerpt: observation.LogExcerpt, LogTruncated: observation.LogTruncated,
		})
		return err
	case PhaseCanceled:
		return r.operations.ConfirmCanceled(ctx, lease, observation.LogExcerpt, observation.LogTruncated)
	case PhaseMissing, PhaseUnknown:
		span.SetStatus(codes.Error, "build_result_unknown")
		_, err := r.operations.HandleUnknownOutcome(ctx, lease, buildoperation.Failure{
			Code:        stableValue(observation.ErrorCode, "build_result_unknown"),
			Summary:     stableValue(observation.ErrorSummary, "build executor result could not be confirmed"),
			Disposition: buildoperation.UnknownOutcome, LogExcerpt: observation.LogExcerpt,
			LogTruncated: observation.LogTruncated,
		}, false)
		return err
	default:
		return errors.New("unsupported build execution observation")
	}
}

func (r *Runner) finishExecutorError(ctx context.Context, lease buildoperation.Lease, err error) error {
	trace.SpanFromContext(ctx).SetStatus(codes.Error, "build_executor_interrupted")
	var failure *FailureError
	if !errors.As(err, &failure) {
		// Adapter 的普通错误表示观察通道中断，不能据此断言外部 Job 已失败。
		failure = newFailure("build_executor_unavailable", "build executor observation is temporarily unavailable", buildoperation.UnknownOutcome, err)
	}
	evidence := buildoperation.Failure{Code: failure.code, Summary: failure.summary, Disposition: failure.disposition}
	if failure.disposition == buildoperation.UnknownOutcome {
		_, completionErr := r.operations.HandleUnknownOutcome(ctx, lease, evidence, failure.retry)
		return completionErr
	}
	_, completionErr := r.operations.Fail(ctx, lease, evidence)
	return completionErr
}

type FailureError struct {
	code, summary, disposition string
	retry                      bool
	cause                      error
}

func NewRetryableFailure(code, summary string, cause error) error {
	return newFailure(code, summary, buildoperation.Retryable, cause)
}

func NewFailure(code, summary string, cause error) error {
	return newFailure(code, summary, buildoperation.NonRetryable, cause)
}

func NewUnknownOutcome(code, summary string, retry bool, cause error) error {
	failure := newFailure(code, summary, buildoperation.UnknownOutcome, cause)
	failure.retry = retry
	return failure
}

func newFailure(code, summary, disposition string, cause error) *FailureError {
	return &FailureError{code: code, summary: summary, disposition: disposition, cause: cause}
}

func (e *FailureError) Error() string          { return e.summary }
func (e *FailureError) Unwrap() error          { return e.cause }
func (e *FailureError) Code() string           { return e.code }
func (e *FailureError) Summary() string        { return e.summary }
func (e *FailureError) Disposition() string    { return e.disposition }
func (e *FailureError) RetryRecommended() bool { return e.retry }

func stableValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (r *Runner) String() string {
	return fmt.Sprintf("build worker %s", r.config.WorkerID)
}
