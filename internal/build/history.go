package build

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
)

const maximumHistoryPageSize = 100

type HistoryPage struct {
	Items      []Acceptance
	NextCursor *string
}

type ListHistoryQuery struct {
	ApplicationID uuid.UUID
	ActorID       string
	Limit         int
	Cursor        string
}

type historyCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        uuid.UUID `json:"id"`
}

// ListHistory 使用不可变的 `(created_at, id)` 游标稳定返回一个 Application 的构建历史。
func (m *Module) ListHistory(ctx context.Context, query ListHistoryQuery) (HistoryPage, error) {
	if query.Limit < 1 || query.Limit > maximumHistoryPageSize {
		return HistoryPage{}, errors.New("build history limit must be between 1 and 100")
	}
	var projectID uuid.UUID
	if err := m.db.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id = $1`, query.ApplicationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryPage{}, ErrApplicationNotFound
		}
		return HistoryPage{}, fmt.Errorf("load build history application: %w", err)
	}
	if err := m.authorizer.Require(ctx, projectID, query.ActorID, projectauth.PermissionRead); err != nil {
		return HistoryPage{}, err
	}
	var cursor *historyCursor
	if query.Cursor != "" {
		decoded, err := decodeHistoryCursor(query.Cursor)
		if err != nil {
			return HistoryPage{}, err
		}
		cursor = &decoded
	}

	records := make([]Record, 0, query.Limit+1)
	selection := buildSelect + ` WHERE application_id = $1`
	arguments := []any{query.ApplicationID, query.Limit + 1}
	if cursor == nil {
		selection += ` ORDER BY created_at DESC, id DESC LIMIT $2`
	} else {
		selection += ` AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`
		arguments = []any{query.ApplicationID, cursor.CreatedAt, cursor.ID, query.Limit + 1}
	}
	if err := m.db.SelectContext(ctx, &records, selection, arguments...); err != nil {
		return HistoryPage{}, fmt.Errorf("list build history: %w", err)
	}
	hasMore := len(records) > query.Limit
	if hasMore {
		records = records[:query.Limit]
	}
	items := make([]Acceptance, 0, len(records))
	for _, record := range records {
		var operation buildoperation.Record
		if err := m.db.GetContext(ctx, &operation, buildOperationSelect+` WHERE build_id = $1`, record.ID); err != nil {
			return HistoryPage{}, fmt.Errorf("get build history operation: %w", err)
		}
		artifact, err := m.getArtifactForBuild(ctx, record.ID)
		if err != nil {
			return HistoryPage{}, err
		}
		items = append(items, Acceptance{Build: record, BuildOperation: operation, ImageArtifact: artifact})
	}
	page := HistoryPage{Items: items}
	if hasMore {
		value := encodeHistoryCursor(historyCursor{CreatedAt: records[len(records)-1].CreatedAt, ID: records[len(records)-1].ID})
		page.NextCursor = &value
	}
	return page, nil
}

func encodeHistoryCursor(cursor historyCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeHistoryCursor(value string) (historyCursor, error) {
	if len(value) > 512 {
		return historyCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return historyCursor{}, ErrInvalidCursor
	}
	var cursor historyCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return historyCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}
