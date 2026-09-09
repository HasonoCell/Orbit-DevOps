package buildoperation

import "context"

// MetricsSnapshot 只包含可由 PostgreSQL 重建的低基数构建可靠性事实。
type MetricsSnapshot struct {
	PendingAvailable     int64   `db:"pending_available"`
	PendingDelayed       int64   `db:"pending_delayed"`
	Running              int64   `db:"running"`
	AttentionRequired    int64   `db:"attention_required"`
	Succeeded            int64   `db:"succeeded"`
	Failed               int64   `db:"failed"`
	Canceled             int64   `db:"canceled"`
	ActiveExecutors      int64   `db:"active_executors"`
	AttemptDurationSum   float64 `db:"attempt_duration_sum"`
	AttemptDurationCount int64   `db:"attempt_duration_count"`
	DispatchPending      int64   `db:"dispatch_pending"`
	DispatchPublished    int64   `db:"dispatch_published"`
	DispatchQuarantined  int64   `db:"dispatch_quarantined"`
	DispatchOldestAge    float64 `db:"dispatch_oldest_age"`
	Reservations         int64   `db:"reservations"`
	Redeliveries         int64   `db:"redeliveries"`
}

// ReadMetricsSnapshot 一次读取 Operation、Attempt 与 Dispatch 快照；调用者用 NaN 表示读取失败。
func (m *Module) ReadMetricsSnapshot(ctx context.Context) (MetricsSnapshot, error) {
	var snapshot MetricsSnapshot
	now := m.now()
	if err := m.db.GetContext(ctx, &snapshot, `SELECT
	 count(*) FILTER (WHERE status='pending' AND available_at <= $1) AS pending_available,
	 count(*) FILTER (WHERE status='pending' AND available_at > $1) AS pending_delayed,
	 count(*) FILTER (WHERE status='running') AS running,
	 count(*) FILTER (WHERE status='attention_required') AS attention_required,
	 count(*) FILTER (WHERE status='succeeded') AS succeeded,
	 count(*) FILTER (WHERE status='failed') AS failed,
	 count(*) FILTER (WHERE status='canceled') AS canceled
	 FROM build_operations`, now); err != nil {
		return MetricsSnapshot{}, err
	}
	if err := m.db.QueryRowxContext(ctx, `SELECT
	 count(*) FILTER (WHERE status='running' AND executor_uid IS NOT NULL),
	 COALESCE(sum(EXTRACT(EPOCH FROM (finished_at-started_at))) FILTER (WHERE finished_at IS NOT NULL),0),
	 count(*) FILTER (WHERE finished_at IS NOT NULL)
	 FROM build_attempts`).Scan(&snapshot.ActiveExecutors, &snapshot.AttemptDurationSum, &snapshot.AttemptDurationCount); err != nil {
		return MetricsSnapshot{}, err
	}
	if err := m.db.QueryRowxContext(ctx, `SELECT
	 count(*) FILTER (WHERE current AND state='pending'),
	 count(*) FILTER (WHERE current AND state='published'),
	 count(*) FILTER (WHERE current AND state='quarantined'),
	 COALESCE(GREATEST(EXTRACT(EPOCH FROM ($1::timestamptz-min(created_at) FILTER (WHERE current AND state IN ('pending','published')))),0),0),
	 COALESCE(sum(reservation_count),0), COALESCE(sum(GREATEST(reservation_count-1,0)),0)
	 FROM (SELECT d.*, (d.sequence=o.current_dispatch_sequence AND
	 o.status IN ('pending','running','cancel_requested')) AS current
	 FROM build_dispatches d JOIN build_operations o ON o.id=d.build_operation_id) d`, now).Scan(
		&snapshot.DispatchPending, &snapshot.DispatchPublished, &snapshot.DispatchQuarantined,
		&snapshot.DispatchOldestAge, &snapshot.Reservations, &snapshot.Redeliveries); err != nil {
		return MetricsSnapshot{}, err
	}
	return snapshot, nil
}
