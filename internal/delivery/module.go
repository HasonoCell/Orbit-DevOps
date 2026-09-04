package delivery

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/distribution/reference"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrDeploymentTargetNotFound = errors.New("deployment target not found")
	ErrInvalidImageReference    = errors.New("image reference must contain an OCI digest")
	ErrReleaseNotFound          = errors.New("release not found")
	ErrInvalidCursor            = errors.New("invalid release history cursor")
)

type TargetSnapshot struct {
	ProjectID     uuid.UUID `json:"projectId"`
	ApplicationID uuid.UUID `json:"applicationId"`
	Stage         string    `json:"stage"`
	ClusterRef    string    `json:"clusterRef"`
	Namespace     string    `json:"namespace"`
	Replicas      int       `json:"replicas"`
	ContainerPort int       `json:"containerPort"`
}

func (s *TargetSnapshot) Scan(source any) error {
	var payload []byte
	switch value := source.(type) {
	case []byte:
		payload = value
	case string:
		payload = []byte(value)
	default:
		return fmt.Errorf("scan target snapshot from %T", source)
	}

	if err := json.Unmarshal(payload, s); err != nil {
		return fmt.Errorf("decode target snapshot: %w", err)
	}
	return nil
}

func (s TargetSnapshot) Value() (driver.Value, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode target snapshot: %w", err)
	}
	return payload, nil
}

type Release struct {
	ID                  uuid.UUID      `db:"id"`
	DeploymentTargetID  uuid.UUID      `db:"deployment_target_id"`
	ImageReference      string         `db:"image_reference"`
	TargetSnapshot      TargetSnapshot `db:"target_snapshot"`
	RollbackOfReleaseID *uuid.UUID     `db:"rollback_of_release_id"`
	CreatedBy           string         `db:"created_by"`
	CreatedAt           time.Time      `db:"created_at"`
}

type Acceptance struct {
	Release   Release
	Operation operation.Record
}

type CreateReleaseCommand struct {
	DeploymentTargetID uuid.UUID
	ImageReference     string
	ActorID            string
	IdempotencyKey     string
	TraceParent        string
	TraceState         string
}

type targetRecord struct {
	ProjectID     uuid.UUID `db:"project_id"`
	ApplicationID uuid.UUID `db:"application_id"`
	Stage         string    `db:"stage"`
	ClusterRef    string    `db:"cluster_ref"`
	Namespace     string    `db:"namespace"`
	Replicas      int       `db:"replicas"`
	ContainerPort int       `db:"container_port"`
}

type Module struct {
	db         *sqlx.DB
	operations *operation.Module
	authorizer *projectauth.Module
}

// New 创建交付模块，并将发布命令接入统一项目授权。
func New(
	db *sqlx.DB,
	operations *operation.Module,
	authorizer *projectauth.Module,
) *Module {
	return &Module{db: db, operations: operations, authorizer: authorizer}
}

