package access

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// SignalTargetPortChange 与 Target 更新同事务唤醒入口调和，后端端口只从当前 Target 派生。
func SignalTargetPortChange(ctx context.Context, tx *sqlx.Tx, targetID uuid.UUID, now time.Time) error {
	var projectIDs []uuid.UUID
	if err := tx.SelectContext(ctx, &projectIDs, `UPDATE project_gateway_sync s SET
		desired_revision=desired_revision+1,state='pending',next_attempt_at=$2,
		last_error_code=NULL,updated_at=$2 WHERE EXISTS (
			SELECT 1 FROM access_routes r JOIN access_hosts h ON h.id=r.host_id
			WHERE r.deployment_target_id=$1 AND r.lifecycle='active' AND h.lifecycle='active'
			AND h.project_id=s.project_id AND h.cluster_ref=s.cluster_ref AND h.namespace=s.namespace
		) RETURNING s.project_id`, targetID, now); err != nil {
		return err
	}
	for _, projectID := range projectIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO internal_event_outbox
			(id,topic,aggregate_id,state,available_at,next_dispatch_at,created_at,updated_at)
			VALUES($1,'project_gateway.reconcile.v1',$2,'pending',$3,$3,$3,$3)`, uuid.New(), projectID, now); err != nil {
			return err
		}
	}
	return nil
}
