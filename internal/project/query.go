package project

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var ErrInvalidCursor = errors.New("invalid project cursor")

type Page struct {
	Items      []Project
	NextCursor *string
}

type projectCursor struct {
	Scope     string    `json:"scope"`
	UserID    uuid.UUID `json:"userId"`
	CreatedAt time.Time `json:"createdAt"`
	ID        uuid.UUID `json:"id"`
}

// List 只遍历当前 User 的成员项目；平台管理员不会获得全项目旁路。
func (m *Module) List(ctx context.Context, caller identity.Caller, limit int, cursorValue string) (Page, error) {
	if limit < 1 || limit > 100 || caller.UserID() == uuid.Nil {
		return Page{}, ErrInvalidCursor
	}
	var cursor *projectCursor
	if cursorValue != "" {
		decoded, err := decodeProjectCursor(cursorValue, caller.UserID())
		if err != nil {
			return Page{}, err
		}
		cursor = &decoded
	}
	items := make([]Project, 0, limit+1)
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		query := `SELECT p.id,p.name,p.slug,p.created_by,p.created_at
			FROM projects p JOIN project_members pm ON pm.project_id=p.id
			WHERE pm.user_id=$1 AND p.identity_state='governed'`
		args := []any{caller.UserID(), limit + 1}
		if cursor == nil {
			query += ` ORDER BY p.created_at DESC,p.id DESC LIMIT $2`
		} else {
			query += ` AND (p.created_at,p.id)<($2,$3) ORDER BY p.created_at DESC,p.id DESC LIMIT $4`
			args = []any{caller.UserID(), cursor.CreatedAt, cursor.ID, limit + 1}
		}
		return tx.SelectContext(ctx, &items, query, args...)
	})
	if err != nil {
		return Page{}, err
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	page := Page{Items: items}
	if hasMore {
		value := encodeProjectCursor(projectCursor{Scope: "projects", UserID: caller.UserID(),
			CreatedAt: items[len(items)-1].CreatedAt, ID: items[len(items)-1].ID})
		page.NextCursor = &value
	}
	return page, nil
}

func encodeProjectCursor(cursor projectCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeProjectCursor(value string, userID uuid.UUID) (projectCursor, error) {
	if len(value) > 512 {
		return projectCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return projectCursor{}, ErrInvalidCursor
	}
	var cursor projectCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Scope != "projects" || cursor.UserID != userID ||
		cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return projectCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}
