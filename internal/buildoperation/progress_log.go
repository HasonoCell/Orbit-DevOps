package buildoperation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// RecordProgressLog 接收 Executor 已脱敏的有界快照，不改变业务状态或产生交付事件。
// 先锁 Operation 再写 Attempt，与终态/接管使用同一锁顺序，避免过期 Worker 覆盖新执行的证据。
func (m *Module) RecordProgressLog(ctx context.Context, lease Lease, excerpt string, truncated bool) error {
	if err := validateLog(excerpt); err != nil {
		return err
	}
	if !utf8.ValidString(excerpt) {
		return errors.New("build log excerpt is not valid UTF-8")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin build progress log: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var expiresAt time.Time
	if err := tx.GetContext(ctx, &expiresAt, `SELECT lease_expires_at FROM build_operations
	 WHERE id=$1 AND status IN ('running','cancel_requested') AND lease_owner=$2 AND attempt_count=$3
	 FOR UPDATE`, lease.BuildOperationID, lease.WorkerID, lease.BuildAttemptNumber); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("lock build progress log: %w", err)
	}
	// 锁等待也可能跨过到期时间；取得锁后重新检查，而不是使用查询前的时间。
	if !expiresAt.After(m.now()) {
		return ErrLeaseLost
	}
	var attemptID uuid.UUID
	if err := tx.GetContext(ctx, &attemptID, `UPDATE build_attempts SET
	 log_excerpt=CASE WHEN $1='' THEN log_excerpt ELSE $1 END,
	 log_truncated=CASE WHEN $1='' THEN log_truncated ELSE $2 END
	 WHERE id=$3 AND build_operation_id=$4 AND attempt_number=$5 AND status='running'
	 RETURNING id`, excerpt, truncated, lease.BuildAttemptID, lease.BuildOperationID, lease.BuildAttemptNumber); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeaseLost
		}
		return fmt.Errorf("record build progress log: %w", err)
	}
	return tx.Commit()
}
