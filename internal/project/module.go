package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrIdempotencyConflict = idempotency.ErrConflict
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
	Caller         identity.Caller
	IdempotencyKey string
}

type Module struct {
	db         *sqlx.DB
	authorizer *projectauth.Module
}

// New 创建项目模块；项目创建与首个 owner 必须共享同一事务。
func New(db *sqlx.DB, authorizer *projectauth.Module) *Module {
	return &Module{db: db, authorizer: authorizer}
}

// Create 幂等创建项目，并原子建立创建者的 owner 成员关系。
func (m *Module) Create(ctx context.Context, command CreateCommand) (Project, error) {
	requestHash, err := idempotency.Fingerprint(struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}{
		Name: command.Name,
		Slug: command.Slug,
	})
	if err != nil {
		return Project{}, fmt.Errorf("fingerprint create project: %w", err)
	}

	createdAt := time.Time{}
	createdProject := Project{
		ID:        uuid.New(),
		Name:      command.Name,
		Slug:      command.Slug,
		CreatedBy: command.Caller.ActorID(),
		CreatedAt: createdAt,
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("begin create project: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return Project{}, err
	}
	if err := tx.GetContext(ctx, &createdAt, `SELECT clock_timestamp()`); err != nil {
		return Project{}, fmt.Errorf("read database time: %w", err)
	}
	createdProject.CreatedAt = createdAt

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:     command.Caller.ActorID(),
			CommandType: "project.create",
			Key:         command.IdempotencyKey,
		},
		requestHash,
		createdProject.ID,
		createdAt,
	)
	if err != nil {
		return Project{}, err
	}
	if !isNew {
		if err := m.authorizer.RequireInTransaction(ctx, tx, resourceID, command.Caller, projectauth.PermissionRead); err != nil {
			return Project{}, err
		}
		return replayProjectCreate(ctx, tx, resourceID)
	}

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO projects (id, name, slug, created_by, created_at, identity_state)
		 VALUES ($1, $2, $3, $4, $5, 'governed')`,
		createdProject.ID,
		createdProject.Name,
		createdProject.Slug,
		createdProject.CreatedBy,
		createdProject.CreatedAt,
	)
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	if err := m.authorizer.CreateInitialOwner(
		ctx,
		tx,
		createdProject.ID,
		command.Caller.UserID(),
		createdAt,
	); err != nil {
		return Project{}, err
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.Caller.ActorID(),
			Action:     "project.create",
			TargetType: "project",
			TargetID:   createdProject.ID,
			Summary: map[string]string{
				"idempotencyKey": command.IdempotencyKey,
				"projectSlug":    command.Slug,
			},
			CreatedAt: createdAt,
		},
	); err != nil {
		return Project{}, err
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit create project: %w", err)
	}

	return createdProject, nil
}

// Get 在同一短事务里核验当前身份/成员并读取项目，避免分离授权快照。
func (m *Module) Get(ctx context.Context, id uuid.UUID, caller identity.Caller) (Project, error) {
	var existingProject Project
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireInTransaction(ctx, tx, id, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		return tx.GetContext(ctx, &existingProject, `SELECT id, name, slug, created_by, created_at
			FROM projects WHERE id = $1 AND identity_state='governed'`, id)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, err
	}

	return existingProject, nil
}

func replayProjectCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	resourceID uuid.UUID,
) (Project, error) {
	var existingProject Project
	if err := tx.GetContext(
		ctx,
		&existingProject,
		`SELECT id, name, slug, created_by, created_at
		 FROM projects
		 WHERE id = $1`,
		resourceID,
	); err != nil {
		return Project{}, fmt.Errorf("load idempotent project result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit project replay: %w", err)
	}

	return existingProject, nil
}
