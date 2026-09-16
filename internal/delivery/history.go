package delivery

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const maximumReleaseHistoryPageSize = 100

type ReleaseOperationSummary struct {
	ID           uuid.UUID                               `db:"operation_id"`
	Status       releaseoperation.ReleaseOperationStatus `db:"operation_status"`
	AttemptCount int                                     `db:"operation_attempt_count"`
	ErrorCode    *string                                 `db:"operation_error_code"`
	ErrorSummary *string                                 `db:"operation_error_summary"`
	QueuedAt     time.Time                               `db:"operation_queued_at"`
	StartedAt    *time.Time                              `db:"operation_started_at"`
	FinishedAt   *time.Time                              `db:"operation_finished_at"`
}

type HistoryItem struct {
	Release
	ReleaseOperation ReleaseOperationSummary
}

type HistoryPage struct {
	Items      []HistoryItem
	NextCursor *string
}

type ListHistoryQuery struct {
	DeploymentTargetID uuid.UUID
	Caller             identity.Caller
	Limit              int
	Cursor             string
}

type SnapshotDifference struct {
	Field        string
	ReleaseValue string
	CurrentValue string
}

type Detail struct {
	Release             Release
	SnapshotDifferences []SnapshotDifference
	ReleaseOperation    releaseoperation.Record
	AuditTimeline       []audit.Record
}

type historyCursor struct {
	DeploymentTargetID uuid.UUID `json:"deploymentTargetId"`
	CreatedAt          time.Time `json:"createdAt"`
	ID                 uuid.UUID `json:"id"`
}

type historyRow struct {
	Release
	ReleaseOperationSummary
}

// ListHistory 使用不可变 `(created_at, id)` 游标稳定遍历指定 DeploymentTarget 的发布历史。
func (m *Module) ListHistory(ctx context.Context, query ListHistoryQuery) (HistoryPage, error) {
	if query.Limit < 1 || query.Limit > maximumReleaseHistoryPageSize {
		return HistoryPage{}, errors.New("release history limit must be between 1 and 100")
	}
	var cursor *historyCursor
	if query.Cursor != "" {
		decoded, err := decodeHistoryCursor(query.Cursor, query.DeploymentTargetID)
		if err != nil {
			return HistoryPage{}, err
		}
		cursor = &decoded
	}

	rows := make([]historyRow, 0, query.Limit+1)
	const selection = `SELECT r.id, r.deployment_target_id, r.image_reference,
	       r.image_artifact_id, r.target_snapshot, r.rollback_of_release_id, r.created_by, r.created_at,
	       o.id AS operation_id, o.status AS operation_status,
	       o.attempt_count AS operation_attempt_count,
	       o.error_code AS operation_error_code,
	       o.error_summary AS operation_error_summary,
	       o.queued_at AS operation_queued_at,
	       o.started_at AS operation_started_at,
	       o.finished_at AS operation_finished_at
	 FROM releases AS r
	 JOIN release_operations AS o ON o.release_id = r.id`
	err := m.authorizer.Read(ctx, query.Caller, func(tx *sqlx.Tx) error {
		var projectID uuid.UUID
		if err := tx.GetContext(ctx, &projectID, `SELECT a.project_id FROM deployment_targets d JOIN applications a ON a.id=d.application_id WHERE d.id=$1`, query.DeploymentTargetID); err != nil {
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, query.Caller, projectauth.PermissionRead); err != nil {
			return err
		}
		if cursor == nil {
			return tx.SelectContext(
				ctx,
				&rows,
				selection+`
			 WHERE r.deployment_target_id = $1
			 ORDER BY r.created_at DESC, r.id DESC
			 LIMIT $2`,
				query.DeploymentTargetID,
				query.Limit+1,
			)
		}
		return tx.SelectContext(ctx, &rows, selection+`
		 WHERE r.deployment_target_id = $1
		   AND (r.created_at, r.id) < ($2, $3)
		 ORDER BY r.created_at DESC, r.id DESC
		 LIMIT $4`,
			query.DeploymentTargetID,
			cursor.CreatedAt,
			cursor.ID,
			query.Limit+1,
		)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryPage{}, ErrDeploymentTargetNotFound
		}
		return HistoryPage{}, err
	}

	hasMore := len(rows) > query.Limit
	if hasMore {
		rows = rows[:query.Limit]
	}
	items := make([]HistoryItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, HistoryItem{Release: row.Release, ReleaseOperation: row.ReleaseOperationSummary})
	}
	page := HistoryPage{Items: items}
	if hasMore {
		next := encodeHistoryCursor(historyCursor{
			DeploymentTargetID: query.DeploymentTargetID,
			CreatedAt:          rows[len(rows)-1].CreatedAt,
			ID:                 rows[len(rows)-1].Release.ID,
		})
		page.NextCursor = &next
	}
	return page, nil
}

