package pipeline

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Page struct {
	Items      []Detail
	NextCursor *string
}

type pipelineCursor struct {
	Scope         string    `json:"scope"`
	ApplicationID uuid.UUID `json:"applicationId"`
	CreatedAt     time.Time `json:"createdAt"`
	ID            uuid.UUID `json:"id"`
}

const detailSelect = `SELECT p.id AS pipeline_id, p.project_id, p.application_id, p.name, p.current_revision,
	p.enabled, p.activation_generation, p.created_by, p.created_at, p.updated_at,
	r.delivery_pipeline_id, r.revision, r.provider, r.endpoint_key, r.repository_id,
	r.repository_owner_id, r.repository_full_name, r.repository_url, r.git_ref,
	r.dockerfile_path, r.context_path, r.platform, r.mode, r.deployment_target_id,
	r.created_by AS revision_created_by, r.created_at AS revision_created_at
	FROM delivery_pipelines p
	JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=p.current_revision`

type detailRow struct {
	PipelineID           uuid.UUID  `db:"pipeline_id"`
	ProjectID            uuid.UUID  `db:"project_id"`
	ApplicationID        uuid.UUID  `db:"application_id"`
	Name                 string     `db:"name"`
	CurrentRevision      int        `db:"current_revision"`
	Enabled              bool       `db:"enabled"`
	ActivationGeneration int64      `db:"activation_generation"`
	CreatedBy            string     `db:"created_by"`
	CreatedAt            time.Time  `db:"created_at"`
	UpdatedAt            time.Time  `db:"updated_at"`
	RevisionPipelineID   uuid.UUID  `db:"delivery_pipeline_id"`
	RevisionNumber       int        `db:"revision"`
	Provider             string     `db:"provider"`
	EndpointKey          string     `db:"endpoint_key"`
	RepositoryID         int64      `db:"repository_id"`
	RepositoryOwnerID    int64      `db:"repository_owner_id"`
	RepositoryFullName   string     `db:"repository_full_name"`
	RepositoryURL        string     `db:"repository_url"`
	GitRef               string     `db:"git_ref"`
	DockerfilePath       string     `db:"dockerfile_path"`
	ContextPath          string     `db:"context_path"`
	Platform             string     `db:"platform"`
	Mode                 string     `db:"mode"`
	DeploymentTargetID   *uuid.UUID `db:"deployment_target_id"`
	RevisionCreatedBy    string     `db:"revision_created_by"`
	RevisionCreatedAt    time.Time  `db:"revision_created_at"`
}

func (row detailRow) detail() Detail {
	return Detail{
		Pipeline: Record{ID: row.PipelineID, ProjectID: row.ProjectID, ApplicationID: row.ApplicationID,
			Name: row.Name, CurrentRevision: row.CurrentRevision, Enabled: row.Enabled,
			ActivationGeneration: row.ActivationGeneration, CreatedBy: row.CreatedBy,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt},
		Revision: Revision{PipelineID: row.RevisionPipelineID, Revision: row.RevisionNumber,
			Provider: row.Provider, EndpointKey: row.EndpointKey, RepositoryID: row.RepositoryID,
			RepositoryOwnerID: row.RepositoryOwnerID, RepositoryFullName: row.RepositoryFullName,
			RepositoryURL: row.RepositoryURL, GitRef: row.GitRef, DockerfilePath: row.DockerfilePath,
			ContextPath: row.ContextPath, Platform: row.Platform, Mode: row.Mode,
			DeploymentTargetID: row.DeploymentTargetID, CreatedBy: row.RevisionCreatedBy,
			CreatedAt: row.RevisionCreatedAt},
	}
}

// Get 返回当前 Revision；非成员与不存在统一隐藏为 NotFound。
func (m *Module) Get(ctx context.Context, id uuid.UUID, caller identity.Caller) (Detail, error) {
	var detail Detail
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		var err error
		detail, err = m.getWith(ctx, tx, id)
		if err != nil {
			return err
		}
		return m.authorizer.RequireAuthorizedInTransaction(ctx, tx, detail.Pipeline.ProjectID, caller, projectauth.PermissionRead)
	})
	if err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, err
	}
	return detail, nil
}

// List 以绑定 Application 的游标返回 Pipeline，游标只定位位置、不携带授权能力。
func (m *Module) List(ctx context.Context, applicationID uuid.UUID, caller identity.Caller,
	limit int, cursorValue string) (Page, error) {
	if limit < 1 || limit > 100 || applicationID == uuid.Nil {
		return Page{}, ErrInvalidCursor
	}
	var cursor *pipelineCursor
	if cursorValue != "" {
		decoded, err := decodePipelineCursor(cursorValue, applicationID)
		if err != nil {
			return Page{}, err
		}
		cursor = &decoded
	}
	rows := make([]detailRow, 0, limit+1)
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		var projectID uuid.UUID
		if err := tx.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id=$1`, applicationID); err != nil {
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		query := detailSelect + ` WHERE p.application_id=$1`
		args := []any{applicationID, limit + 1}
		if cursor == nil {
			query += ` ORDER BY p.created_at DESC,p.id DESC LIMIT $2`
		} else {
			query += ` AND (p.created_at,p.id)<($2,$3) ORDER BY p.created_at DESC,p.id DESC LIMIT $4`
			args = []any{applicationID, cursor.CreatedAt, cursor.ID, limit + 1}
		}
		return tx.SelectContext(ctx, &rows, query, args...)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Page{}, ErrApplicationNotFound
		}
		return Page{}, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	result := make([]Detail, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.detail())
	}
	page := Page{Items: result}
	if hasMore {
		last := rows[len(rows)-1]
		value := encodePipelineCursor(pipelineCursor{Scope: "delivery-pipelines", ApplicationID: applicationID,
			CreatedAt: last.CreatedAt, ID: last.PipelineID})
		page.NextCursor = &value
	}
	return page, nil
}

func encodePipelineCursor(cursor pipelineCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodePipelineCursor(value string, applicationID uuid.UUID) (pipelineCursor, error) {
	if len(value) > 512 {
		return pipelineCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return pipelineCursor{}, ErrInvalidCursor
	}
	var cursor pipelineCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Scope != "delivery-pipelines" ||
		cursor.ApplicationID != applicationID || cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return pipelineCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

func (m *Module) getWith(ctx context.Context, queryer sqlx.QueryerContext, id uuid.UUID) (Detail, error) {
	var row detailRow
	if err := sqlx.GetContext(ctx, queryer, &row, detailSelect+` WHERE p.id=$1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, fmt.Errorf("get delivery pipeline: %w", err)
	}
	return row.detail(), nil
}

func (m *Module) getWithLock(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (Detail, error) {
	var row detailRow
	if err := tx.GetContext(ctx, &row, detailSelect+` WHERE p.id=$1 FOR UPDATE OF p`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, ErrNotFound
		}
		return Detail{}, fmt.Errorf("lock delivery pipeline: %w", err)
	}
	return row.detail(), nil
}
