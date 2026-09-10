package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const detailSelect = `SELECT p.id, p.project_id, p.application_id, p.name, p.current_revision,
	p.enabled, p.activation_generation, p.created_by, p.created_at, p.updated_at,
	r.delivery_pipeline_id, r.revision, r.provider, r.endpoint_key, r.repository_id,
	r.repository_owner_id, r.repository_full_name, r.repository_url, r.git_ref,
	r.dockerfile_path, r.context_path, r.platform, r.mode, r.deployment_target_id,
	r.created_by AS revision_created_by, r.created_at AS revision_created_at
	FROM delivery_pipelines p
	JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=p.current_revision`

type detailRow struct {
	Record
	Revision
}

// Get 返回当前 Revision；非成员与不存在统一隐藏为 NotFound。
func (m *Module) Get(ctx context.Context, id uuid.UUID, actorID string) (Detail, error) {
	detail, err := m.getWith(ctx, m.db, id)
	if err != nil {
		return Detail{}, err
	}
	if err := m.authorizer.Require(ctx, detail.Pipeline.ProjectID, actorID, projectauth.PermissionRead); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, err
	}
	return detail, nil
}

// List 返回应用下 Pipeline 的稳定名称顺序；S5 的配置数量受应用边界约束。
func (m *Module) List(ctx context.Context, applicationID uuid.UUID, actorID string) ([]Detail, error) {
	var projectID uuid.UUID
	if err := m.db.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id=$1`, applicationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrApplicationNotFound
		}
		return nil, fmt.Errorf("load delivery pipeline application: %w", err)
	}
	if err := m.authorizer.Require(ctx, projectID, actorID, projectauth.PermissionRead); err != nil {
		return nil, err
	}
	rows := make([]detailRow, 0)
	if err := m.db.SelectContext(ctx, &rows, detailSelect+` WHERE p.application_id=$1 ORDER BY p.name, p.id`, applicationID); err != nil {
		return nil, fmt.Errorf("list delivery pipelines: %w", err)
	}
	result := make([]Detail, 0, len(rows))
	for _, row := range rows {
		result = append(result, Detail{Pipeline: row.Record, Revision: row.Revision})
	}
	return result, nil
}

func (m *Module) getWith(ctx context.Context, queryer sqlx.QueryerContext, id uuid.UUID) (Detail, error) {
	var row detailRow
	if err := sqlx.GetContext(ctx, queryer, &row, detailSelect+` WHERE p.id=$1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, fmt.Errorf("get delivery pipeline: %w", err)
	}
	return Detail{Pipeline: row.Record, Revision: row.Revision}, nil
}

func (m *Module) getWithLock(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (Detail, error) {
	var row detailRow
	if err := tx.GetContext(ctx, &row, detailSelect+` WHERE p.id=$1 FOR UPDATE OF p`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, fmt.Errorf("lock delivery pipeline: %w", err)
	}
	return Detail{Pipeline: row.Record, Revision: row.Revision}, nil
}
