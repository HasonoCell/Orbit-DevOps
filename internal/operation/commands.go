package operation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrInvalidTransition     = errors.New("operation state does not allow this command")
	ErrAuthorizerUnavailable = errors.New("operation authorizer is not configured")
)

// Authorizer 把项目角色校验保持在 Operation 状态事务内，避免授权与写入之间出现竞态。
type Authorizer interface {
	RequireInTransaction(
		ctx context.Context,
		tx *sqlx.Tx,
		projectID uuid.UUID,
		actorID string,
		permission projectauth.Permission,
	) error
}

type RetryCommand struct {
	OperationID    uuid.UUID
	ActorID        string
	IdempotencyKey string
}

type CancelCommand struct {
	OperationID    uuid.UUID
	ActorID        string
	IdempotencyKey string
}

// Retry 将确定失败的同一个 Operation 放回目标队尾，并保留其全部 Attempt 历史。
func (m *Module) Retry(ctx context.Context, command RetryCommand) (Record, error) {
	return m.executeUserCommand(
		ctx,
		command.OperationID,
		command.ActorID,
		command.IdempotencyKey,
		"operation.retry",
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusFailed {
				return ErrInvalidTransition
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE operations
				 SET status = 'pending', automatic_retry_count = 0,
				     queued_at = $1, available_at = $1,
				     lease_owner = NULL, lease_expires_at = NULL,
				     error_code = NULL, error_summary = NULL, retry_disposition = NULL,
				     updated_at = $1, finished_at = NULL
				 WHERE id = $2`,
				now,
				current.ID,
			); err != nil {
				return fmt.Errorf("retry operation: %w", err)
			}
			return nil
		},
	)
}

// Cancel 对排队任务立即终结，对运行任务只记录请求并等待持有租约的 Worker 确认停止。
func (m *Module) Cancel(ctx context.Context, command CancelCommand) (Record, error) {
	return m.executeUserCommand(
		ctx,
		command.OperationID,
		command.ActorID,
		command.IdempotencyKey,
		"operation.cancel",
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			switch current.Status {
			case StatusPending:
				if _, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET status = 'canceled', updated_at = $1, finished_at = $1
					 WHERE id = $2`,
					now,
					current.ID,
				); err != nil {
					return fmt.Errorf("cancel pending operation: %w", err)
				}
			case StatusRunning:
				if _, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET status = 'cancel_requested', updated_at = $1
					 WHERE id = $2`,
					now,
					current.ID,
				); err != nil {
					return fmt.Errorf("request operation cancellation: %w", err)
				}
			default:
				return ErrInvalidTransition
			}
			return nil
		},
	)
}

type userTransition func(
	ctx context.Context,
	tx *sqlx.Tx,
	current Record,
	now time.Time,
) error

// executeUserCommand 集中处理锁、授权、幂等、状态变化与审计的共同事务边界。
func (m *Module) executeUserCommand(
	ctx context.Context,
	operationID uuid.UUID,
	actorID string,
	idempotencyKey string,
	commandType string,
	transition userTransition,
) (Record, error) {
	if m.authorizer == nil {
		return Record{}, ErrAuthorizerUnavailable
	}
	now := m.now()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin %s: %w", commandType, err)
	}
	defer func() { _ = tx.Rollback() }()

	current, projectID, err := lockForUserCommand(ctx, tx, operationID)
	if err != nil {
		return Record{}, err
	}
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		projectID,
		actorID,
		projectauth.PermissionDevelop,
	); err != nil {
		return Record{}, err
	}
	requestHash, err := idempotency.Fingerprint(struct {
		OperationID uuid.UUID `json:"operationId"`
	}{OperationID: operationID})
	if err != nil {
		return Record{}, fmt.Errorf("fingerprint %s: %w", commandType, err)
	}
	scope := idempotency.Scope{
		ActorID: actorID, CommandType: commandType, Key: idempotencyKey,
	}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, operationID, now)
	if err != nil {
		return Record{}, err
	}
	if !isNew {
		var replay Record
		if err := idempotency.LoadResponse(ctx, tx, scope, &replay); err != nil {
			return Record{}, err
		}
		if err := tx.Commit(); err != nil {
			return Record{}, fmt.Errorf("commit %s replay: %w", commandType, err)
		}
		return replay, nil
	}

	if err := transition(ctx, tx, current, now); err != nil {
		return Record{}, err
	}
	updated, err := getInTransaction(ctx, tx, operationID)
	if err != nil {
		return Record{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, updated); err != nil {
		return Record{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID: actorID, Action: commandType, TargetType: "operation", TargetID: operationID,
		Summary: map[string]any{
			"fromStatus": current.Status,
			"toStatus":   updated.Status,
		},
		CreatedAt: now,
	}); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit %s: %w", commandType, err)
	}
	return updated, nil
}

func lockForUserCommand(
	ctx context.Context,
	tx *sqlx.Tx,
	operationID uuid.UUID,
) (Record, uuid.UUID, error) {
	var current Record
	if err := tx.GetContext(
		ctx,
		&current,
		operationSelect+` WHERE id = $1 FOR UPDATE`,
		operationID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, uuid.Nil, ErrNotFound
		}
		return Record{}, uuid.Nil, fmt.Errorf("lock operation command: %w", err)
	}
	var projectID uuid.UUID
	if err := tx.GetContext(
		ctx,
		&projectID,
		`SELECT applications.project_id
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1`,
		current.DeploymentTargetID,
	); err != nil {
		return Record{}, uuid.Nil, fmt.Errorf("resolve operation project: %w", err)
	}
	return current, projectID, nil
}

func getInTransaction(ctx context.Context, tx *sqlx.Tx, operationID uuid.UUID) (Record, error) {
	var record Record
	if err := tx.GetContext(ctx, &record, operationSelect+` WHERE id = $1`, operationID); err != nil {
		return Record{}, fmt.Errorf("get operation after command: %w", err)
	}
	attempts := make([]Attempt, 0)
	if err := tx.SelectContext(
		ctx,
		&attempts,
		attemptSelect+` WHERE operation_id = $1 ORDER BY attempt_number`,
		operationID,
	); err != nil {
		return Record{}, fmt.Errorf("get operation attempts after command: %w", err)
	}
	record.Attempts = attempts
	return record, nil
}
