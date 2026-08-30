package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Entry struct {
	ActorID    string
	Action     string
	TargetType string
	TargetID   uuid.UUID
	Summary    any
	CreatedAt  time.Time
}

func Append(ctx context.Context, tx *sqlx.Tx, entry Entry) error {
	summary, err := json.Marshal(entry.Summary)
	if err != nil {
		return fmt.Errorf("encode %s audit summary: %w", entry.TargetType, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO audit_records
		 (id, actor_id, action, target_type, target_id, summary, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(),
		entry.ActorID,
		entry.Action,
		entry.TargetType,
		entry.TargetID,
		summary,
		entry.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert %s audit record: %w", entry.TargetType, err)
	}

	return nil
}
