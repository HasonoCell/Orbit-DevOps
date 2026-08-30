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

	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"

	AttemptRunning   = "running"
	AttemptSucceeded = "succeeded"
	AttemptFailed    = "failed"

	FailureLeaseExpired = "worker_lease_expired"
)

var (
	ErrLeaseLost = errors.New("operation lease is no longer owned by worker")
	ErrNotFound  = errors.New("operation not found")
)

type Record struct {
	ID             uuid.UUID  `db:"id"`
	Type           string     `db:"operation_type"`
	ReleaseID      uuid.UUID  `db:"release_id"`
	CreatedBy      string     `db:"actor_id"`
	IdempotencyKey string     `db:"idempotency_key"`
	TraceParent    string     `db:"traceparent"`
	TraceState     string     `db:"tracestate"`
	Status         string     `db:"status"`
	AttemptCount   int        `db:"attempt_count"`
	ErrorCategory  *string    `db:"error_category"`
	ErrorSummary   *string    `db:"error_summary"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	StartedAt      *time.Time `db:"started_at"`
	FinishedAt     *time.Time `db:"finished_at"`
	Attempts       []Attempt
}

type Attempt struct {
	ID            uuid.UUID  `db:"id"`
	Number        int        `db:"attempt_number"`
	WorkerID      string     `db:"worker_id"`
	Status        string     `db:"status"`
	ErrorCategory *string    `db:"error_category"`
	ErrorSummary  *string    `db:"error_summary"`
	StartedAt     time.Time  `db:"started_at"`
	FinishedAt    *time.Time `db:"finished_at"`
}

type CreatePendingCommand struct {
	ID             uuid.UUID
	ReleaseID      uuid.UUID
	ActorID        string
	IdempotencyKey string
	TraceParent    string
	TraceState     string
	CreatedAt      time.Time
}

type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
}

type Lease struct {
	OperationID   uuid.UUID
	ReleaseID     uuid.UUID
	AttemptID     uuid.UUID
	AttemptNumber int
	WorkerID      string
	ExpiresAt     time.Time
	TraceParent   string
	TraceState    string
}

type Failure struct {
	Category string
	Summary  string
}

type claimCandidate struct {
	ID           uuid.UUID `db:"id"`
	ReleaseID    uuid.UUID `db:"release_id"`
	Status       string    `db:"status"`
	AttemptCount int       `db:"attempt_count"`
	TraceParent  string    `db:"traceparent"`
	TraceState   string    `db:"tracestate"`
}

type Module struct {
	db *sqlx.DB
}

func New(db *sqlx.DB) *Module {
	return &Module{db: db}
}

func (m *Module) CreatePending(
	ctx context.Context,
	tx *sqlx.Tx,
	command CreatePendingCommand,
) (Record, error) {
	record := Record{
		ID:             command.ID,
		Type:           TypeReleaseDeploy,
		ReleaseID:      command.ReleaseID,
		CreatedBy:      command.ActorID,
		IdempotencyKey: command.IdempotencyKey,
		TraceParent:    command.TraceParent,
		TraceState:     command.TraceState,
		Status:         StatusPending,
		AttemptCount:   0,
		CreatedAt:      command.CreatedAt,
		UpdatedAt:      command.CreatedAt,
		Attempts:       []Attempt{},
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO operations
		 (id, operation_type, release_id, actor_id, idempotency_key, traceparent,
		  tracestate, status, attempt_count, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		record.ID,
		record.Type,
		record.ReleaseID,
		record.CreatedBy,
		record.IdempotencyKey,
		record.TraceParent,
		record.TraceState,
		record.Status,
		record.AttemptCount,
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

	now := time.Now().UTC()
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
		`SELECT id, release_id, status, attempt_count, traceparent, tracestate
		 FROM operations
		 WHERE status = 'pending'
		    OR (status = 'running' AND lease_expires_at <= $1)
		 ORDER BY created_at, id
		 FOR UPDATE SKIP LOCKED
		 LIMIT 1`,
		now,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lease{}, false, nil
		}
		return Lease{}, false, fmt.Errorf("select operation claim: %w", err)
	}

	if candidate.Status == StatusRunning {
		failureSummary := "worker lease expired before the attempt reached a terminal state"
		result, err := tx.ExecContext(
			ctx,
			`UPDATE operation_attempts
			 SET status = 'failed', error_category = $1, error_summary = $2, finished_at = $3
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
	}

	attemptNumber := candidate.AttemptCount + 1
	attemptID := uuid.New()
	expiresAt := now.Add(request.LeaseDuration)
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = 'running', attempt_count = $1, lease_owner = $2,
		     lease_expires_at = $3, error_category = NULL, error_summary = NULL,
		     started_at = COALESCE(started_at, $4), finished_at = NULL, updated_at = $4
		 WHERE id = $5`,
		attemptNumber,
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
		OperationID:   candidate.ID,
		ReleaseID:     candidate.ReleaseID,
		AttemptID:     attemptID,
		AttemptNumber: attemptNumber,
		WorkerID:      request.WorkerID,
		ExpiresAt:     expiresAt,
		TraceParent:   candidate.TraceParent,
		TraceState:    candidate.TraceState,
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
) (Lease, error) {
	if leaseDuration <= 0 {
		return Lease{}, errors.New("lease duration must be positive")
	}

	now := time.Now().UTC()
	expiresAt := now.Add(leaseDuration)
	result, err := m.db.ExecContext(
		ctx,
		`UPDATE operations
		 SET lease_expires_at = $1, updated_at = $2
		 WHERE id = $3 AND status = 'running' AND lease_owner = $4
		   AND attempt_count = $5 AND lease_expires_at > $2`,
		expiresAt,
		now,
		lease.OperationID,
		lease.WorkerID,
		lease.AttemptNumber,
	)
	if err != nil {
		return Lease{}, fmt.Errorf("renew operation lease: %w", err)
	}
	if err := requireOneLeaseRow(result); err != nil {
		return Lease{}, err
	}

	lease.ExpiresAt = expiresAt
	return lease, nil
}

func (m *Module) Succeed(ctx context.Context, lease Lease) error {
	return m.complete(ctx, lease, StatusSucceeded, Failure{})
}

func (m *Module) Fail(ctx context.Context, lease Lease, failure Failure) error {
	if failure.Category == "" {
		return errors.New("failure category is required")
	}
	if failure.Summary == "" {
		return errors.New("failure summary is required")
	}
	if utf8.RuneCountInString(failure.Category) > 64 {
		return errors.New("failure category exceeds 64 characters")
	}
	if utf8.RuneCountInString(failure.Summary) > 512 {
		return errors.New("failure summary exceeds 512 characters")
	}
	return m.complete(ctx, lease, StatusFailed, failure)
}

func (m *Module) complete(
	ctx context.Context,
	lease Lease,
	status string,
	failure Failure,
) error {
	now := time.Now().UTC()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin operation completion: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var actorID string
	if err := tx.GetContext(
		ctx,
		&actorID,
		`SELECT actor_id
		 FROM operations
		 WHERE id = $1 AND status = 'running' AND lease_owner = $2
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
		return fmt.Errorf("lock operation completion: %w", err)
	}

	var errorCategory *string
	var errorSummary *string
	attemptStatus := AttemptSucceeded
	if status == StatusFailed {
		attemptStatus = AttemptFailed
		errorCategory = &failure.Category
		errorSummary = &failure.Summary
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE operations
		 SET status = $1, lease_owner = NULL, lease_expires_at = NULL,
		     error_category = $2, error_summary = $3, updated_at = $4, finished_at = $4
		 WHERE id = $5`,
		status,
		errorCategory,
		errorSummary,
		now,
		lease.OperationID,
	); err != nil {
		return fmt.Errorf("complete operation: %w", err)
	}

	attemptResult, err := tx.ExecContext(
		ctx,
		`UPDATE operation_attempts
		 SET status = $1, error_category = $2, error_summary = $3, finished_at = $4
		 WHERE id = $5 AND status = 'running'`,
		attemptStatus,
		errorCategory,
		errorSummary,
		now,
		lease.AttemptID,
	)
	if err != nil {
		return fmt.Errorf("complete operation attempt: %w", err)
	}
	rowsAffected, err := attemptResult.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect operation attempt completion: %w", err)
	}
	if rowsAffected != 1 {
		return errors.New("claimed operation has no active attempt")
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    actorID,
			Action:     "operation." + status,
			TargetType: "operation",
			TargetID:   lease.OperationID,
			Summary: map[string]any{
				"attemptNumber": lease.AttemptNumber,
				"workerId":      lease.WorkerID,
				"status":        status,
				"errorCategory": errorCategory,
			},
			CreatedAt: now,
		},
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit operation completion: %w", err)
	}
	return nil
}

func requireOneLeaseRow(result sql.Result) error {
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect operation lease update: %w", err)
	}
	if rowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

const operationSelect = `SELECT id, operation_type, release_id, actor_id, idempotency_key,
       traceparent, tracestate,
       status, attempt_count, error_category, error_summary, created_at, updated_at,
       started_at, finished_at
 FROM operations`

const attemptSelect = `SELECT id, attempt_number, worker_id, status, error_category,
       error_summary, started_at, finished_at
 FROM operation_attempts`