// CreateRelease 原子完成授权、幂等接纳、快照冻结和 Operation 创建。
func (m *Module) CreateRelease(
	ctx context.Context,
	command CreateReleaseCommand,
) (Acceptance, error) {
	if err := validateImageReference(command.ImageReference); err != nil {
		return Acceptance{}, err
	}

	requestHash, err := idempotency.Fingerprint(struct {
		DeploymentTargetID uuid.UUID `json:"deploymentTargetId"`
		ImageReference     string    `json:"imageReference"`
	}{
		DeploymentTargetID: command.DeploymentTargetID,
		ImageReference:     command.ImageReference,
	})
	if err != nil {
		return Acceptance{}, fmt.Errorf("fingerprint create release: %w", err)
	}

	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Acceptance{}, fmt.Errorf("begin create release: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var target targetRecord
	if err := tx.GetContext(
		ctx,
		&target,
		`SELECT applications.project_id, deployment_targets.application_id,
		        deployment_targets.stage, deployment_targets.cluster_ref,
		        deployment_targets.namespace, deployment_targets.replicas,
		        deployment_targets.container_port
		 FROM deployment_targets
		 JOIN applications ON applications.id = deployment_targets.application_id
		 WHERE deployment_targets.id = $1`,
		command.DeploymentTargetID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrDeploymentTargetNotFound
		}
		return Acceptance{}, fmt.Errorf("load release target: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		target.ProjectID,
		command.ActorID,
		projectauth.PermissionDevelop,
	); err != nil {
		return Acceptance{}, err
	}

	createdAt := time.Now().UTC()
	release := Release{
		ID:                 uuid.New(),
		DeploymentTargetID: command.DeploymentTargetID,
		ImageReference:     command.ImageReference,
		TargetSnapshot: TargetSnapshot{
			ProjectID:     target.ProjectID,
			ApplicationID: target.ApplicationID,
			Stage:         target.Stage,
			ClusterRef:    target.ClusterRef,
			Namespace:     target.Namespace,
			Replicas:      target.Replicas,
			ContainerPort: target.ContainerPort,
		},
		CreatedBy: command.ActorID,
		CreatedAt: createdAt,
	}
	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:     command.ActorID,
			CommandType: "release.create",
			Key:         command.IdempotencyKey,
		},
		requestHash,
		release.ID,
		createdAt,
	)
	if err != nil {
		return Acceptance{}, err
	}
	if !isNew {
		return m.replayReleaseCreate(ctx, tx, resourceID)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO releases
		 (id, deployment_target_id, image_reference, target_snapshot,
		  rollback_of_release_id, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		release.ID,
		release.DeploymentTargetID,
		release.ImageReference,
		release.TargetSnapshot,
		release.RollbackOfReleaseID,
		release.CreatedBy,
		release.CreatedAt,
	); err != nil {
		return Acceptance{}, fmt.Errorf("insert release: %w", err)
	}

	createdOperation, err := m.operations.CreatePending(
		ctx,
		tx,
		operation.CreatePendingCommand{
			ID:                 uuid.New(),
			ReleaseID:          release.ID,
			DeploymentTargetID: release.DeploymentTargetID,
			ActorID:            command.ActorID,
			IdempotencyKey:     command.IdempotencyKey,
			TraceParent:        command.TraceParent,
			TraceState:         command.TraceState,
			CreatedAt:          createdAt,
		},
	)
	if err != nil {
		return Acceptance{}, err
	}

	if err := audit.Append(
		ctx,
		tx,
		audit.Entry{
			ActorID:    command.ActorID,
			Action:     "release.create",
			TargetType: "release",
			TargetID:   release.ID,
			Summary: map[string]string{
				"deploymentTargetId": command.DeploymentTargetID.String(),
				"idempotencyKey":     command.IdempotencyKey,
				"operationId":        createdOperation.ID.String(),
			},
			CreatedAt: createdAt,
		},
	); err != nil {
		return Acceptance{}, err
	}

	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit create release: %w", err)
	}

	return Acceptance{Release: release, Operation: createdOperation}, nil
}

// GetRelease 读取不可变发布；用户可见性由调用入口统一判断。
func (m *Module) GetRelease(ctx context.Context, id uuid.UUID) (Release, error) {
	var release Release
	if err := m.db.GetContext(
		ctx,
		&release,
		releaseSelect+` WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Release{}, ErrReleaseNotFound
		}
		return Release{}, fmt.Errorf("get release: %w", err)
	}

	return release, nil
}

// IsKnownReleaseForTarget 只回答历史归属事实，供恢复流程判断当前资源是否是可安全覆盖的前序发布。
func (m *Module) IsKnownReleaseForTarget(
	ctx context.Context,
	releaseID uuid.UUID,
	deploymentTargetID uuid.UUID,
) (bool, error) {
	var exists bool
	if err := m.db.GetContext(
		ctx,
		&exists,
		`SELECT EXISTS (
		    SELECT 1 FROM releases
		    WHERE id = $1 AND deployment_target_id = $2
		)`,
		releaseID,
		deploymentTargetID,
	); err != nil {
		return false, fmt.Errorf("check release target history: %w", err)
	}
	return exists, nil
}

func (m *Module) replayReleaseCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	releaseID uuid.UUID,
) (Acceptance, error) {
	var release Release
	if err := tx.GetContext(
		ctx,
		&release,
		releaseSelect+` WHERE id = $1`,
		releaseID,
	); err != nil {
		return Acceptance{}, fmt.Errorf("load idempotent release result: %w", err)
	}

	existingOperation, err := m.operations.GetByReleaseInTransaction(ctx, tx, releaseID)
	if err != nil {
		return Acceptance{}, fmt.Errorf("load idempotent operation result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit release replay: %w", err)
	}

	return Acceptance{Release: release, Operation: existingOperation}, nil
}

func validateImageReference(imageReference string) error {
	named, err := reference.ParseNormalizedNamed(imageReference)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidImageReference, err)
	}
	if _, ok := named.(reference.Digested); !ok {
		return ErrInvalidImageReference
	}
	return nil
}

const releaseSelect = `SELECT id, deployment_target_id, image_reference, target_snapshot,
       rollback_of_release_id, created_by, created_at
 FROM releases`
