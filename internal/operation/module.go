package operation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	TypeReleaseDeploy = "release.deploy"

	StatusPending           = "pending"
	StatusRunning           = "running"
	StatusCancelRequested   = "cancel_requested"
	StatusSucceeded         = "succeeded"
	StatusFailed            = "failed"
	StatusCanceled          = "canceled"
	StatusAttentionRequired = "attention_required"

	AttemptRunning        = "running"
	AttemptSucceeded      = "succeeded"
	AttemptFailed         = "failed"
	AttemptCanceled       = "canceled"
	AttemptOutcomeUnknown = "outcome_unknown"

	Retryable      = "retryable"
	NonRetryable   = "non_retryable"
	UnknownOutcome = "unknown_outcome"

	FailureLeaseExpired = "worker_lease_expired"

	defaultMaximumAutomaticRetries = 2
)

var (
	ErrLeaseLost = errors.New("operation lease is no longer owned by worker")
	ErrNotFound  = errors.New("operation not found")
)

type Record struct {
	ID                  uuid.UUID  `db:"id"`
	Type                string     `db:"operation_type"`
	ReleaseID           uuid.UUID  `db:"release_id"`
	DeploymentTargetID  uuid.UUID  `db:"deployment_target_id"`
	CreatedBy           string     `db:"actor_id"`
	IdempotencyKey      string     `db:"idempotency_key"`
	TraceParent         string     `db:"traceparent"`
	TraceState          string     `db:"tracestate"`
	Status              string     `db:"status"`
	AttemptCount        int        `db:"attempt_count"`
	AutomaticRetryCount int        `db:"automatic_retry_count"`
	ErrorCode           *string    `db:"error_code"`
	ErrorSummary        *string    `db:"error_summary"`
	RetryDisposition    *string    `db:"retry_disposition"`
	QueuedAt            time.Time  `db:"queued_at"`
	AvailableAt         time.Time  `db:"available_at"`
	CreatedAt           time.Time  `db:"created_at"`
	UpdatedAt           time.Time  `db:"updated_at"`
	StartedAt           *time.Time `db:"started_at"`
	FinishedAt          *time.Time `db:"finished_at"`
	Attempts            []Attempt
}

type Attempt struct {
	ID               uuid.UUID  `db:"id"`
	Number           int        `db:"attempt_number"`
	WorkerID         string     `db:"worker_id"`
	Status           string     `db:"status"`
	ErrorCode        *string    `db:"error_code"`
	ErrorSummary     *string    `db:"error_summary"`
	RetryDisposition *string    `db:"retry_disposition"`
	StartedAt        time.Time  `db:"started_at"`
	FinishedAt       *time.Time `db:"finished_at"`
}

type CreatePendingCommand struct {
	ID                 uuid.UUID
	ReleaseID          uuid.UUID
	DeploymentTargetID uuid.UUID
	ActorID            string
	IdempotencyKey     string
	TraceParent        string
	TraceState         string
	CreatedAt          time.Time
}

type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
}

type Lease struct {
	OperationID        uuid.UUID
	ReleaseID          uuid.UUID
	DeploymentTargetID uuid.UUID
	AttemptID          uuid.UUID
	AttemptNumber      int
	WorkerID           string
	ExpiresAt          time.Time
	TraceParent        string
	TraceState         string
	Recovery           bool
}

type Renewal struct {
	Lease           Lease
	CancelRequested bool
}

type Failure struct {
	Code        string
	Summary     string
	Disposition string
}

// FailureResult 告诉 Worker 本次失败是终结了 Operation，还是已进入自动重试等待。
type FailureResult struct {
	Status         string
	RetryScheduled bool
	AvailableAt    *time.Time
}

type claimCandidate struct {
	ID                  uuid.UUID `db:"id"`
	ReleaseID           uuid.UUID `db:"release_id"`
	DeploymentTargetID  uuid.UUID `db:"deployment_target_id"`
	Status              string    `db:"status"`
	AttemptCount        int       `db:"attempt_count"`
	AutomaticRetryCount int       `db:"automatic_retry_count"`
	TraceParent         string    `db:"traceparent"`
	TraceState          string    `db:"tracestate"`
}

