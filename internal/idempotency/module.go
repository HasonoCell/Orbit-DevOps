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

type Scope struct {
	ActorID   string
	Operation string
	Key       string
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
		 (actor_id, operation, idempotency_key, request_hash, resource_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (actor_id, operation, idempotency_key) DO NOTHING`,
		scope.ActorID,
		scope.Operation,
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
		 WHERE actor_id = $1 AND operation = $2 AND idempotency_key = $3`,
		scope.ActorID,
		scope.Operation,
		scope.Key,
	); err != nil {
		return uuid.Nil, false, fmt.Errorf("load idempotency record: %w", err)
	}

	if !bytes.Equal(existing.RequestHash, requestHash) {
		return uuid.Nil, false, ErrConflict
	}

	return existing.ResourceID, false, nil
}
