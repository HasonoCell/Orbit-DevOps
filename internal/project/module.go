package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")
	ErrNotFound            = errors.New("project not found")
)

type Project struct {
	ID        uuid.UUID `db:"id"`
	Name      string    `db:"name"`
	Slug      string    `db:"slug"`
	CreatedBy string    `db:"created_by"`
	CreatedAt time.Time `db:"created_at"`
}

type CreateCommand struct {
	Name           string
	Slug           string
	ActorID        string
	IdempotencyKey string
}

type Module struct {
	db *sqlx.DB
}

type idempotencyRecord struct {
	RequestHash []byte    `db:"request_hash"`
	ResourceID  uuid.UUID `db:"resource_id"`
}

func New(db *sqlx.DB) *Module {
	return &Module{db: db}
}

func (m *Module) Create(ctx context.Context, command CreateCommand) (Project, error) {
	requestHash, err := createRequestHash(command)
	if err != nil {
		return Project{}, err
	}

	createdAt := time.Now().UTC()
	createdProject := Project{
		ID:        uuid.New(),
		Name:      command.Name,
		Slug:      command.Slug,
		CreatedBy: command.ActorID,
		CreatedAt: createdAt,
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("begin create project: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO idempotency_records
		 (actor_id, operation, idempotency_key, request_hash, resource_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (actor_id, operation, idempotency_key) DO NOTHING`,
		command.ActorID,
		"project.create",
		command.IdempotencyKey,
		requestHash,
		createdProject.ID,
		createdAt,
	)
	if err != nil {
		return Project{}, fmt.Errorf("claim idempotency key: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return Project{}, fmt.Errorf("inspect idempotency claim: %w", err)
	}
	if rowsAffected == 0 {
		return replayProjectCreate(ctx, tx, command, requestHash)
	}

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO projects (id, name, slug, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		createdProject.ID,
		createdProject.Name,
		createdProject.Slug,
		createdProject.CreatedBy,
		createdProject.CreatedAt,
	)
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}

	auditSummary, err := json.Marshal(map[string]string{
		"idempotencyKey": command.IdempotencyKey,
		"projectSlug":    command.Slug,
	})
	if err != nil {
		return Project{}, fmt.Errorf("encode project audit summary: %w", err)
	}

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO audit_records
		 (id, actor_id, action, target_type, target_id, summary, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New(),
		command.ActorID,
		"project.create",
		"project",
		createdProject.ID,
		auditSummary,
		createdAt,
	)
	if err != nil {
		return Project{}, fmt.Errorf("insert project audit record: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit create project: %w", err)
	}

	return createdProject, nil
}

func (m *Module) Get(ctx context.Context, id uuid.UUID) (Project, error) {
	var existingProject Project
	if err := m.db.GetContext(
		ctx,
		&existingProject,
		`SELECT id, name, slug, created_by, created_at
		 FROM projects
		 WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, fmt.Errorf("get project: %w", err)
	}

	return existingProject, nil
}

func createRequestHash(command CreateCommand) ([]byte, error) {
	payload, err := json.Marshal(struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}{
		Name: command.Name,
		Slug: command.Slug,
	})
	if err != nil {
		return nil, fmt.Errorf("encode project request fingerprint: %w", err)
	}

	requestHash := sha256.Sum256(payload)
	return requestHash[:], nil
}

func replayProjectCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	command CreateCommand,
	requestHash []byte,
) (Project, error) {
	var record idempotencyRecord
	if err := tx.GetContext(
		ctx,
		&record,
		`SELECT request_hash, resource_id
		 FROM idempotency_records
		 WHERE actor_id = $1 AND operation = $2 AND idempotency_key = $3`,
		command.ActorID,
		"project.create",
		command.IdempotencyKey,
	); err != nil {
		return Project{}, fmt.Errorf("load idempotency record: %w", err)
	}

	if !bytes.Equal(record.RequestHash, requestHash) {
		return Project{}, ErrIdempotencyConflict
	}

	var existingProject Project
	if err := tx.GetContext(
		ctx,
		&existingProject,
		`SELECT id, name, slug, created_by, created_at
		 FROM projects
		 WHERE id = $1`,
		record.ResourceID,
	); err != nil {
		return Project{}, fmt.Errorf("load idempotent project result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit project replay: %w", err)
	}

	return existingProject, nil
}
