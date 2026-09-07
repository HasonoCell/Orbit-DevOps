package operation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrInvalidTransition     = errors.New("operation state does not allow this command")
	ErrAuthorizerUnavailable = errors.New("operation authorizer is not configured")
	ErrStaleObservation      = errors.New("operation changed after external observation")
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
	OperationID        uuid.UUID
	ActorID            string
	IdempotencyKey     string
	AttentionConfirmed bool
	ExpectedUpdatedAt  time.Time
}

type CancelCommand struct {
	OperationID    uuid.UUID
	ActorID        string
	IdempotencyKey string
}

// Retry 将确定失败的同一个 Operation 放回目标队尾，并保留其全部 Attempt 历史。
func (m *Module) Retry(ctx context.Context, command RetryCommand) (Record, error) {
	permission := projectauth.PermissionDevelop
	expectedStatus := StatusFailed
	if command.AttentionConfirmed {
		permission = projectauth.PermissionResolveUnknown
		expectedStatus = StatusAttentionRequired
	}
	return m.executeUserCommand(
		ctx,
		command.OperationID,
		command.ActorID,
		command.IdempotencyKey,
		"operation.retry",
		permission,
		struct {
			OperationID uuid.UUID `json:"operationId"`
		}{command.OperationID},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != expectedStatus {
				return ErrInvalidTransition
			}
			if command.AttentionConfirmed && !current.UpdatedAt.Equal(command.ExpectedUpdatedAt) {
				return ErrStaleObservation
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE operations
				 SET status = 'pending', automatic_retry_count = 0,
				     recovery_required = false,
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
		projectauth.PermissionDevelop,
		struct {
			OperationID uuid.UUID `json:"operationId"`
		}{command.OperationID},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			switch current.Status {
			case StatusPending:
				if _, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET status = 'canceled', recovery_required = false,
					     updated_at = $1, finished_at = $1
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

const (
	ReconcileSucceeded = "succeeded"
	ReconcileFailed    = "failed"
	ReconcileUnclear   = "unclear"
)

type ReconcileEvidence struct {
	Resolution        string
	ErrorCode         string
	ErrorSummary      string
	ExpectedUpdatedAt time.Time
}

type ReconcileCommand struct {
	OperationID    uuid.UUID
	ActorID        string
	IdempotencyKey string
	Evidence       ReconcileEvidence
}

// ReconcileAttention 只持久化外部只读检查的结论，不触发任何 Kubernetes 写入。
func (m *Module) ReconcileAttention(
	ctx context.Context,
	command ReconcileCommand,
) (Record, error) {
	if command.Evidence.ErrorCode == "" || command.Evidence.ErrorSummary == "" ||
		utf8.RuneCountInString(command.Evidence.ErrorCode) > 64 ||
		utf8.RuneCountInString(command.Evidence.ErrorSummary) > 512 {
		return Record{}, errors.New("invalid reconciliation evidence")
	}
	return m.executeUserCommand(
		ctx,
		command.OperationID,
		command.ActorID,
		command.IdempotencyKey,
		"operation.reconcile",
		projectauth.PermissionDevelop,
		struct {
			OperationID uuid.UUID `json:"operationId"`
		}{command.OperationID},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusAttentionRequired {
				return ErrInvalidTransition
			}
			if !current.UpdatedAt.Equal(command.Evidence.ExpectedUpdatedAt) {
				return ErrStaleObservation
			}
			switch command.Evidence.Resolution {
			case ReconcileSucceeded:
				_, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET status = 'succeeded', recovery_required = false,
					     error_code = NULL, error_summary = NULL, retry_disposition = NULL,
					     updated_at = $1, finished_at = $1
					 WHERE id = $2`,
					now,
					current.ID,
				)
				return err
			case ReconcileFailed:
				_, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET status = 'failed', recovery_required = false,
					     error_code = $1, error_summary = $2,
					     retry_disposition = 'non_retryable', updated_at = $3, finished_at = $3
					 WHERE id = $4`,
					command.Evidence.ErrorCode,
					command.Evidence.ErrorSummary,
					now,
					current.ID,
				)
				return err
			case ReconcileUnclear:
				_, err := tx.ExecContext(
					ctx,
					`UPDATE operations
					 SET error_code = $1, error_summary = $2,
					     retry_disposition = 'unknown_outcome', updated_at = $3, finished_at = $3
					 WHERE id = $4`,
					command.Evidence.ErrorCode,
					command.Evidence.ErrorSummary,
					now,
					current.ID,
				)
				return err
			default:
				return errors.New("invalid reconciliation resolution")
			}
		},
	)
}

type ForceFailCommand struct {
	OperationID    uuid.UUID
	ActorID        string
	IdempotencyKey string
	Reason         string
}

// ForceFailAttention 允许 owner 用明确原因结束无法自动判断的 Operation，解除目标阻塞。
func (m *Module) ForceFailAttention(
	ctx context.Context,
	command ForceFailCommand,
) (Record, error) {
	if strings.TrimSpace(command.Reason) == "" || utf8.RuneCountInString(command.Reason) > 512 {
		return Record{}, errors.New("invalid manual failure reason")
	}
	return m.executeUserCommand(
		ctx,
		command.OperationID,
		command.ActorID,
		command.IdempotencyKey,
		"operation.fail",
		projectauth.PermissionResolveUnknown,
		struct {
			OperationID uuid.UUID `json:"operationId"`
			Reason      string    `json:"reason"`
		}{command.OperationID, command.Reason},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusAttentionRequired {
				return ErrInvalidTransition
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE operations
				 SET status = 'failed', recovery_required = false,
				     error_code = 'manual_resolution', error_summary = $1,
				     retry_disposition = 'non_retryable', updated_at = $2, finished_at = $2
				 WHERE id = $3`,
				command.Reason,
				now,
				current.ID,
			); err != nil {
				return fmt.Errorf("force fail operation: %w", err)
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
	permission projectauth.Permission,
	fingerprintValue any,
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
		permission,
	); err != nil {
		return Record{}, err
	}
	requestHash, err := idempotency.Fingerprint(fingerprintValue)
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
	// 幂等重放已在前面返回；只有本次真正发生的用户状态变化才更新投递意图。
	if updated.Status == StatusPending {
		if err := scheduleDispatch(ctx, tx, operationID, commandType, now); err != nil {
			return Record{}, err
		}
	} else if err := obsoleteDispatches(ctx, tx, operationID, now); err != nil {
		return Record{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, updated); err != nil {
		return Record{}, err
	}
	auditSummary := map[string]any{
		"fromStatus": current.Status,
		"toStatus":   updated.Status,
	}
	if updated.ErrorCode != nil {
		auditSummary["errorCode"] = *updated.ErrorCode
	}
	if updated.ErrorSummary != nil {
		auditSummary["errorSummary"] = *updated.ErrorSummary
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID: actorID, Action: commandType, TargetType: "operation", TargetID: operationID,
		Summary:   auditSummary,
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
