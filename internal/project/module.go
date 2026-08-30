package project

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Project struct {
	ID        uuid.UUID
	Name      string
	Slug      string
	CreatedBy string
	CreatedAt time.Time
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

func New(db *sqlx.DB) *Module {
	return &Module{db: db}
}

func (m *Module) Create(ctx context.Context, command CreateCommand) (Project, error) {
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