type Module struct {
	db                      *sqlx.DB
	authorizer              Authorizer
	now                     func() time.Time
	maximumAutomaticRetries int
	retryDelay              func(int) time.Duration
}

// WithAuthorizer 为面向用户的 Operation 命令接入项目权限校验。
func WithAuthorizer(authorizer Authorizer) Option {
	return func(module *Module) {
		module.authorizer = authorizer
	}
}

// Option 只用于配置 Operation 调度策略，所有 Worker 必须使用一致的生产配置。
type Option func(*Module)

// WithClock 为测试提供确定性时间；生产代码应使用默认 UTC 时钟。
func WithClock(now func() time.Time) Option {
	return func(module *Module) {
		if now != nil {
			module.now = now
		}
	}
}

// WithAutomaticRetryPolicy 配置单次显式执行允许的自动恢复次数与退避算法。
func WithAutomaticRetryPolicy(maximum int, delay func(int) time.Duration) Option {
	return func(module *Module) {
		if maximum >= 0 {
			module.maximumAutomaticRetries = maximum
		}
		if delay != nil {
			module.retryDelay = delay
		}
	}
}

func New(db *sqlx.DB, options ...Option) *Module {
	module := &Module{
		db:                      db,
		now:                     func() time.Time { return time.Now().UTC() },
		maximumAutomaticRetries: defaultMaximumAutomaticRetries,
		retryDelay: func(retryNumber int) time.Duration {
			delay := time.Second
			for current := 1; current < retryNumber && delay < 30*time.Second; current++ {
				delay *= 2
				if delay > 30*time.Second {
					return 30 * time.Second
				}
			}
			return delay
		},
	}
	for _, option := range options {
		option(module)
	}
	return module
}

