package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrInvalidCursor = errors.New("invalid identity cursor")

type UserPage struct {
	Items      []User
	NextCursor *string
}

type identityCursor struct {
	Scope     string    `json:"scope"`
	SubjectID uuid.UUID `json:"subjectId"`
	CreatedAt time.Time `json:"createdAt"`
	ID        uuid.UUID `json:"id"`
}

// ListUsers 只返回安全投影，游标携带固定集合范围且不承载授权能力。
func (m *Module) ListUsers(ctx context.Context, caller Caller, limit int, cursorValue string) (UserPage, error) {
	if limit < 1 || limit > 100 {
		return UserPage{}, ErrInvalidCursor
	}
	var cursor *identityCursor
	if cursorValue != "" {
		decoded, err := decodeIdentityCursor(cursorValue, "users", uuid.Nil)
		if err != nil {
			return UserPage{}, err
		}
		cursor = &decoded
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return UserPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := requireAdministrator(ctx, tx, caller, false); err != nil {
		return UserPage{}, err
	}
	items := make([]User, 0, limit+1)
	query := `SELECT id,display_name,status,platform_role,created_at FROM users`
	arguments := []any{limit + 1}
	if cursor == nil {
		query += ` ORDER BY created_at DESC,id DESC LIMIT $1`
	} else {
		query += ` WHERE (created_at,id)<($1,$2) ORDER BY created_at DESC,id DESC LIMIT $3`
		arguments = []any{cursor.CreatedAt, cursor.ID, limit + 1}
	}
	if err := tx.SelectContext(ctx, &items, query, arguments...); err != nil {
		return UserPage{}, dependencyError(err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	page := UserPage{Items: items}
	if hasMore {
		value := encodeIdentityCursor(identityCursor{Scope: "users", CreatedAt: items[len(items)-1].CreatedAt, ID: items[len(items)-1].ID})
		page.NextCursor = &value
	}
	if err := tx.Commit(); err != nil {
		return UserPage{}, dependencyError(err)
	}
	return page, nil
}

func encodeIdentityCursor(cursor identityCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeIdentityCursor(value, scope string, subjectID uuid.UUID) (identityCursor, error) {
	if len(value) > 512 {
		return identityCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return identityCursor{}, ErrInvalidCursor
	}
	var cursor identityCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Scope != scope || cursor.SubjectID != subjectID ||
		cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return identityCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}
