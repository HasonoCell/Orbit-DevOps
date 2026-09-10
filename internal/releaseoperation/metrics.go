package releaseoperation

import (
	"context"
	"fmt"
)

type LabeledCount struct {
	Label string `db:"label"`
	Count int    `db:"count"`
}

// MetricsSnapshot 汇总可以从 PostgreSQL 重新构建的低基数 ReleaseOperation 事实。
type MetricsSnapshot struct {
	Statuses          []LabeledCount
	PendingAvailable  int
	PendingDelayed    int
	Events            []LabeledCount
	AttemptErrorCodes []LabeledCount
}

// ReadMetricsSnapshot 从 PostgreSQL 一次读取可重建指标，避免进程重启丢失可靠性事实。
func (m *Module) ReadMetricsSnapshot(ctx context.Context) (MetricsSnapshot, error) {
	snapshot := MetricsSnapshot{
		Statuses:          make([]LabeledCount, 0),
		Events:            make([]LabeledCount, 0),
		AttemptErrorCodes: make([]LabeledCount, 0),
	}
	if err := m.db.SelectContext(
		ctx,
		&snapshot.Statuses,
		`SELECT status AS label, count(*)::integer AS count
		 FROM release_operations GROUP BY status`,
	); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count operation statuses: %w", err)
	}
	if err := m.db.QueryRowxContext(
		ctx,
		`SELECT
		 count(*) FILTER (WHERE status = 'pending' AND available_at <= $1)::integer,
		 count(*) FILTER (WHERE status = 'pending' AND available_at > $1)::integer
		 FROM release_operations`,
		m.now(),
	).Scan(&snapshot.PendingAvailable, &snapshot.PendingDelayed); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count pending availability: %w", err)
	}
	if err := m.db.SelectContext(
		ctx,
		&snapshot.Events,
		`SELECT label, sum(count)::integer AS count
		 FROM (
		   SELECT action AS label, count(*)::integer AS count
		   FROM audit_records
		   WHERE target_type = 'operation'
		   GROUP BY action
		   UNION ALL
		   SELECT 'operation.reclaimed' AS label, count(*)::integer AS count
		   FROM audit_records
		   WHERE target_type = 'operation'
		     AND action = 'operation.claimed'
		     AND summary->>'recovery' = 'true'
		 ) AS event_counts
		 GROUP BY label`,
	); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count operation events: %w", err)
	}
	if err := m.db.SelectContext(
		ctx,
		&snapshot.AttemptErrorCodes,
		`SELECT error_code AS label, count(*)::integer AS count
		 FROM operation_attempts
		 WHERE error_code IS NOT NULL
		 GROUP BY error_code`,
	); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count attempt error codes: %w", err)
	}
	return snapshot, nil
}
