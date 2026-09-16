package buildoperation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrInvalidTransition     = errors.New("build operation state does not allow this command")
	ErrAuthorizerUnavailable = errors.New("build operation authorizer is not configured")
)

type CancelCommand struct {
	BuildOperationID uuid.UUID
	Caller           identity.Caller
	IdempotencyKey   string
}

type RetryCommand struct {
	BuildOperationID uuid.UUID
	Caller           identity.Caller
	IdempotencyKey   string
}

type ReconcileCommand struct {
	BuildOperationID uuid.UUID
	Caller           identity.Caller
	IdempotencyKey   string
}

type ForceFailCommand struct {
	BuildOperationID uuid.UUID
	Caller           identity.Caller
	IdempotencyKey   string
	Reason           string
}

// Cancel 对 pending 构建立即终结；running 构建只记录请求，等待 Worker 确认 Job 已停止。
func (m *Module) Cancel(ctx context.Context, command CancelCommand) (Record, error) {
	return m.executeUserCommand(ctx, command.BuildOperationID, command.Caller, command.IdempotencyKey,
		"build_operation.cancel", projectauth.PermissionDevelop, command.BuildOperationID,
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			switch current.Status {
			case StatusPending:
				_, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'canceled',
				 recovery_required = false, updated_at = $1, finished_at = $1 WHERE id = $2`, now, current.ID)
				return err
			case StatusRunning:
				_, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'cancel_requested',
				 updated_at = $1 WHERE id = $2`, now, current.ID)
				return err
			default:
				return ErrInvalidTransition
			}
		})
}

// Retry 复用失败 Build 的冻结输入和历史，并建立新的调度序列。
func (m *Module) Retry(ctx context.Context, command RetryCommand) (Record, error) {
	return m.executeUserCommand(ctx, command.BuildOperationID, command.Caller, command.IdempotencyKey,
		"build_operation.retry", projectauth.PermissionDevelop, command.BuildOperationID,
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusFailed {
				return ErrInvalidTransition
			}
			_, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'pending', automatic_retry_count = 0,
			 recovery_required = false, queued_at = $1, available_at = $1, lease_owner = NULL,
			 lease_expires_at = NULL, error_code = NULL, error_summary = NULL, retry_disposition = NULL,
			 updated_at = $1, finished_at = NULL WHERE id = $2`, now, current.ID)
			return err
		})
}

// Reconcile 将 attention_required 放回只读恢复路径；Runner 必须先观察旧 Job。
func (m *Module) Reconcile(ctx context.Context, command ReconcileCommand) (Record, error) {
	return m.executeUserCommand(ctx, command.BuildOperationID, command.Caller, command.IdempotencyKey,
		"build_operation.reconcile", projectauth.PermissionDevelop, command.BuildOperationID,
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusAttentionRequired {
				return ErrInvalidTransition
			}
			_, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'pending', recovery_required = true,
			 available_at = $1, lease_owner = NULL, lease_expires_at = NULL, error_code = NULL,
			 error_summary = NULL, retry_disposition = NULL, updated_at = $1, finished_at = NULL WHERE id = $2`, now, current.ID)
			return err
		})
}

// ForceFail 允许 owner 用明确原因结束无法自动判断的构建，不删除旧 Job 证据。
func (m *Module) ForceFail(ctx context.Context, command ForceFailCommand) (Record, error) {
	reason := strings.TrimSpace(command.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 512 {
		return Record{}, errors.New("invalid build manual failure reason")
	}
	return m.executeUserCommand(ctx, command.BuildOperationID, command.Caller, command.IdempotencyKey,
		"build_operation.force_fail", projectauth.PermissionResolveUnknown,
		struct {
			ID     uuid.UUID `json:"buildOperationId"`
			Reason string    `json:"reason"`
		}{command.BuildOperationID, reason},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusAttentionRequired {
				return ErrInvalidTransition
			}
			_, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'failed', recovery_required = false,
			 error_code = 'manual_resolution', error_summary = $1, retry_disposition = 'non_retryable',
			 updated_at = $2, finished_at = $2 WHERE id = $3`, reason, now, current.ID)
			return err
		})
}

type userTransition func(context.Context, *sqlx.Tx, Record, time.Time) error

// executeUserCommand 集中锁、授权、幂等、状态转换、Dispatch 和审计事务。
func (m *Module) executeUserCommand(ctx context.Context, id uuid.UUID, caller identity.Caller, key, action string,
	permission projectauth.Permission, fingerprint any, transition userTransition) (Record, error) {
	actorID := caller.ActorID()
	if m.authorizer == nil {
		return Record{}, ErrAuthorizerUnavailable
	}
	now := m.now()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback() }()
	projectID, err := locateUserCommandProject(ctx, tx, id)
	if err != nil {
		return Record{}, err
	}
	// 控制共享锁必须先于操作锁，避免与账号/成员排他撤销发生反向等待。
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, caller); err != nil {
		return Record{}, err
	}
	current, err := getInTransactionForUpdate(ctx, tx, id)
	if err != nil {
		return Record{}, err
	}
	lockedProjectID, err := locateUserCommandProject(ctx, tx, id)
	if err != nil || lockedProjectID != projectID {
		return Record{}, ErrNotFound
	}
	if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, permission); err != nil {
		return Record{}, err
	}
	hash, err := idempotency.Fingerprint(fingerprint)
	if err != nil {
		return Record{}, err
	}
	scope := idempotency.Scope{ActorID: actorID, CommandType: action, Key: key}
	_, fresh, err := idempotency.Claim(ctx, tx, scope, hash, id, now)
	if err != nil {
		return Record{}, err
	}
	if !fresh {
		var replay Record
		if err := idempotency.LoadResponse(ctx, tx, scope, &replay); err != nil {
			return Record{}, err
		}
		return replay, tx.Commit()
	}
	if err := transition(ctx, tx, current, now); err != nil {
		return Record{}, err
	}
	updated, err := getInTransaction(ctx, tx, id)
	if err != nil {
		return Record{}, err
	}
	if updated.Status == StatusPending {
		if err := scheduleDispatch(ctx, tx, id, action, now); err != nil {
			return Record{}, err
		}
	} else if err := obsoleteDispatches(ctx, tx, id, now); err != nil {
		return Record{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, updated); err != nil {
		return Record{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: actorID, Action: action,
		TargetType: "build_operation", TargetID: id,
		Summary: map[string]any{"fromStatus": current.Status, "toStatus": updated.Status}, CreatedAt: now}); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit %s: %w", action, err)
	}
	return updated, nil
}

func locateUserCommandProject(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	if err := tx.GetContext(ctx, &projectID, `SELECT b.project_id FROM builds b
	 JOIN build_operations o ON o.build_id = b.id WHERE o.id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("resolve build operation project: %w", err)
	}
	return projectID, nil
}

func getInTransactionForUpdate(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (Record, error) {
	var record Record
	if err := tx.GetContext(ctx, &record, operationSelect+` WHERE id = $1 FOR UPDATE`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("lock build operation: %w", err)
	}
	return record, nil
}

func getInTransaction(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (Record, error) {
	var record Record
	if err := tx.GetContext(ctx, &record, operationSelect+` WHERE id = $1`, id); err != nil {
		return Record{}, err
	}
	return record, nil
}

const operationSelect = `SELECT id, build_id, actor_id, idempotency_key, traceparent, tracestate,
 status, attempt_count, automatic_retry_count, recovery_required, error_code, error_summary,
 retry_disposition, queued_at, available_at, current_dispatch_sequence, created_at, updated_at,
 started_at, finished_at FROM build_operations`
