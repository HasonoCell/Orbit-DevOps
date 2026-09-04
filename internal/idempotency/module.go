package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var ErrConflict = errors.New("idempotency key was already used with a different request")

type conflictRecorderKey struct{}

// ConflictRecorder 只接收稳定命令类型，避免幂等键和请求内容进入指标标签。
type ConflictRecorder interface {
	RecordIdempotencyConflict(commandType string)
}

// WithConflictRecorder 只为当前请求挂载低基数冲突计数器，不把 Actor 或幂等键写入指标。
func WithConflictRecorder(ctx context.Context, recorder ConflictRecorder) context.Context {
	return context.WithValue(ctx, conflictRecorderKey{}, recorder)
}

type Scope struct {
	ActorID     string
	CommandType string
	Key         string
}

type record struct {
	RequestHash []byte    `db:"request_hash"`
	ResourceID  uuid.UUID `db:"resource_id"`
}

func Fingerprint(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode idempotency fingerprint: %w", err)
	}

	hash := sha256.Sum256(payload)
	return hash[:], nil
}

func Claim(
	ctx context.Context,
	tx *sqlx.Tx,
	scope Scope,
	requestHash []byte,
	proposedResourceID uuid.UUID,
	createdAt time.Time,
) (uuid.UUID, bool, error) {
	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO idempotency_records
		 (actor_id, command_type, idempotency_key, request_hash, resource_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (actor_id, command_type, idempotency_key) DO NOTHING`,
		scope.ActorID,
		scope.CommandType,
		scope.Key,
		requestHash,
		proposedResourceID,
		createdAt,
	)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("inspect idempotency claim: %w", err)
	}
	if rowsAffected == 1 {
		return proposedResourceID, true, nil
	}

	var existing record
	if err := tx.GetContext(
		ctx,
		&existing,
		`SELECT request_hash, resource_id
		 FROM idempotency_records
		 WHERE actor_id = $1 AND command_type = $2 AND idempotency_key = $3`,
		scope.ActorID,
		scope.CommandType,
		scope.Key,
	); err != nil {
		return uuid.Nil, false, fmt.Errorf("load idempotency record: %w", err)
	}

	if !bytes.Equal(existing.RequestHash, requestHash) {
		if recorder, ok := ctx.Value(conflictRecorderKey{}).(ConflictRecorder); ok {
			recorder.RecordIdempotencyConflict(scope.CommandType)
		}
		return uuid.Nil, false, ErrConflict
	}

	return existing.ResourceID, false, nil
}

func StoreResponse(
	ctx context.Context,
	tx *sqlx.Tx,
	scope Scope,
	response any,
) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode idempotent response: %w", err)
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE idempotency_records
		 SET response_payload = $1
		 WHERE actor_id = $2 AND command_type = $3 AND idempotency_key = $4`,
		payload,
		scope.ActorID,
		scope.CommandType,
		scope.Key,
	)
	if err != nil {
		return fmt.Errorf("store idempotent response: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect idempotent response storage: %w", err)
	}
	if rowsAffected != 1 {
		return errors.New("idempotency record does not exist")
	}
	return nil
}

func LoadResponse(
	ctx context.Context,
	tx *sqlx.Tx,
	scope Scope,
	destination any,
) error {
	var payload []byte
	if err := tx.GetContext(
		ctx,
		&payload,
		`SELECT response_payload
		 FROM idempotency_records
		 WHERE actor_id = $1 AND command_type = $2 AND idempotency_key = $3`,
		scope.ActorID,
		scope.CommandType,
		scope.Key,
	); err != nil {
		return fmt.Errorf("load idempotent response: %w", err)
	}
	if err := json.Unmarshal(payload, destination); err != nil {
		return fmt.Errorf("decode idempotent response: %w", err)
	}
	return nil
}
