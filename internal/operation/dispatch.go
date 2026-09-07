package operation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// DispatchRef 只定位一次业务调度意图；消息不携带发布配置或权限凭据。
type DispatchRef struct {
	DispatchID  uuid.UUID `db:"id" json:"dispatch_id"`
	OperationID uuid.UUID `db:"operation_id" json:"operation_id"`
	Generation  int64     `db:"generation" json:"generation"`
	Version     int       `db:"version" json:"version"`
}

// Dispatch 是投递器取得的短期运输权限，不是 Worker 的业务执行租约。
type Dispatch struct {
	DispatchRef
	AvailableAt   time.Time `db:"available_at"`
	PublishToken  uuid.UUID `db:"publish_token"`
	DeliveryCount int       `db:"delivery_count"`
}

// scheduleDispatch 与调用者的业务状态变化共用事务，保证提交后必有待投递意图。
// 调度元数据不更新 Operation.updated_at，避免破坏人工恢复的观测版本。
func scheduleDispatch(ctx context.Context, tx *sqlx.Tx, operationID uuid.UUID, reason string, now time.Time) error {
	var current struct {
		Generation   int64     `db:"dispatch_generation"`
		AvailableAt  time.Time `db:"available_at"`
		AttemptCount int       `db:"attempt_count"`
	}
	if err := tx.GetContext(ctx, &current, `UPDATE operations
	 SET dispatch_generation = dispatch_generation + 1 WHERE id = $1
	 RETURNING dispatch_generation, available_at, attempt_count`, operationID); err != nil {
		return fmt.Errorf("advance dispatch generation: %w", err)
	}
	if err := obsoleteDispatches(ctx, tx, operationID, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_dispatches
	 (id, operation_id, generation, reason, expected_attempt_count, state,
	  available_at, next_dispatch_at, created_at, updated_at)
	 VALUES ($1, $2, $3, $4, $5, 'pending', $6, $7, $7, $7)`,
		uuid.New(), operationID, current.Generation, reason, current.AttemptCount, current.AvailableAt, now)
	if err != nil {
		return fmt.Errorf("record durable dispatch: %w", err)
	}
	return nil
}

// obsoleteDispatches 撤销尚未消费的旧意图和投递 token；已消费记录保留关联历史。
func obsoleteDispatches(ctx context.Context, tx *sqlx.Tx, operationID uuid.UUID, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE operation_dispatches SET state = 'obsolete',
	 publish_token = NULL, publish_expires_at = NULL, updated_at = $2
	 WHERE operation_id = $1 AND state IN ('pending', 'published', 'quarantined')`, operationID, now)
	return err
}

// ReserveDispatches 只领取有资格进入运输链路的意图，不创建 Attempt。
// 锁只覆盖 Outbox；业务资格会在消费者事务中再次核验，网络发送不占用数据库事务。
func (m *Module) ReserveDispatches(ctx context.Context, limit int, leaseDuration time.Duration) ([]Dispatch, error) {
	if limit < 1 || limit > 1000 || leaseDuration <= 0 {
		return nil, errors.New("invalid dispatch reservation")
	}
	now := m.now()
	items := []Dispatch{}
	// 只依据数据库当前意图隔离未知协议；外部损坏载荷不能改变合法工作。
	if _, err := m.db.ExecContext(ctx, `WITH invalid AS (
	 SELECT d.id FROM operation_dispatches d JOIN operations o ON o.id = d.operation_id
	 WHERE d.state IN ('pending','published') AND d.generation = o.dispatch_generation AND d.version <> 1
	 ORDER BY d.id FOR UPDATE OF d SKIP LOCKED LIMIT $2
	) UPDATE operation_dispatches d SET state = 'quarantined', publish_token = NULL, publish_expires_at = NULL,
	 last_error_code = 'unsupported_dispatch_version', updated_at = $1 FROM invalid WHERE d.id = invalid.id`, now, limit); err != nil {
		return nil, err
	}
	err := m.db.SelectContext(ctx, &items, `WITH due AS (
	 SELECT d.id FROM operation_dispatches d JOIN operations o ON o.id = d.operation_id
	 WHERE d.state IN ('pending', 'published') AND d.version = 1 AND d.next_dispatch_at <= $1
	   AND (d.publish_expires_at IS NULL OR d.publish_expires_at <= $1)
	   AND d.generation = o.dispatch_generation AND d.expected_attempt_count = o.attempt_count
	   AND (o.status = 'pending' OR (o.status IN ('running', 'cancel_requested') AND o.lease_expires_at <= $1))
	   AND NOT EXISTS (SELECT 1 FROM operations p WHERE p.deployment_target_id = o.deployment_target_id
	     AND p.status IN ('pending', 'running', 'cancel_requested', 'attention_required')
	     AND (p.queued_at, p.id) < (o.queued_at, o.id))
	 ORDER BY d.next_dispatch_at, d.id FOR UPDATE OF d SKIP LOCKED LIMIT $2
	) UPDATE operation_dispatches d SET publish_token = $3, publish_expires_at = $4,
	 delivery_count = delivery_count + 1, updated_at = $1 FROM due WHERE d.id = due.id
	 RETURNING d.id, d.operation_id, d.generation, d.version, d.available_at, d.publish_token, d.delivery_count`,
		now, limit, uuid.New(), now.Add(leaseDuration))
	if err != nil {
		return nil, fmt.Errorf("reserve dispatches: %w", err)
	}
	return items, nil
}

