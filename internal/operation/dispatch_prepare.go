package operation

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// DispatchPreparation 记录一次离线切换的结果，不是用户可调用的发布或权限接口。
type DispatchPreparation struct {
	Scheduled int `db:"scheduled"`
	Replayed  bool
}

// PrepareDispatches 只能在所有新旧 API、Worker 已停止后调用。数据库事务不能证明
// 外部执行者已经退出，因此离线前提由操作员核验，不能用租约过期代替进程检查。
// 同一批次全部提交或全部回滚；重复执行复用结果，下一次切换必须使用新批次。
func (m *Module) PrepareDispatches(ctx context.Context, batchID uuid.UUID) (DispatchPreparation, error) {
	result := DispatchPreparation{}
	if batchID == uuid.Nil {
		return result, errors.New("preparation batch ID is required")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	now := m.now()
	var inserted uuid.UUID
	err = tx.GetContext(ctx, &inserted, `INSERT INTO dispatch_preparations (id, completed_at)
	 VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING id`, batchID, now)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.GetContext(ctx, &result, `SELECT scheduled FROM dispatch_preparations WHERE id = $1`, batchID)
		result.Replayed = true
		return result, err
	}
	if err != nil {
		return result, err
	}
	var items []struct {
		ID      uuid.UUID       `db:"id"`
		Status  OperationStatus `db:"status"`
		Expires *time.Time      `db:"lease_expires_at"`
	}
	// 离线批次按统一顺序锁业务行，之后才修改 Outbox。历史快照和业务时间不变。
	err = tx.SelectContext(ctx, &items, `SELECT id, status, lease_expires_at FROM operations ORDER BY id FOR UPDATE`)
	if err != nil {
		return result, err
	}
	for _, item := range items {
		active := item.Status == StatusRunning || item.Status == StatusCancelRequested
		if item.Status == StatusPending || (active && item.Expires != nil && !item.Expires.After(now)) {
			if err := scheduleDispatch(ctx, tx, item.ID, "offline_preparation", now); err != nil {
				return result, err
			}
			result.Scheduled++
		} else {
			// 有效活动租约到期前不接管，终态及人工处理状态也不生成执行意图。
			if _, err := tx.ExecContext(ctx, `UPDATE operations SET current_dispatch_sequence = current_dispatch_sequence + 1 WHERE id = $1`, item.ID); err != nil {
				return result, err
			}
			if err := obsoleteDispatches(ctx, tx, item.ID, now); err != nil {
				return result, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE dispatch_preparations SET scheduled = $2 WHERE id = $1`, batchID, result.Scheduled); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
