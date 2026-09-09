// Package buildoperation 管理源码构建工作的状态、执行历史、业务租约与持久化调度意图。
package buildoperation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Status 表示一次构建工作的整体业务状态，不描述单次执行或消息运输结果。
type Status string

const (
	StatusPending           Status = "pending"
	StatusRunning           Status = "running"
	StatusCancelRequested   Status = "cancel_requested"
	StatusAttentionRequired Status = "attention_required"
	StatusSucceeded         Status = "succeeded"
	StatusFailed            Status = "failed"
	StatusCanceled          Status = "canceled"
)

var ErrNotFound = errors.New("build operation not found")

type Record struct {
	ID                      uuid.UUID  `db:"id"`
	BuildID                 uuid.UUID  `db:"build_id"`
	CreatedBy               string     `db:"actor_id"`
	IdempotencyKey          string     `db:"idempotency_key"`
	TraceParent             string     `db:"traceparent"`
	TraceState              string     `db:"tracestate"`
	Status                  Status     `db:"status"`
	AttemptCount            int        `db:"attempt_count"`
	AutomaticRetryCount     int        `db:"automatic_retry_count"`
	RecoveryRequired        bool       `db:"recovery_required"`
	ErrorCode               *string    `db:"error_code"`
	ErrorSummary            *string    `db:"error_summary"`
	RetryDisposition        *string    `db:"retry_disposition"`
	QueuedAt                time.Time  `db:"queued_at"`
	AvailableAt             time.Time  `db:"available_at"`
	CurrentDispatchSequence int64      `db:"current_dispatch_sequence"`
	CreatedAt               time.Time  `db:"created_at"`
	UpdatedAt               time.Time  `db:"updated_at"`
	StartedAt               *time.Time `db:"started_at"`
	FinishedAt              *time.Time `db:"finished_at"`
	Attempts                []Attempt
}

// AttemptStatus 表示 Build Worker 的一次执行或恢复结果。
type AttemptStatus string

const (
	AttemptRunning        AttemptStatus = "running"
	AttemptSucceeded      AttemptStatus = "succeeded"
	AttemptFailed         AttemptStatus = "failed"
	AttemptCanceled       AttemptStatus = "canceled"
	AttemptOutcomeUnknown AttemptStatus = "outcome_unknown"
)

type Attempt struct {
	ID                     uuid.UUID     `db:"id"`
	Number                 int           `db:"attempt_number"`
	WorkerID               string        `db:"worker_id"`
	Status                 AttemptStatus `db:"status"`
	RecoveredFromAttemptID *uuid.UUID    `db:"recovered_from_attempt_id"`
	ExecutorName           *string       `db:"executor_name"`
	ExecutorUID            *string       `db:"executor_uid"`
	ErrorCode              *string       `db:"error_code"`
	ErrorSummary           *string       `db:"error_summary"`
	RetryDisposition       *string       `db:"retry_disposition"`
	LogExcerpt             string        `db:"log_excerpt"`
	LogTruncated           bool          `db:"log_truncated"`
	StartedAt              time.Time     `db:"started_at"`
	FinishedAt             *time.Time    `db:"finished_at"`
}

type CreatePendingCommand struct {
	ID             uuid.UUID
	BuildID        uuid.UUID
	ActorID        string
	IdempotencyKey string
	TraceParent    string
	TraceState     string
	CreatedAt      time.Time
}

type Module struct {
	db                      *sqlx.DB
	authorizer              Authorizer
	now                     func() time.Time
	maximumAutomaticRetries int
	retryDelay              func(int) time.Duration
}

// Authorizer 把 BuildOperation 用户命令的权限检查保持在状态事务内。
type Authorizer interface {
	RequireInTransaction(context.Context, *sqlx.Tx, uuid.UUID, string, projectauth.Permission) error
}

// Option 配置所有 Build Worker 必须保持一致的重试策略或测试时钟。
type Option func(*Module)

// WithAuthorizer 启用面向用户的取消、重试和人工收束命令。
func WithAuthorizer(authorizer Authorizer) Option {
	return func(module *Module) { module.authorizer = authorizer }
}

// WithClock 只为确定性测试替换 UTC 时钟。
func WithClock(now func() time.Time) Option {
	return func(module *Module) {
		if now != nil {
			module.now = now
		}
	}
}