// GetDetail 在同一个可重复读快照中组合 Release、ReleaseOperation、快照差异与审计时间线。
func (m *Module) GetDetail(ctx context.Context, releaseID uuid.UUID, caller identity.Caller) (Detail, error) {
	var release Release
	var releaseOperationRecord releaseoperation.Record
	var current targetRecord
	var timeline []audit.Record
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &release, releaseSelect+` WHERE id = $1`, releaseID); err != nil {
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, release.TargetSnapshot.ProjectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		var err error
		releaseOperationRecord, err = m.releaseOperations.GetByReleaseInTransaction(ctx, tx, releaseID)
		if err != nil {
			return err
		}
		if err := tx.GetContext(
			ctx,
			&current,
			`SELECT applications.project_id, deployment_targets.application_id,
		        deployment_targets.stage, deployment_targets.cluster_ref,
		        deployment_targets.namespace, deployment_targets.replicas,
		        deployment_targets.container_port
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1`,
			release.DeploymentTargetID,
		); err != nil {
			return err
		}
		timeline, err = audit.ListReleaseTimeline(ctx, tx, release.ID, releaseOperationRecord.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrReleaseNotFound
		}
		return Detail{}, err
	}
	return Detail{
		Release:             release,
		SnapshotDifferences: compareTargetSnapshot(release.TargetSnapshot, current),
		ReleaseOperation:    releaseOperationRecord,
		AuditTimeline:       timeline,
	}, nil
}

func encodeHistoryCursor(cursor historyCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeHistoryCursor(value string, deploymentTargetID uuid.UUID) (historyCursor, error) {
	if len(value) > 512 {
		return historyCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return historyCursor{}, ErrInvalidCursor
	}
	var cursor historyCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.DeploymentTargetID != deploymentTargetID ||
		cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return historyCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

func compareTargetSnapshot(snapshot TargetSnapshot, current targetRecord) []SnapshotDifference {
	differences := make([]SnapshotDifference, 0)
	appendDifference := func(field string, releaseValue string, currentValue string) {
		if releaseValue != currentValue {
			differences = append(differences, SnapshotDifference{
				Field: field, ReleaseValue: releaseValue, CurrentValue: currentValue,
			})
		}
	}
	appendDifference("projectId", snapshot.ProjectID.String(), current.ProjectID.String())
	appendDifference("applicationId", snapshot.ApplicationID.String(), current.ApplicationID.String())
	appendDifference("stage", snapshot.Stage, current.Stage)
	appendDifference("clusterRef", snapshot.ClusterRef, current.ClusterRef)
	appendDifference("namespace", snapshot.Namespace, current.Namespace)
	appendDifference("replicas", strconv.Itoa(snapshot.Replicas), strconv.Itoa(current.Replicas))
	appendDifference("containerPort", strconv.Itoa(snapshot.ContainerPort), strconv.Itoa(current.ContainerPort))
	return differences
}
