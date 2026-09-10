package pipeline

import (
	"context"
	"fmt"
	"time"
)

// Maintain 补建到期 Run 的唤醒事件，并清理已收束的短期运输与 Webhook 记录。
func (m *Module) Maintain(ctx context.Context) error {
	now := time.Now().UTC()
	if _, err := m.db.ExecContext(ctx, `INSERT INTO internal_event_outbox
		(id,topic,aggregate_id,protocol_version,state,available_at,next_dispatch_at,
		 traceparent,tracestate,created_at,updated_at)
		SELECT gen_random_uuid(),'delivery_run.reconcile.v1',r.id,1,'pending',$1,$1,
		 r.traceparent,r.tracestate,$1,$1 FROM delivery_runs r
		WHERE r.phase='artifact_ready' AND r.source_check_next_at<=$1
		AND NOT EXISTS (SELECT 1 FROM internal_event_outbox e
		 WHERE e.topic='delivery_run.reconcile.v1' AND e.aggregate_id=r.id
		 AND e.state IN ('pending','published')) LIMIT 100`, now); err != nil {
		m.recorder.RecordMaintenance("repair_failed")
		return fmt.Errorf("repair delivery run wakeups: %w", err)
	}
	// 先删运输记录；DeliveryRun 已冻结触发摘要，Webhook 外键会自动置空。
	if _, err := m.db.ExecContext(ctx, `DELETE FROM internal_event_outbox WHERE id IN (
		SELECT id FROM internal_event_outbox WHERE state='consumed' AND consumed_at<$1
		ORDER BY consumed_at,id LIMIT 1000)`, now.Add(-7*24*time.Hour)); err != nil {
		m.recorder.RecordMaintenance("event_cleanup_failed")
		return fmt.Errorf("clean consumed internal events: %w", err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE id IN (
		SELECT id FROM webhook_deliveries WHERE state IN ('processed','ignored','quarantined')
		AND processed_at<$1 ORDER BY processed_at,id LIMIT 1000)`, now.Add(-90*24*time.Hour)); err != nil {
		m.recorder.RecordMaintenance("webhook_cleanup_failed")
		return fmt.Errorf("clean retained webhook deliveries: %w", err)
	}
	m.recorder.RecordMaintenance("succeeded")
	return nil
}
