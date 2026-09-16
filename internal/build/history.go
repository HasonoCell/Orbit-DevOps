package build

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const maximumHistoryPageSize = 100

type HistoryPage struct {
	Items      []Acceptance
	NextCursor *string
}

type ListHistoryQuery struct {
	ApplicationID uuid.UUID
	Caller        identity.Caller
	Limit         int
	Cursor        string
}

type historyCursor struct {
	ApplicationID uuid.UUID `json:"applicationId"`
	CreatedAt     time.Time `json:"createdAt"`
	ID            uuid.UUID `json:"id"`
}

// ListHistory 使用不可变的 `(created_at, id)` 游标稳定返回一个 Application 的构建历史。
func (m *Module) ListHistory(ctx context.Context, query ListHistoryQuery) (HistoryPage, error) {
	if query.Limit < 1 || query.Limit > maximumHistoryPageSize {
		return HistoryPage{}, errors.New("build history limit must be between 1 and 100")
	}
	var cursor *historyCursor
	if query.Cursor != "" {
		decoded, err := decodeHistoryCursor(query.Cursor, query.ApplicationID)
		if err != nil {
			return HistoryPage{}, err
		}
		cursor = &decoded
	}

	records := make([]Record, 0, query.Limit+1)
	items := make([]Acceptance, 0, query.Limit)
	hasMore := false
	err := m.authorizer.Read(ctx, query.Caller, func(tx *sqlx.Tx) error {
		var projectID uuid.UUID
		if err := tx.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id = $1`, query.ApplicationID); err != nil {
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, query.Caller, projectauth.PermissionRead); err != nil {
			return err
		}
		selection := buildSelect + ` WHERE application_id = $1`
		arguments := []any{query.ApplicationID, query.Limit + 1}
		if cursor == nil {
			selection += ` ORDER BY created_at DESC, id DESC LIMIT $2`
		} else {
			selection += ` AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`
			arguments = []any{query.ApplicationID, cursor.CreatedAt, cursor.ID, query.Limit + 1}
		}
		if err := tx.SelectContext(ctx, &records, selection, arguments...); err != nil {
			return err
		}
		hasMore = len(records) > query.Limit
		if hasMore {
			records = records[:query.Limit]
		}
		for _, record := range records {
			var operation buildoperation.Record
			if err := tx.GetContext(ctx, &operation, buildOperationSelect+` WHERE build_id = $1`, record.ID); err != nil {
				return err
			}
			var artifact *ImageArtifact
			var item ImageArtifact
			if err := tx.GetContext(ctx, &item, imageArtifactSelect+` WHERE build_id = $1`, record.ID); err == nil {
				artifact = &item
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			items = append(items, Acceptance{Build: record, BuildOperation: operation, ImageArtifact: artifact})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return HistoryPage{}, ErrApplicationNotFound
		}
		return HistoryPage{}, err
	}
	page := HistoryPage{Items: items}
	if hasMore {
		value := encodeHistoryCursor(historyCursor{ApplicationID: query.ApplicationID,
			CreatedAt: records[len(records)-1].CreatedAt, ID: records[len(records)-1].ID})
		page.NextCursor = &value
	}
	return page, nil
}

func encodeHistoryCursor(cursor historyCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeHistoryCursor(value string, applicationID uuid.UUID) (historyCursor, error) {
	if len(value) > 512 {
		return historyCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return historyCursor{}, ErrInvalidCursor
	}
	var cursor historyCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.ApplicationID != applicationID ||
		cursor.CreatedAt.IsZero() || cursor.ID == uuid.Nil {
		return historyCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}
