package operation

import "context"

// DispatchMetrics 来自持久化事实，运输占用次数不是业务 Attempt 数量。
type DispatchMetrics struct {
	Pending      int64   `db:"pending"`
	Published    int64   `db:"published"`
	Quarantined  int64   `db:"quarantined"`
	OldestAge    float64 `db:"oldest_age"`
	Reservations int64   `db:"reservations"`
	Redeliveries int64   `db:"redeliveries"`
}

// ReadDispatchMetrics 读取有效积压与历史运输量；查询失败时由调用者暴露不可用，不能返回零积压。
func (m *Module) ReadDispatchMetrics(ctx context.Context) (DispatchMetrics, error) {
	var result DispatchMetrics
	err := m.db.GetContext(ctx, &result, `SELECT
	 count(*) FILTER (WHERE current AND state='pending') AS pending,
	 count(*) FILTER (WHERE current AND state='published') AS published,
	 count(*) FILTER (WHERE current AND state='quarantined') AS quarantined,
	 COALESCE(GREATEST(EXTRACT(EPOCH FROM ($1::timestamptz - min(created_at) FILTER (WHERE current AND state IN ('pending','published')))),0),0) AS oldest_age,
	 COALESCE(sum(reservation_count),0) AS reservations,
	 COALESCE(sum(GREATEST(reservation_count-1,0)),0) AS redeliveries
	 FROM (SELECT d.*, (d.sequence = o.current_dispatch_sequence AND
	 o.status IN ('pending','running','cancel_requested')) AS current
	 FROM operation_dispatches d JOIN operations o ON o.id=d.operation_id) d`, m.now())
	return result, err
}