// WithAutomaticRetryPolicy 配置可恢复基础设施失败的业务重试预算与退避。
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
		db: db, now: func() time.Time { return time.Now().UTC() }, maximumAutomaticRetries: 2,
		retryDelay: func(retryNumber int) time.Duration {
			delay := time.Second
			for current := 1; current < retryNumber && delay < 30*time.Second; current++ {
				delay *= 2
			}
			return min(delay, 30*time.Second)
		},
	}
	for _, option := range options {
		option(module)
	}
	return module
}

// CreatePending 与 Build 受理共用事务，并在该事务中建立第一代可靠调度意图。
func (m *Module) CreatePending(ctx context.Context, tx *sqlx.Tx, command CreatePendingCommand) (Record, error) {
	record := Record{
		ID:             command.ID,
		BuildID:        command.BuildID,
		CreatedBy:      command.ActorID,
		IdempotencyKey: command.IdempotencyKey,
		TraceParent:    command.TraceParent,
		TraceState:     command.TraceState,
		Status:         StatusPending,
		QueuedAt:       command.CreatedAt,
		AvailableAt:    command.CreatedAt,
		CreatedAt:      command.CreatedAt,
		UpdatedAt:      command.CreatedAt,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO build_operations
	 (id, build_id, actor_id, idempotency_key, traceparent, tracestate, status,
	  queued_at, available_at, created_at, updated_at)
	 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $7, $7, $7)`,
		record.ID, record.BuildID, record.CreatedBy, record.IdempotencyKey,
		record.TraceParent, record.TraceState, record.CreatedAt); err != nil {
		return Record{}, fmt.Errorf("insert build operation: %w", err)
	}
	if err := scheduleDispatch(ctx, tx, record.ID, "build_created", command.CreatedAt); err != nil {
		return Record{}, err
	}
	record.CurrentDispatchSequence = 1
	return record, nil
}

// Get 返回 BuildOperation 当前状态；Attempt 历史在执行切片中加入同一查询表面。
func (m *Module) Get(ctx context.Context, id uuid.UUID) (Record, error) {
	var record Record
	if err := m.db.GetContext(ctx, &record, `SELECT id, build_id, actor_id, idempotency_key,
	 traceparent, tracestate, status, attempt_count, automatic_retry_count,
	 recovery_required, error_code, error_summary, retry_disposition, queued_at,
	 available_at, current_dispatch_sequence, created_at, updated_at, started_at, finished_at
	 FROM build_operations WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("get build operation: %w", err)
	}
	record.Attempts = []Attempt{}
	if err := m.db.SelectContext(ctx, &record.Attempts, `SELECT id, attempt_number, worker_id,
	 status, recovered_from_attempt_id, executor_name, executor_uid, error_code, error_summary,
	 retry_disposition, log_excerpt, log_truncated, started_at, finished_at
	 FROM build_attempts WHERE build_operation_id = $1 ORDER BY attempt_number`, id); err != nil {
		return Record{}, fmt.Errorf("list build attempts: %w", err)
	}
	return record, nil
}

// scheduleDispatch 只在领域状态事务中调用，保证已受理 Build 一定有持久化运输意图。
func scheduleDispatch(ctx context.Context, tx *sqlx.Tx, operationID uuid.UUID, reason string, now time.Time) error {
	var current struct {
		Sequence     int64     `db:"current_dispatch_sequence"`
		AvailableAt  time.Time `db:"available_at"`
		AttemptCount int       `db:"attempt_count"`
	}
	if err := tx.GetContext(ctx, &current, `UPDATE build_operations
	 SET current_dispatch_sequence = current_dispatch_sequence + 1 WHERE id = $1
	 RETURNING current_dispatch_sequence, available_at, attempt_count`, operationID); err != nil {
		return fmt.Errorf("advance build dispatch sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO build_dispatches
	 (id, build_operation_id, sequence, dispatch_reason, expected_attempt_count,
	  state, available_at, next_dispatch_at, created_at, updated_at)
	 VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $7, $7)`,
		uuid.New(), operationID, current.Sequence, reason, current.AttemptCount,
		current.AvailableAt, now); err != nil {
		return fmt.Errorf("record build dispatch: %w", err)
	}
	return nil
}