const (
	DispatchClaimed  = "claimed"
	DispatchIgnored  = "ignored"
	DispatchDeferred = "deferred"
	DispatchResolved = "resolved"
)

// DispatchClaim 区分已取得执行权、重复消息、持久化延期与无须外部执行的状态收束。
type DispatchClaim struct {
	Disposition string
	Lease       Lease
}

// ClaimDispatch 只领取指定代次。先锁 Operation 再锁 Outbox，与业务受理/重试保持相同锁序。
// 代次被消费与业务租约创建在同一事务内完成，重复消息不能制造新的 Attempt。
func (m *Module) ClaimDispatch(ctx context.Context, ref DispatchRef, request ClaimRequest) (DispatchClaim, error) {
	ignored := DispatchClaim{Disposition: DispatchIgnored}
	if ref.DispatchID == uuid.Nil || ref.OperationID == uuid.Nil || ref.Generation <= 0 || ref.Version != 1 {
		return ignored, errors.New("invalid dispatch reference")
	}
	if request.WorkerID == "" || request.LeaseDuration <= 0 {
		return ignored, errors.New("invalid worker lease request")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return ignored, err
	}
	defer func() { _ = tx.Rollback() }()
	var candidate claimCandidate
	err = tx.GetContext(ctx, &candidate, `SELECT id, release_id, deployment_target_id, status,
	 attempt_count, automatic_retry_count, recovery_required, traceparent, tracestate,
	 dispatch_generation, available_at, lease_expires_at FROM operations WHERE id = $1 FOR UPDATE`, ref.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ignored, nil
	}
	if err != nil {
		return ignored, err
	}
	if candidate.DispatchGeneration != ref.Generation {
		return ignored, nil
	}
	var intent struct {
		State    string `db:"state"`
		Expected int    `db:"expected_attempt_count"`
		Version  int    `db:"version"`
	}
	err = tx.GetContext(ctx, &intent, `SELECT state, expected_attempt_count, version
	 FROM operation_dispatches WHERE id = $1 AND operation_id = $2 AND generation = $3 FOR UPDATE`,
		ref.DispatchID, ref.OperationID, ref.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return ignored, nil
	}
	if err != nil {
		return ignored, err
	}
	if intent.State != "pending" && intent.State != "published" {
		return ignored, nil
	}
	now := m.now()
	if intent.Version != 1 {
		_, err = tx.ExecContext(ctx, `UPDATE operation_dispatches SET state = 'quarantined',
		 publish_token = NULL, publish_expires_at = NULL, last_error_code = 'unsupported_dispatch_version', updated_at = $2 WHERE id = $1`, ref.DispatchID, now)
		if err != nil {
			return ignored, err
		}
		return ignored, tx.Commit()
	}
	active := candidate.Status == StatusRunning || candidate.Status == StatusCancelRequested
	if intent.Expected != candidate.AttemptCount || (candidate.Status != StatusPending && !active) {
		if err := obsoleteDispatches(ctx, tx, ref.OperationID, now); err != nil {
			return ignored, err
		}
		return ignored, tx.Commit()
	}
	var preceding bool
	err = tx.GetContext(ctx, &preceding, `SELECT EXISTS (SELECT 1 FROM operations p JOIN operations o
	 ON o.id = $1 WHERE p.deployment_target_id = o.deployment_target_id
	 AND p.status IN ('pending', 'running', 'cancel_requested', 'attention_required')
	 AND (p.queued_at, p.id) < (o.queued_at, o.id))`, ref.OperationID)
	if err != nil {
		return ignored, err
	}
	if preceding || (candidate.Status == StatusPending && candidate.AvailableAt.After(now)) ||
		(active && (candidate.LeaseExpiresAt == nil || candidate.LeaseExpiresAt.After(now))) {
		next := now.Add(time.Second)
		if candidate.Status == StatusPending && candidate.AvailableAt.After(next) {
			next = candidate.AvailableAt
		}
		_, err = tx.ExecContext(ctx, `UPDATE operation_dispatches SET state = 'pending',
		 next_dispatch_at = $2, publish_token = NULL, publish_expires_at = NULL, updated_at = $3 WHERE id = $1`, ref.DispatchID, next, now)
		if err != nil {
			return ignored, err
		}
		return DispatchClaim{Disposition: DispatchDeferred}, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE operation_dispatches SET state = 'consumed',
	 consumed_at = $2, publish_token = NULL, publish_expires_at = NULL, updated_at = $2 WHERE id = $1`, ref.DispatchID, now); err != nil {
		return ignored, err
	}
	lease, claimed, err := m.claimCandidate(ctx, tx, candidate, request, now)
	if err != nil {
		return ignored, err
	}
	if !claimed {
		return DispatchClaim{Disposition: DispatchResolved}, nil
	}
	return DispatchClaim{Disposition: DispatchClaimed, Lease: lease}, nil
}

// ConfirmDispatch 仅确认本轮运输 token。消费或延期先于确认时，迟到确认是安全的空操作。
// errorCode 只能是调用者映射后的稳定代码，不得将 Redis 连接字符串写入持久化记录。
func (m *Module) ConfirmDispatch(ctx context.Context, dispatch Dispatch, errorCode string, grace time.Duration) error {
	if grace <= 0 {
		return errors.New("dispatch grace must be positive")
	}
	if errorCode != "" && errorCode != "queue_unavailable" {
		return errors.New("invalid dispatch error code")
	}
	now := m.now()
	state := "published"
	// 持续未消费时逐步降低补发频率，最多增加 30 秒；业务可执行时间不变。
	if dispatch.DeliveryCount > 1 {
		grace += dispatchBackoff(dispatch.DeliveryCount - 1)
	}
	next := now.Add(grace)
	if dispatch.AvailableAt.After(now) {
		next = dispatch.AvailableAt.Add(grace)
	}
	if errorCode != "" {
		state = "pending"
		next = now.Add(dispatchBackoff(dispatch.DeliveryCount))
	}
	_, err := m.db.ExecContext(ctx, `UPDATE operation_dispatches SET state = $3,
	 next_dispatch_at = $4, publish_token = NULL, publish_expires_at = NULL,
	 last_error_code = NULLIF($5, ''), published_at = CASE WHEN $5 = '' THEN $6 ELSE published_at END,
	 updated_at = $6 WHERE id = $1 AND publish_token = $2 AND publish_expires_at > $6
	 AND state IN ('pending', 'published')`, dispatch.DispatchID, dispatch.PublishToken, state, next, errorCode, now)
	return err
}

// dispatchBackoff 限制基础设施重发频率，不改变业务重试预算或最早执行时间。
func dispatchBackoff(attempt int) time.Duration {
	delay := time.Second
	for n := 1; n < attempt && delay < 30*time.Second; n++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

// RepairDispatches 为真实过期的业务租约登记恢复意图。恢复记录绑定当前 Attempt，
// 已有未消费意图时不重复增加代次；关闭未知 Attempt 与恢复预算仍由领取事务决定。
func (m *Module) RepairDispatches(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid dispatch repair limit")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := m.now()
	var ids []uuid.UUID
	err = tx.SelectContext(ctx, &ids, `SELECT o.id FROM operations o
	 WHERE o.status IN ('running', 'cancel_requested') AND o.lease_expires_at <= $1
	 AND NOT EXISTS (SELECT 1 FROM operation_dispatches d WHERE d.operation_id = o.id
	   AND d.generation = o.dispatch_generation AND d.expected_attempt_count = o.attempt_count
	   AND d.state IN ('pending', 'published', 'quarantined'))
	 ORDER BY o.lease_expires_at, o.id FOR UPDATE OF o SKIP LOCKED LIMIT $2`, now, limit)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := scheduleDispatch(ctx, tx, id, "lease_recovery", now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}