func (m *Module) CreatePending(
	ctx context.Context,
	tx *sqlx.Tx,
	command CreatePendingCommand,
) (Record, error) {
	record := Record{
		ID:                  command.ID,
		Type:                TypeReleaseDeploy,
		ReleaseID:           command.ReleaseID,
		DeploymentTargetID:  command.DeploymentTargetID,
		CreatedBy:           command.ActorID,
		IdempotencyKey:      command.IdempotencyKey,
		TraceParent:         command.TraceParent,
		TraceState:          command.TraceState,
		Status:              StatusPending,
		AttemptCount:        0,
		AutomaticRetryCount: 0,
		QueuedAt:            command.CreatedAt,
		AvailableAt:         command.CreatedAt,
		CreatedAt:           command.CreatedAt,
		UpdatedAt:           command.CreatedAt,
		Attempts:            []Attempt{},
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO operations
		 (id, operation_type, release_id, deployment_target_id, actor_id,
		  idempotency_key, traceparent, tracestate, status, attempt_count,
		  automatic_retry_count, queued_at, available_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		record.ID,
		record.Type,
		record.ReleaseID,
		record.DeploymentTargetID,
		record.CreatedBy,
		record.IdempotencyKey,
		record.TraceParent,
		record.TraceState,
		record.Status,
		record.AttemptCount,
		record.AutomaticRetryCount,
		record.QueuedAt,
		record.AvailableAt,
		record.CreatedAt,
		record.UpdatedAt,
	); err != nil {
		return Record{}, fmt.Errorf("insert release operation: %w", err)
	}

	return record, nil
}

func (m *Module) Get(ctx context.Context, id uuid.UUID) (Record, error) {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return Record{}, fmt.Errorf("begin operation query: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var record Record
	if err := tx.GetContext(ctx, &record, operationSelect+` WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("get operation: %w", err)
	}

	attempts := make([]Attempt, 0)
	if err := tx.SelectContext(
		ctx,
		&attempts,
		attemptSelect+` WHERE operation_id = $1 ORDER BY attempt_number`,
		id,
	); err != nil {
		return Record{}, fmt.Errorf("get operation attempts: %w", err)
	}
	record.Attempts = attempts
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit operation query: %w", err)
	}
	return record, nil
}

func (m *Module) GetByReleaseInTransaction(
	ctx context.Context,
	tx *sqlx.Tx,
	releaseID uuid.UUID,
) (Record, error) {
	var record Record
	if err := tx.GetContext(
		ctx,
		&record,
		operationSelect+` WHERE release_id = $1`,
		releaseID,
	); err != nil {
		return Record{}, fmt.Errorf("get operation by release: %w", err)
	}

	attempts := make([]Attempt, 0)
	if err := tx.SelectContext(
		ctx,
		&attempts,
		attemptSelect+` WHERE operation_id = $1 ORDER BY attempt_number`,
		record.ID,
	); err != nil {
		return Record{}, fmt.Errorf("get operation attempts by release: %w", err)
	}
	record.Attempts = attempts
	return record, nil
}

func (m *Module) ClaimNext(
	ctx context.Context,
	request ClaimRequest,
) (Lease, bool, error) {
	if request.WorkerID == "" {
		return Lease{}, false, errors.New("worker ID is required")
	}
	if request.LeaseDuration <= 0 {
		return Lease{}, false, errors.New("lease duration must be positive")
	}

	now := m.now()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Lease{}, false, fmt.Errorf("begin operation claim: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var candidate claimCandidate
	if err := tx.GetContext(
		ctx,
		&candidate,
		`SELECT candidate.id, candidate.release_id, candidate.deployment_target_id,
		        candidate.status, candidate.attempt_count,
		        candidate.automatic_retry_count, candidate.traceparent, candidate.tracestate
		 FROM operations AS candidate
		 WHERE ((candidate.status = 'pending' AND candidate.available_at <= $1)
		        OR (candidate.status IN ('running', 'cancel_requested')
		            AND candidate.lease_expires_at <= $1))
		   AND NOT EXISTS (
		       SELECT 1
		       FROM operations AS preceding
		       WHERE preceding.deployment_target_id = candidate.deployment_target_id
		         AND preceding.status IN (
		             'pending', 'running', 'cancel_requested', 'attention_required'
		         )
		         AND (preceding.queued_at, preceding.id) < (candidate.queued_at, candidate.id)
		   )
		 ORDER BY candidate.queued_at, candidate.id
		 FOR UPDATE OF candidate SKIP LOCKED
		 LIMIT 1`,
		now,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lease{}, false, nil
		}
		return Lease{}, false, fmt.Errorf("select operation claim: %w", err)
	}

	leaseExpired := candidate.Status == StatusRunning || candidate.Status == StatusCancelRequested
	recovery := candidate.Status == StatusRunning
	if leaseExpired {
		failureSummary := "worker lease expired before the attempt reached a terminal state"
		result, err := tx.ExecContext(
			ctx,
			`UPDATE operation_attempts
			 SET status = 'outcome_unknown', error_code = $1, error_summary = $2,
			     retry_disposition = 'unknown_outcome', finished_at = $3
			 WHERE operation_id = $4 AND attempt_number = $5 AND status = 'running'`,
			FailureLeaseExpired,
			failureSummary,
			now,
			candidate.ID,
			candidate.AttemptCount,
		)
		if err != nil {
			return Lease{}, false, fmt.Errorf("expire abandoned operation attempt: %w", err)
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return Lease{}, false, fmt.Errorf("inspect expired attempt update: %w", err)
		}
		if rowsAffected != 1 {
			return Lease{}, false, errors.New("running operation has no active attempt")
		}

		if candidate.Status == StatusCancelRequested {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE operations
				 SET status = 'attention_required', lease_owner = NULL, lease_expires_at = NULL,
				     error_code = 'cancellation_outcome_unknown', error_summary = $1,
				     retry_disposition = 'unknown_outcome', updated_at = $2, finished_at = $2
				 WHERE id = $3`,
				"worker lease expired before cancellation could be confirmed",
				now,
				candidate.ID,
			); err != nil {
				return Lease{}, false, fmt.Errorf("mark unknown cancellation outcome: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Entry{
				ActorID:    "operation-scheduler",
				ActorKind:  audit.ActorKindSystem,
				Action:     "operation.attention_required",
				TargetType: "operation",
				TargetID:   candidate.ID,
				Summary: map[string]any{
					"attemptNumber": candidate.AttemptCount,
					"errorCode":     "cancellation_outcome_unknown",
				},
				CreatedAt: now,
			}); err != nil {
				return Lease{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return Lease{}, false, fmt.Errorf("commit unknown cancellation outcome: %w", err)
			}
			return Lease{}, false, nil
		}

		// 租约过期意味着外部写入结果未知。超过自动恢复预算后必须停下等待人工处理，
		// 不能继续盲目执行可能重复或覆盖的发布动作。
		if candidate.AutomaticRetryCount >= m.maximumAutomaticRetries {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE operations
				 SET status = 'attention_required', lease_owner = NULL, lease_expires_at = NULL,
				     error_code = $1, error_summary = $2,
				     retry_disposition = 'unknown_outcome', updated_at = $3, finished_at = $3
				 WHERE id = $4`,
				FailureLeaseExpired,
				failureSummary,
				now,
				candidate.ID,
			); err != nil {
				return Lease{}, false, fmt.Errorf("mark exhausted lease recovery: %w", err)
			}
			if err := audit.Append(ctx, tx, audit.Entry{
				ActorID:    "operation-scheduler",
				ActorKind:  audit.ActorKindSystem,
				Action:     "operation.attention_required",
				TargetType: "operation",
				TargetID:   candidate.ID,
				Summary: map[string]any{
					"attemptNumber": candidate.AttemptCount,
					"errorCode":     FailureLeaseExpired,
				},
				CreatedAt: now,
			}); err != nil {
				return Lease{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return Lease{}, false, fmt.Errorf("commit exhausted lease recovery: %w", err)
			}
			return Lease{}, false, nil
		}
		candidate.AutomaticRetryCount++
	}

	attemptNumber := candidate.AttemptCount + 1
	attemptID := uuid.New()
	expiresAt := now.Add(request.LeaseDuration)
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = 'running', attempt_count = $1, automatic_retry_count = $2,
		     lease_owner = $3, lease_expires_at = $4, error_code = NULL,
		     error_summary = NULL, retry_disposition = NULL,
		     started_at = COALESCE(started_at, $5), finished_at = NULL, updated_at = $5
		 WHERE id = $6`,
		attemptNumber,
		candidate.AutomaticRetryCount,
		request.WorkerID,
		expiresAt,
		now,
		candidate.ID,
	); err != nil {
		return Lease{}, false, fmt.Errorf("claim operation: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO operation_attempts
		 (id, operation_id, attempt_number, worker_id, status, started_at)
		 VALUES ($1, $2, $3, $4, 'running', $5)`,
		attemptID,
		candidate.ID,
		attemptNumber,
		request.WorkerID,
		now,
	); err != nil {
		return Lease{}, false, fmt.Errorf("insert operation attempt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Lease{}, false, fmt.Errorf("commit operation claim: %w", err)
	}

	return Lease{
		OperationID:        candidate.ID,
		ReleaseID:          candidate.ReleaseID,
		DeploymentTargetID: candidate.DeploymentTargetID,
		AttemptID:          attemptID,
		AttemptNumber:      attemptNumber,
		WorkerID:           request.WorkerID,
		ExpiresAt:          expiresAt,
		TraceParent:        candidate.TraceParent,
		TraceState:         candidate.TraceState,
		Recovery:           recovery,
	}, true, nil
}

func (m *Module) CountPending(ctx context.Context) (int, error) {
	var count int
	if err := m.db.GetContext(
		ctx,
		&count,
		`SELECT count(*) FROM operations WHERE status = 'pending'`,
	); err != nil {
		return 0, fmt.Errorf("count pending operations: %w", err)
	}
	return count, nil
}

func (m *Module) Renew(
	ctx context.Context,
	lease Lease,
	leaseDuration time.Duration,
) (Renewal, error) {
	if leaseDuration <= 0 {
		return Renewal{}, errors.New("lease duration must be positive")
	}

	now := m.now()
	expiresAt := now.Add(leaseDuration)
	var status string
	err := m.db.GetContext(
		ctx,
		&status,
		`UPDATE operations
		 SET lease_expires_at = $1, updated_at = $2
		 WHERE id = $3 AND status IN ('running', 'cancel_requested') AND lease_owner = $4
		   AND attempt_count = $5 AND lease_expires_at > $2
		 RETURNING status`,
		expiresAt,
		now,
		lease.OperationID,
		lease.WorkerID,
		lease.AttemptNumber,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Renewal{}, ErrLeaseLost
		}
		return Renewal{}, fmt.Errorf("renew operation lease: %w", err)
	}

	lease.ExpiresAt = expiresAt
	return Renewal{Lease: lease, CancelRequested: status == StatusCancelRequested}, nil
}

func (m *Module) Succeed(ctx context.Context, lease Lease) error {
	return m.completeSuccess(ctx, lease)
}

// ConfirmCanceled 只允许当前租约所有者关闭 cancel_requested，过期 Worker 仍会被 fencing。
func (m *Module) ConfirmCanceled(ctx context.Context, lease Lease) error {
	now := m.now()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation cancellation completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var locked int
	if err := tx.GetContext(
		ctx,
		&locked,
		`SELECT 1 FROM operations
		 WHERE id = $1 AND status = 'cancel_requested' AND lease_owner = $2
		   AND attempt_count = $3 AND lease_expires_at > $4
		 FOR UPDATE`,
		lease.OperationID,
		lease.WorkerID,
		lease.AttemptNumber,
		now,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("lock operation cancellation completion: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = 'canceled', lease_owner = NULL, lease_expires_at = NULL,
		     updated_at = $1, finished_at = $1
		 WHERE id = $2`,
		now,
		lease.OperationID,
	); err != nil {
		return fmt.Errorf("complete operation cancellation: %w", err)
	}
	if err := completeAttempt(ctx, tx, lease, AttemptCanceled, Failure{}, now); err != nil {
		return err
	}
	if err := appendCompletionAudit(
		ctx,
		tx,
		"operation.canceled",
		lease,
		StatusCanceled,
		nil,
		now,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation cancellation completion: %w", err)
	}
	return nil
}

// Fail 关闭当前 Attempt，并依据稳定的失败分类决定终结 Operation 或安排自动重试。
func (m *Module) Fail(
	ctx context.Context,
	lease Lease,
	failure Failure,
) (FailureResult, error) {
	if failure.Code == "" {
		return FailureResult{}, errors.New("failure code is required")
	}
	if failure.Summary == "" {
		return FailureResult{}, errors.New("failure summary is required")
	}
	if utf8.RuneCountInString(failure.Code) > 64 {
		return FailureResult{}, errors.New("failure code exceeds 64 characters")
	}
	if utf8.RuneCountInString(failure.Summary) > 512 {
		return FailureResult{}, errors.New("failure summary exceeds 512 characters")
	}
	if failure.Disposition != Retryable && failure.Disposition != NonRetryable {
		return FailureResult{}, errors.New("failure retry disposition is invalid")
	}

	now := m.now()
	tx, automaticRetryCount, err := m.lockCompletion(ctx, lease, now)
	if err != nil {
		return FailureResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := completeAttempt(ctx, tx, lease, AttemptFailed, failure, now); err != nil {
		return FailureResult{}, err
	}

	result := FailureResult{Status: StatusFailed}
	action := "operation.failed"
	if failure.Disposition == Retryable && automaticRetryCount < m.maximumAutomaticRetries {
		automaticRetryCount++
		delay := m.retryDelay(automaticRetryCount)
		if delay < 0 {
			delay = 0
		}
		availableAt := now.Add(delay)
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE operations
			 SET status = 'pending', automatic_retry_count = $1, available_at = $2,
			     lease_owner = NULL, lease_expires_at = NULL, error_code = NULL,
			     error_summary = NULL, retry_disposition = NULL,
			     updated_at = $3, finished_at = NULL
			 WHERE id = $4`,
			automaticRetryCount,
			availableAt,
			now,
			lease.OperationID,
		); err != nil {
			return FailureResult{}, fmt.Errorf("schedule operation retry: %w", err)
		}
		result = FailureResult{
			Status:         StatusPending,
			RetryScheduled: true,
			AvailableAt:    &availableAt,
		}
		action = "operation.retry_scheduled"
	} else {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE operations
			 SET status = 'failed', lease_owner = NULL, lease_expires_at = NULL,
			     error_code = $1, error_summary = $2, retry_disposition = $3,
			     updated_at = $4, finished_at = $4
			 WHERE id = $5`,
			failure.Code,
			failure.Summary,
			failure.Disposition,
			now,
			lease.OperationID,
		); err != nil {
			return FailureResult{}, fmt.Errorf("fail operation: %w", err)
		}
	}

	if err := appendCompletionAudit(
		ctx,
		tx,
		action,
		lease,
		result.Status,
		&failure,
		now,
	); err != nil {
		return FailureResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return FailureResult{}, fmt.Errorf("commit operation failure: %w", err)
	}
	return result, nil
}

func (m *Module) completeSuccess(ctx context.Context, lease Lease) error {
	now := m.now()
	tx, _, err := m.lockCompletion(ctx, lease, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = 'succeeded', lease_owner = NULL, lease_expires_at = NULL,
		     error_code = NULL, error_summary = NULL, retry_disposition = NULL,
		     updated_at = $1, finished_at = $1
		 WHERE id = $2`,
		now,
		lease.OperationID,
	); err != nil {
		return fmt.Errorf("succeed operation: %w", err)
	}
	if err := completeAttempt(ctx, tx, lease, AttemptSucceeded, Failure{}, now); err != nil {
		return err
	}
	if err := appendCompletionAudit(
		ctx,
		tx,
		"operation.succeeded",
		lease,
		StatusSucceeded,
		nil,
		now,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation success: %w", err)
	}
	return nil
}

// lockCompletion 使用租约与 Attempt 序号共同做 fencing，拒绝过期 Worker 的迟到结果。
func (m *Module) lockCompletion(
	ctx context.Context,
	lease Lease,
	now time.Time,
) (*sqlx.Tx, int, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("begin operation completion: %w", err)
	}
	var state struct {
		AutomaticRetryCount int `db:"automatic_retry_count"`
	}
	if err := tx.GetContext(
		ctx,
		&state,
		`SELECT automatic_retry_count
		 FROM operations
		 WHERE id = $1 AND status = 'running' AND lease_owner = $2
		   AND attempt_count = $3 AND lease_expires_at > $4
		 FOR UPDATE`,
		lease.OperationID,
		lease.WorkerID,
		lease.AttemptNumber,
		now,
	); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrLeaseLost
		}
		return nil, 0, fmt.Errorf("lock operation completion: %w", err)
	}
	return tx, state.AutomaticRetryCount, nil
}

func completeAttempt(
	ctx context.Context,
	tx *sqlx.Tx,
	lease Lease,
	status string,
	failure Failure,
	now time.Time,
) error {
	var errorCode, errorSummary, disposition *string
	if status == AttemptFailed {
		errorCode = &failure.Code
		errorSummary = &failure.Summary
		disposition = &failure.Disposition
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE operation_attempts
		 SET status = $1, error_code = $2, error_summary = $3,
		     retry_disposition = $4, finished_at = $5
		 WHERE id = $6 AND status = 'running'`,
		status,
		errorCode,
		errorSummary,
		disposition,
		now,
		lease.AttemptID,
	)
	if err != nil {
		return fmt.Errorf("complete operation attempt: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect operation attempt completion: %w", err)
	}
	if rowsAffected != 1 {
		return errors.New("claimed operation has no active attempt")
	}
	return nil
}

func appendCompletionAudit(
	ctx context.Context,
	tx *sqlx.Tx,
	action string,
	lease Lease,
	status string,
	failure *Failure,
	now time.Time,
) error {
	summary := map[string]any{
		"attemptNumber": lease.AttemptNumber,
		"workerId":      lease.WorkerID,
		"status":        status,
	}
	if failure != nil {
		summary["errorCode"] = failure.Code
		summary["retryDisposition"] = failure.Disposition
	}
	return audit.Append(ctx, tx, audit.Entry{
		ActorID:    lease.WorkerID,
		ActorKind:  audit.ActorKindSystem,
		Action:     action,
		TargetType: "operation",
		TargetID:   lease.OperationID,
		Summary:    summary,
		CreatedAt:  now,
	})
}

const operationSelect = `SELECT id, operation_type, release_id, deployment_target_id,
       actor_id, idempotency_key, traceparent, tracestate, status, attempt_count,
       automatic_retry_count, error_code, error_summary, retry_disposition,
       queued_at, available_at, created_at, updated_at, started_at, finished_at
 FROM operations`

const attemptSelect = `SELECT id, attempt_number, worker_id, status, error_code,
	   error_summary, retry_disposition, started_at, finished_at
 FROM operation_attempts`
