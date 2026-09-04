package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	ActorKindUser   = "user"
	ActorKindSystem = "system"
)

type Entry struct {
	ActorID    string
	ActorKind  string
	Action     string
	TargetType string
	TargetID   uuid.UUID
	Summary    any
	CreatedAt  time.Time
}

// Append 在调用者事务中追加一条不可变审计记录。
func Append(ctx context.Context, tx *sqlx.Tx, entry Entry) error {
	actorKind := entry.ActorKind
	if actorKind == "" {
		actorKind = ActorKindUser
	}

	summary, err := json.Marshal(entry.Summary)
	if err != nil {
		return fmt.Errorf("encode %s audit summary: %w", entry.TargetType, err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO audit_records
		 (id, actor_id, actor_kind, action, target_type, target_id, summary, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(),
		entry.ActorID,
		actorKind,
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
