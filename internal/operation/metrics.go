package operation

import (
	"context"
	"fmt"
)

type LabeledCount struct {
	Label string `db:"label"`
	Count int    `db:"count"`
}

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
		 FROM operations GROUP BY status`,
	); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count operation statuses: %w", err)
	}
	if err := m.db.QueryRowxContext(
		ctx,
		`SELECT
		 count(*) FILTER (WHERE status = 'pending' AND available_at <= now())::integer,
		 count(*) FILTER (WHERE status = 'pending' AND available_at > now())::integer
		 FROM operations`,
	).Scan(&snapshot.PendingAvailable, &snapshot.PendingDelayed); err != nil {
		return MetricsSnapshot{}, fmt.Errorf("count pending availability: %w", err)
	}
	if err := m.db.SelectContext(
		ctx,
		&snapshot.Events,
		`SELECT action AS label, count(*)::integer AS count
		 FROM audit_records
		 WHERE target_type = 'operation'
		 GROUP BY action`,
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
