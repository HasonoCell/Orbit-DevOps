package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/pgerrors"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

var (
	ErrApplicationNotFound      = errors.New("application not found")
	ErrDeploymentTargetNotFound = errors.New("deployment target not found")
	ErrInvalidStage             = errors.New("invalid deployment target stage")
	ErrTargetStageConflict      = errors.New("deployment target stage already exists")
	ErrProjectNotFound          = errors.New("project not found")
	ErrApplicationSlugConflict  = errors.New("application slug already exists in project")
)

const (
	DevelopmentStage = "development"
	ProductionStage  = "production"
)

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
	Caller         identity.Caller
	IdempotencyKey string
}

type DeploymentTarget struct {
	ID            uuid.UUID `db:"id" json:"id"`
	ProjectID     uuid.UUID `db:"project_id" json:"projectId"`
	ApplicationID uuid.UUID `db:"application_id" json:"applicationId"`
	Stage         string    `db:"stage" json:"stage"`
	ClusterRef    string    `db:"cluster_ref" json:"clusterRef"`
	Namespace     string    `db:"namespace" json:"namespace"`
	Replicas      int       `db:"replicas" json:"replicas"`
	ContainerPort int       `db:"container_port" json:"containerPort"`
	CreatedBy     string    `db:"created_by" json:"createdBy"`
	CreatedAt     time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt     time.Time `db:"updated_at" json:"updatedAt"`
}

type CreateDeploymentTargetCommand struct {
	ApplicationID  uuid.UUID
	Stage          string
	Replicas       int
	ContainerPort  int
	Caller         identity.Caller
	IdempotencyKey string
}

type UpdateDeploymentTargetCommand struct {
	ID             uuid.UUID
	Replicas       int
	ContainerPort  int
	Caller         identity.Caller
	IdempotencyKey string
}

type Module struct {
	db         *sqlx.DB
	config     Config
	authorizer *projectauth.Module
}

// New 创建应用目录模块，并通过统一权限模块保护领域写操作。
func New(db *sqlx.DB, config Config, authorizer *projectauth.Module) *Module {
	return &Module{db: db, config: config, authorizer: authorizer}
}

// CreateApplication 在项目中幂等创建应用。
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
		CreatedBy: command.Caller.ActorID(),
		CreatedAt: createdAt,
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Application{}, fmt.Errorf("begin create application: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return Application{}, err
	}

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
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		command.ProjectID,
		command.Caller,
		projectauth.PermissionDevelop,
	); err != nil {
		return Application{}, err
	}

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:     command.Caller.ActorID(),
			CommandType: "application.create",
			Key:         command.IdempotencyKey,
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
		if pgerrors.IsUniqueConstraint(err, "applications_project_id_slug_key") {
			return Application{}, ErrApplicationSlugConflict
		}
		return Application{}, fmt.Errorf("insert application: %w", err)
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.Caller.ActorID(),
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

// GetApplication 在一个身份控制快照内读取应用及核验项目可见性。
func (m *Module) GetApplication(ctx context.Context, id uuid.UUID, caller identity.Caller) (Application, error) {
	var application Application
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &application, `SELECT id, project_id, name, slug, created_by, created_at
			FROM applications WHERE id = $1`, id); err != nil {
			return err
		}
		return m.authorizer.RequireAuthorizedInTransaction(ctx, tx, application.ProjectID, caller, projectauth.PermissionRead)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Application{}, ErrApplicationNotFound
		}
		return Application{}, err
	}

	return application, nil
}

// CreateDeploymentTarget 在应用所属项目中幂等创建部署目标。
func (m *Module) CreateDeploymentTarget(
	ctx context.Context,
	command CreateDeploymentTargetCommand,
) (DeploymentTarget, error) {
	if command.Stage != DevelopmentStage && command.Stage != ProductionStage {
		return DeploymentTarget{}, ErrInvalidStage
	}
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
		CreatedBy:     command.Caller.ActorID(),
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
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return DeploymentTarget{}, err
	}

	var projectID uuid.UUID
	if err := tx.GetContext(
		ctx,
		&projectID,
		`SELECT project_id FROM applications WHERE id = $1`,
		command.ApplicationID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeploymentTarget{}, ErrApplicationNotFound
		}
		return DeploymentTarget{}, fmt.Errorf("check deployment target application: %w", err)
	}
	created.ProjectID = projectID
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		projectID,
		command.Caller,
		projectauth.PermissionDevelop,
	); err != nil {
		return DeploymentTarget{}, err
	}

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:     command.Caller.ActorID(),
			CommandType: "deployment_target.create",
			Key:         command.IdempotencyKey,
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
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "deployment_targets_application_id_stage_key" {
			return DeploymentTarget{}, ErrTargetStageConflict
		}
		return DeploymentTarget{}, fmt.Errorf("insert deployment target: %w", err)
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.Caller.ActorID(),
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

