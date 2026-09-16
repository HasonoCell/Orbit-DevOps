package releaseoperation

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
	ErrInvalidTransition     = errors.New("operation state does not allow this command")
	ErrAuthorizerUnavailable = errors.New("operation authorizer is not configured")
	ErrStaleObservation      = errors.New("operation changed after external observation")
)

// Authorizer 把项目角色校验保持在 ReleaseOperation 状态事务内，避免授权与写入之间出现竞态。
type Authorizer interface {
	AuthorizeUserInTransaction(context.Context, *sqlx.Tx, identity.Caller) error
	RequireAuthorizedInTransaction(
		ctx context.Context,
		tx *sqlx.Tx,
		projectID uuid.UUID,
		caller identity.Caller,
		permission projectauth.Permission,
	) error
}

type RetryCommand struct {
	ReleaseOperationID uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
	AttentionConfirmed bool
	ExpectedUpdatedAt  time.Time
}

type CancelCommand struct {
	ReleaseOperationID uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
}

// Retry 将确定失败的同一个 ReleaseOperation 放回目标队尾，并保留其全部 Attempt 历史。
func (m *Module) Retry(ctx context.Context, command RetryCommand) (Record, error) {
	permission := projectauth.PermissionDevelop
	expectedStatus := StatusFailed
	if command.AttentionConfirmed {
		permission = projectauth.PermissionResolveUnknown
		expectedStatus = StatusAttentionRequired
	}
	return m.executeUserCommand(
		ctx,
		command.ReleaseOperationID,
		command.Caller,
		command.IdempotencyKey,
		"operation.retry",
		permission,
		struct {
			ReleaseOperationID uuid.UUID `json:"operationId"`
		}{command.ReleaseOperationID},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != expectedStatus {
				return ErrInvalidTransition
			}
			if command.AttentionConfirmed && !current.UpdatedAt.Equal(command.ExpectedUpdatedAt) {
				return ErrStaleObservation
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE release_operations
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
		command.ReleaseOperationID,
		command.Caller,
		command.IdempotencyKey,
		"operation.cancel",
		projectauth.PermissionDevelop,
		struct {
			ReleaseOperationID uuid.UUID `json:"operationId"`
		}{command.ReleaseOperationID},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			switch current.Status {
			case StatusPending:
				if _, err := tx.ExecContext(
					ctx,
					`UPDATE release_operations
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
					`UPDATE release_operations
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
	ReleaseOperationID uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
	Evidence           ReconcileEvidence
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
		command.ReleaseOperationID,
		command.Caller,
		command.IdempotencyKey,
		"operation.reconcile",
		projectauth.PermissionDevelop,
		struct {
			ReleaseOperationID uuid.UUID `json:"operationId"`
		}{command.ReleaseOperationID},
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
					`UPDATE release_operations
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
					`UPDATE release_operations
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
					`UPDATE release_operations
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
	ReleaseOperationID uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
	Reason             string
}

// ForceFailAttention 允许 owner 用明确原因结束无法自动判断的 ReleaseOperation，解除目标阻塞。
func (m *Module) ForceFailAttention(
	ctx context.Context,
	command ForceFailCommand,
) (Record, error) {
	if strings.TrimSpace(command.Reason) == "" || utf8.RuneCountInString(command.Reason) > 512 {
		return Record{}, errors.New("invalid manual failure reason")
	}
	return m.executeUserCommand(
		ctx,
		command.ReleaseOperationID,
		command.Caller,
		command.IdempotencyKey,
		"operation.fail",
		projectauth.PermissionResolveUnknown,
		struct {
			ReleaseOperationID uuid.UUID `json:"operationId"`
			Reason             string    `json:"reason"`
		}{command.ReleaseOperationID, command.Reason},
		func(ctx context.Context, tx *sqlx.Tx, current Record, now time.Time) error {
			if current.Status != StatusAttentionRequired {
				return ErrInvalidTransition
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE release_operations
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
	releaseOperationID uuid.UUID,
	caller identity.Caller,
	idempotencyKey string,
	commandType string,
	permission projectauth.Permission,
	fingerprintValue any,
	transition userTransition,
) (Record, error) {
	actorID := caller.ActorID()
	if m.authorizer == nil {
		return Record{}, ErrAuthorizerUnavailable
	}
	now := m.now()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin %s: %w", commandType, err)
	}
	defer func() { _ = tx.Rollback() }()
	projectID, err := locateUserCommandProject(ctx, tx, releaseOperationID)
	if err != nil {
		return Record{}, err
	}
	// 先身份控制共享锁，再锁操作；等待后最终权限/幂等仍在本事务内重读。
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, caller); err != nil {
		return Record{}, err
	}

	current, err := lockForUserCommand(ctx, tx, releaseOperationID)
	if err != nil {
		return Record{}, err
	}
	lockedProjectID, err := locateUserCommandProject(ctx, tx, releaseOperationID)
	if err != nil || lockedProjectID != projectID {
		return Record{}, ErrNotFound
	}
	if err := m.authorizer.RequireAuthorizedInTransaction(
		ctx,
		tx,
		projectID,
		caller,
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
	_, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, releaseOperationID, now)
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
	updated, err := getInTransaction(ctx, tx, releaseOperationID)
	if err != nil {
		return Record{}, err
	}
	// 幂等重放已在前面返回；只有本次真正发生的用户状态变化才更新投递意图。
	if updated.Status == StatusPending {
		if err := scheduleDispatch(ctx, tx, releaseOperationID, commandType, now); err != nil {
			return Record{}, err
		}
	} else if err := obsoleteDispatches(ctx, tx, releaseOperationID, now); err != nil {
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
		ActorID: actorID, Action: commandType, TargetType: "operation", TargetID: releaseOperationID,
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
	releaseOperationID uuid.UUID,
) (Record, error) {
	var current Record
	if err := tx.GetContext(
		ctx,
		&current,
		operationSelect+` WHERE id = $1 FOR UPDATE`,
		releaseOperationID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("lock operation command: %w", err)
	}
	return current, nil
}

func locateUserCommandProject(ctx context.Context, tx *sqlx.Tx, releaseOperationID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	if err := tx.GetContext(
		ctx,
		&projectID,
		`SELECT applications.project_id
		 FROM release_operations
		 JOIN deployment_targets ON deployment_targets.id = release_operations.deployment_target_id
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE release_operations.id = $1`,
		releaseOperationID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		return uuid.Nil, fmt.Errorf("resolve operation project: %w", err)
	}
	return projectID, nil
}

func getInTransaction(ctx context.Context, tx *sqlx.Tx, releaseOperationID uuid.UUID) (Record, error) {
	var record Record
	if err := tx.GetContext(ctx, &record, operationSelect+` WHERE id = $1`, releaseOperationID); err != nil {
		return Record{}, fmt.Errorf("get operation after command: %w", err)
	}
	attempts := make([]Attempt, 0)
	if err := tx.SelectContext(
		ctx,
		&attempts,
		attemptSelect+` WHERE operation_id = $1 ORDER BY attempt_number`,
		releaseOperationID,
	); err != nil {
		return Record{}, fmt.Errorf("get operation attempts after command: %w", err)
	}
	record.Attempts = attempts
	return record, nil
}
