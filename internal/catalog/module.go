package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrApplicationNotFound      = errors.New("application not found")
	ErrDeploymentTargetNotFound = errors.New("deployment target not found")
	ErrProjectNotFound          = errors.New("project not found")
)

const DevelopmentStage = "development"

type Config struct {
	ClusterRef string
	Namespace  string
}

type Application struct {
	ID        uuid.UUID `db:"id"`
	ProjectID uuid.UUID `db:"project_id"`
	Name      string    `db:"name"`
	Slug      string    `db:"slug"`
	CreatedBy string    `db:"created_by"`
	CreatedAt time.Time `db:"created_at"`
}

type CreateApplicationCommand struct {
	ProjectID      uuid.UUID
	Name           string
	Slug           string
	ActorID        string
	IdempotencyKey string
}

type DeploymentTarget struct {
	ID            uuid.UUID `db:"id"`
	ApplicationID uuid.UUID `db:"application_id"`
	Stage         string    `db:"stage"`
	ClusterRef    string    `db:"cluster_ref"`
	Namespace     string    `db:"namespace"`
	Replicas      int       `db:"replicas"`
	ContainerPort int       `db:"container_port"`
	CreatedBy     string    `db:"created_by"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

type CreateDeploymentTargetCommand struct {
	ApplicationID  uuid.UUID
	Stage          string
	Replicas       int
	ContainerPort  int
	ActorID        string
	IdempotencyKey string
}

type Module struct {
	db     *sqlx.DB
	config Config
}

func New(db *sqlx.DB, config Config) *Module {
	return &Module{db: db, config: config}
}

func (m *Module) CreateApplication(
	ctx context.Context,
	command CreateApplicationCommand,
) (Application, error) {
	requestHash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID `json:"projectId"`
		Name      string    `json:"name"`
		Slug      string    `json:"slug"`
	}{
		ProjectID: command.ProjectID,
		Name:      command.Name,
		Slug:      command.Slug,
	})
	if err != nil {
		return Application{}, fmt.Errorf("fingerprint create application: %w", err)
	}

	createdAt := time.Now().UTC()
	created := Application{
		ID:        uuid.New(),
		ProjectID: command.ProjectID,
		Name:      command.Name,
		Slug:      command.Slug,
		CreatedBy: command.ActorID,
		CreatedAt: createdAt,
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Application{}, fmt.Errorf("begin create application: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var projectExists bool
	if err := tx.GetContext(
		ctx,
		&projectExists,
		`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)`,
		command.ProjectID,
	); err != nil {
		return Application{}, fmt.Errorf("check application project: %w", err)
	}
	if !projectExists {
		return Application{}, ErrProjectNotFound
	}

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:   command.ActorID,
			Operation: "application.create",
			Key:       command.IdempotencyKey,
		},
		requestHash,
		created.ID,
		createdAt,
	)
	if err != nil {
		return Application{}, err
	}
	if !isNew {
		return replayApplicationCreate(ctx, tx, resourceID)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO applications
		 (id, project_id, name, slug, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		created.ID,
		created.ProjectID,
		created.Name,
		created.Slug,
		created.CreatedBy,
		created.CreatedAt,
	); err != nil {
		return Application{}, fmt.Errorf("insert application: %w", err)
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.ActorID,
			Action:     "application.create",
			TargetType: "application",
			TargetID:   created.ID,
			Summary: map[string]string{
				"applicationSlug": command.Slug,
				"idempotencyKey":  command.IdempotencyKey,
				"projectId":       command.ProjectID.String(),
			},
			CreatedAt: createdAt,
		},
	); err != nil {
		return Application{}, err
	}

	if err := tx.Commit(); err != nil {
		return Application{}, fmt.Errorf("commit create application: %w", err)
	}

	return created, nil
}

func (m *Module) GetApplication(ctx context.Context, id uuid.UUID) (Application, error) {
	var application Application
	if err := m.db.GetContext(
		ctx,
		&application,
		`SELECT id, project_id, name, slug, created_by, created_at
		 FROM applications
		 WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Application{}, ErrApplicationNotFound
		}
		return Application{}, fmt.Errorf("get application: %w", err)
	}

	return application, nil
}