// GetDeploymentTarget 读取部署目标并同时返回权限判断所需的项目标识。
func (m *Module) GetDeploymentTarget(
	ctx context.Context,
	id uuid.UUID,
	caller identity.Caller,
) (DeploymentTarget, error) {
	var target DeploymentTarget
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &target,
			`SELECT deployment_targets.id, applications.project_id,
		        deployment_targets.application_id, deployment_targets.stage,
		        deployment_targets.cluster_ref, deployment_targets.namespace,
		        deployment_targets.replicas, deployment_targets.container_port,
		        deployment_targets.created_by, deployment_targets.created_at,
		        deployment_targets.updated_at
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1`,
			id); err != nil {
			return err
		}
		return m.authorizer.RequireAuthorizedInTransaction(ctx, tx, target.ProjectID, caller, projectauth.PermissionRead)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeploymentTarget{}, ErrDeploymentTargetNotFound
		}
		return DeploymentTarget{}, err
	}

	return target, nil
}

// UpdateDeploymentTarget 在同一事务中完成授权、幂等和目标配置更新。
func (m *Module) UpdateDeploymentTarget(
	ctx context.Context,
	command UpdateDeploymentTargetCommand,
) (DeploymentTarget, error) {
	requestHash, err := idempotency.Fingerprint(struct {
		ID            uuid.UUID `json:"id"`
		Replicas      int       `json:"replicas"`
		ContainerPort int       `json:"containerPort"`
	}{
		ID:            command.ID,
		Replicas:      command.Replicas,
		ContainerPort: command.ContainerPort,
	})
	if err != nil {
		return DeploymentTarget{}, fmt.Errorf("fingerprint update deployment target: %w", err)
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return DeploymentTarget{}, fmt.Errorf("begin update deployment target: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return DeploymentTarget{}, err
	}

	var current DeploymentTarget
	if err := tx.GetContext(
		ctx,
		&current,
		`SELECT deployment_targets.id, applications.project_id,
		        deployment_targets.application_id, deployment_targets.stage,
		        deployment_targets.cluster_ref, deployment_targets.namespace,
		        deployment_targets.replicas, deployment_targets.container_port,
		        deployment_targets.created_by, deployment_targets.created_at,
		        deployment_targets.updated_at
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1
		 FOR UPDATE OF deployment_targets`,
		command.ID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeploymentTarget{}, ErrDeploymentTargetNotFound
		}
		return DeploymentTarget{}, fmt.Errorf("lock deployment target update: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		current.ProjectID,
		command.Caller,
		projectauth.PermissionDevelop,
	); err != nil {
		return DeploymentTarget{}, err
	}

	scope := idempotency.Scope{
		ActorID:     command.Caller.ActorID(),
		CommandType: "deployment_target.update",
		Key:         command.IdempotencyKey,
	}
	updatedAt := time.Now().UTC()
	_, isNew, err := idempotency.Claim(
		ctx,
		tx,
		scope,
		requestHash,
		command.ID,
		updatedAt,
	)
	if err != nil {
		return DeploymentTarget{}, err
	}
	if !isNew {
		var replayed DeploymentTarget
		if err := idempotency.LoadResponse(ctx, tx, scope, &replayed); err != nil {
			return DeploymentTarget{}, err
		}
		if err := tx.Commit(); err != nil {
			return DeploymentTarget{}, fmt.Errorf("commit deployment target update replay: %w", err)
		}
		return replayed, nil
	}

	updated := current
	updated.Replicas = command.Replicas
	updated.ContainerPort = command.ContainerPort
	updated.UpdatedAt = updatedAt
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE deployment_targets
		 SET replicas = $1, container_port = $2, updated_at = $3
		 WHERE id = $4`,
		updated.Replicas,
		updated.ContainerPort,
		updated.UpdatedAt,
		updated.ID,
	); err != nil {
		return DeploymentTarget{}, fmt.Errorf("update deployment target: %w", err)
	}
	if current.ContainerPort != updated.ContainerPort {
		if err := access.SignalTargetPortChange(ctx, tx, updated.ID, updatedAt); err != nil {
			return DeploymentTarget{}, fmt.Errorf("queue access routing update: %w", err)
		}
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, updated); err != nil {
		return DeploymentTarget{}, err
	}
	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.Caller.ActorID(),
			Action:     "deployment_target.update",
			TargetType: "deployment_target",
			TargetID:   updated.ID,
			Summary: map[string]any{
				"applicationId":  updated.ApplicationID.String(),
				"containerPort":  updated.ContainerPort,
				"idempotencyKey": command.IdempotencyKey,
				"replicas":       updated.Replicas,
				"stage":          updated.Stage,
			},
			CreatedAt: updatedAt,
		},
	); err != nil {
		return DeploymentTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeploymentTarget{}, fmt.Errorf("commit update deployment target: %w", err)
	}
	return updated, nil
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
		`SELECT deployment_targets.id, applications.project_id,
		        deployment_targets.application_id, deployment_targets.stage,
		        deployment_targets.cluster_ref, deployment_targets.namespace,
		        deployment_targets.replicas, deployment_targets.container_port,
		        deployment_targets.created_by, deployment_targets.created_at,
		        deployment_targets.updated_at
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1`,
		resourceID,
	); err != nil {
		return DeploymentTarget{}, fmt.Errorf("load idempotent deployment target result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeploymentTarget{}, fmt.Errorf("commit deployment target replay: %w", err)
	}

	return target, nil
}
