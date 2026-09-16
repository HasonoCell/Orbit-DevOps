package catalog

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var ErrInvalidCursor = errors.New("invalid catalog cursor")

type ApplicationPage struct {
	Items      []Application
	NextCursor *string
}

type DeploymentTargetPage struct {
	Items      []DeploymentTarget
	NextCursor *string
}

type catalogCursor struct {
	Scope      string    `json:"scope"`
	ResourceID uuid.UUID `json:"resourceId"`
	CreatedAt  time.Time `json:"createdAt"`
	ID         uuid.UUID `json:"id"`
}

func (m *Module) ListApplications(ctx context.Context, projectID uuid.UUID, caller identity.Caller,
	limit int, cursorValue string) (ApplicationPage, error) {
	cursor, err := parseCatalogCursor(cursorValue, "applications", projectID, limit)
	if err != nil {
		return ApplicationPage{}, err
	}
	items := make([]Application, 0, limit+1)
	err = m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		query := `SELECT id,project_id,name,slug,created_by,created_at FROM applications WHERE project_id=$1`
		args := []any{projectID, limit + 1}
		if cursor == nil {
			query += ` ORDER BY created_at DESC,id DESC LIMIT $2`
		} else {
			query += ` AND (created_at,id)<($2,$3) ORDER BY created_at DESC,id DESC LIMIT $4`
			args = []any{projectID, cursor.CreatedAt, cursor.ID, limit + 1}
		}
		return tx.SelectContext(ctx, &items, query, args...)
	})
	if err != nil {
		return ApplicationPage{}, err
	}
	items, next := applicationPage(items, limit, projectID)
	return ApplicationPage{Items: items, NextCursor: next}, nil
}

func (m *Module) ListDeploymentTargets(ctx context.Context, applicationID uuid.UUID, caller identity.Caller,
	limit int, cursorValue string) (DeploymentTargetPage, error) {
	cursor, err := parseCatalogCursor(cursorValue, "deployment-targets", applicationID, limit)
	if err != nil {
		return DeploymentTargetPage{}, err
	}
	items := make([]DeploymentTarget, 0, limit+1)
	err = m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		var projectID uuid.UUID
		if err := tx.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id=$1`, applicationID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrApplicationNotFound
			}
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		query := `SELECT d.id,a.project_id,d.application_id,d.stage,d.cluster_ref,d.namespace,d.replicas,
			d.container_port,d.created_by,d.created_at,d.updated_at FROM deployment_targets d
			JOIN applications a ON a.id=d.application_id WHERE d.application_id=$1`
		args := []any{applicationID, limit + 1}
		if cursor == nil {
			query += ` ORDER BY d.created_at DESC,d.id DESC LIMIT $2`
		} else {
			query += ` AND (d.created_at,d.id)<($2,$3) ORDER BY d.created_at DESC,d.id DESC LIMIT $4`
			args = []any{applicationID, cursor.CreatedAt, cursor.ID, limit + 1}
		}
		return tx.SelectContext(ctx, &items, query, args...)
	})
	if err != nil {
		return DeploymentTargetPage{}, err
	}
	items, next := deploymentTargetPage(items, limit, applicationID)
	return DeploymentTargetPage{Items: items, NextCursor: next}, nil
}

func parseCatalogCursor(value, scope string, resourceID uuid.UUID, limit int) (*catalogCursor, error) {
	if limit < 1 || limit > 100 || resourceID == uuid.Nil {
		return nil, ErrInvalidCursor
	}
	if value == "" {
		return nil, nil
	}
	if len(value) > 512 {
		return nil, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var cursor catalogCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Scope != scope || cursor.ResourceID != resourceID ||
		cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return nil, ErrInvalidCursor
	}
	return &cursor, nil
}

func encodeCatalogCursor(cursor catalogCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func applicationPage(items []Application, limit int, resourceID uuid.UUID) ([]Application, *string) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	value := encodeCatalogCursor(catalogCursor{Scope: "applications", ResourceID: resourceID, CreatedAt: last.CreatedAt, ID: last.ID})
	return items, &value
}

func deploymentTargetPage(items []DeploymentTarget, limit int, resourceID uuid.UUID) ([]DeploymentTarget, *string) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	last := items[len(items)-1]
	value := encodeCatalogCursor(catalogCursor{Scope: "deployment-targets", ResourceID: resourceID, CreatedAt: last.CreatedAt, ID: last.ID})
	return items, &value
}