func (m *Module) CreateDeploymentTarget(
	ctx context.Context,
	command CreateDeploymentTargetCommand,
) (DeploymentTarget, error) {
	requestHash, err := idempotency.Fingerprint(struct {
		ApplicationID uuid.UUID `json:"applicationId"`
		Stage         string    `json:"stage"`
		Replicas      int       `json:"replicas"`
		ContainerPort int       `json:"containerPort"`
	}{
		ApplicationID: command.ApplicationID,
		Stage:         command.Stage,
		Replicas:      command.Replicas,
		ContainerPort: command.ContainerPort,
	})
	if err != nil {
		return DeploymentTarget{}, fmt.Errorf("fingerprint create deployment target: %w", err)
	}

	createdAt := time.Now().UTC()
	created := DeploymentTarget{
		ID:            uuid.New(),
		ApplicationID: command.ApplicationID,
		Stage:         command.Stage,
		ClusterRef:    m.config.ClusterRef,
		Namespace:     m.config.Namespace,
		Replicas:      command.Replicas,
		ContainerPort: command.ContainerPort,
		CreatedBy:     command.ActorID,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return DeploymentTarget{}, fmt.Errorf("begin create deployment target: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var applicationExists bool
	if err := tx.GetContext(
		ctx,
		&applicationExists,
		`SELECT EXISTS (SELECT 1 FROM applications WHERE id = $1)`,
		command.ApplicationID,
	); err != nil {
		return DeploymentTarget{}, fmt.Errorf("check deployment target application: %w", err)
	}
	if !applicationExists {
		return DeploymentTarget{}, ErrApplicationNotFound
	}

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:   command.ActorID,
			Operation: "deployment_target.create",
			Key:       command.IdempotencyKey,
		},
		requestHash,
		created.ID,
		createdAt,
	)
	if err != nil {
		return DeploymentTarget{}, err
	}
	if !isNew {
		return replayDeploymentTargetCreate(ctx, tx, resourceID)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO deployment_targets
		 (id, application_id, stage, cluster_ref, namespace, replicas, container_port,
		  created_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		created.ID,
		created.ApplicationID,
		created.Stage,
		created.ClusterRef,
		created.Namespace,
		created.Replicas,
		created.ContainerPort,
		created.CreatedBy,
		created.CreatedAt,
		created.UpdatedAt,
	); err != nil {
		return DeploymentTarget{}, fmt.Errorf("insert deployment target: %w", err)
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.ActorID,
			Action:     "deployment_target.create",
			TargetType: "deployment_target",
			TargetID:   created.ID,
			Summary: map[string]string{
				"applicationId":  command.ApplicationID.String(),
				"idempotencyKey": command.IdempotencyKey,
				"stage":          command.Stage,
			},
			CreatedAt: createdAt,
		},
	); err != nil {
		return DeploymentTarget{}, err
	}

	if err := tx.Commit(); err != nil {
		return DeploymentTarget{}, fmt.Errorf("commit create deployment target: %w", err)
	}

	return created, nil
}

func (m *Module) GetDeploymentTarget(
	ctx context.Context,
	id uuid.UUID,
) (DeploymentTarget, error) {
	var target DeploymentTarget
	if err := m.db.GetContext(
		ctx,
		&target,
		`SELECT id, application_id, stage, cluster_ref, namespace, replicas,
		        container_port, created_by, created_at, updated_at
		 FROM deployment_targets
		 WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeploymentTarget{}, ErrDeploymentTargetNotFound
		}
		return DeploymentTarget{}, fmt.Errorf("get deployment target: %w", err)
	}

	return target, nil
}

func replayApplicationCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	resourceID uuid.UUID,
) (Application, error) {
	var application Application
	if err := tx.GetContext(
		ctx,
		&application,
		`SELECT id, project_id, name, slug, created_by, created_at
		 FROM applications
		 WHERE id = $1`,
		resourceID,
	); err != nil {
		return Application{}, fmt.Errorf("load idempotent application result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Application{}, fmt.Errorf("commit application replay: %w", err)
	}

	return application, nil
}

func replayDeploymentTargetCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	resourceID uuid.UUID,
) (DeploymentTarget, error) {
	var target DeploymentTarget
	if err := tx.GetContext(
		ctx,
		&target,
		`SELECT id, application_id, stage, cluster_ref, namespace, replicas,
		        container_port, created_by, created_at, updated_at
		 FROM deployment_targets
		 WHERE id = $1`,
		resourceID,
	); err != nil {
		return DeploymentTarget{}, fmt.Errorf("load idempotent deployment target result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeploymentTarget{}, fmt.Errorf("commit deployment target replay: %w", err)
	}

	return target, nil
}
