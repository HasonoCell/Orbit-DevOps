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

type Record struct {
	ID         uuid.UUID       `db:"id"`
	ActorID    string          `db:"actor_id"`
	ActorKind  string          `db:"actor_kind"`
	Action     string          `db:"action"`
	TargetType string          `db:"target_type"`
	TargetID   uuid.UUID       `db:"target_id"`
	Summary    json.RawMessage `db:"summary"`
	CreatedAt  time.Time       `db:"created_at"`
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

// ListReleaseTimeline 合并 Release 与其一对一 Operation 的追加式审计，并保持稳定顺序。
func ListReleaseTimeline(
	ctx context.Context,
	db sqlx.QueryerContext,
	releaseID uuid.UUID,
	operationID uuid.UUID,
) ([]Record, error) {
	records := make([]Record, 0)
	if err := sqlx.SelectContext(
		ctx,
		db,
		&records,
		`SELECT id, actor_id, actor_kind, action, target_type, target_id, summary, created_at
		 FROM audit_records
		 WHERE (target_type = 'release' AND target_id = $1)
		    OR (target_type = 'operation' AND target_id = $2)
		 ORDER BY created_at, id`,
		releaseID,
		operationID,
	); err != nil {
		return nil, fmt.Errorf("list release audit timeline: %w", err)
	}
	return records, nil
}
