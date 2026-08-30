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
	"github.com/distribution/reference"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	OperationTypeDeploy = "release.deploy"
	OperationPending    = "pending"
)

var (
	ErrDeploymentTargetNotFound = errors.New("deployment target not found")
	ErrInvalidImageReference    = errors.New("image reference must contain an OCI digest")
	ErrOperationNotFound        = errors.New("operation not found")
	ErrReleaseNotFound          = errors.New("release not found")
)

type TargetSnapshot struct {
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
	ID                 uuid.UUID      `db:"id"`
	DeploymentTargetID uuid.UUID      `db:"deployment_target_id"`
	ImageReference     string         `db:"image_reference"`
	TargetSnapshot     TargetSnapshot `db:"target_snapshot"`
	CreatedBy          string         `db:"created_by"`
	CreatedAt          time.Time      `db:"created_at"`
}

type Operation struct {
	ID             uuid.UUID  `db:"id"`
	Type           string     `db:"operation_type"`
	ReleaseID      uuid.UUID  `db:"release_id"`
	CreatedBy      string     `db:"actor_id"`
	IdempotencyKey string     `db:"idempotency_key"`
	Status         string     `db:"status"`
	AttemptCount   int        `db:"attempt_count"`
	ErrorCategory  *string    `db:"error_category"`
	ErrorSummary   *string    `db:"error_summary"`
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
	StartedAt      *time.Time `db:"started_at"`
	FinishedAt     *time.Time `db:"finished_at"`
}

type Acceptance struct {
	Release   Release
	Operation Operation
}

type CreateReleaseCommand struct {
	DeploymentTargetID uuid.UUID
	ImageReference     string
	ActorID            string
	IdempotencyKey     string
}

type targetRecord struct {
	ApplicationID uuid.UUID `db:"application_id"`
	Stage         string    `db:"stage"`
	ClusterRef    string    `db:"cluster_ref"`
	Namespace     string    `db:"namespace"`
	Replicas      int       `db:"replicas"`
	ContainerPort int       `db:"container_port"`
}

type Module struct {
	db *sqlx.DB
}

func New(db *sqlx.DB) *Module {
	return &Module{db: db}
}

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
		`SELECT application_id, stage, cluster_ref, namespace, replicas, container_port
		 FROM deployment_targets
		 WHERE id = $1`,
		command.DeploymentTargetID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrDeploymentTargetNotFound
		}
		return Acceptance{}, fmt.Errorf("load release target: %w", err)
	}

	createdAt := time.Now().UTC()
	release := Release{
		ID:                 uuid.New(),
		DeploymentTargetID: command.DeploymentTargetID,
		ImageReference:     command.ImageReference,
		TargetSnapshot: TargetSnapshot{
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
	operation := Operation{
		ID:             uuid.New(),
		Type:           OperationTypeDeploy,
		ReleaseID:      release.ID,
		CreatedBy:      command.ActorID,
		IdempotencyKey: command.IdempotencyKey,
		Status:         OperationPending,
		AttemptCount:   0,
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt,
	}

	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID:   command.ActorID,
			Operation: "release.create",
			Key:       command.IdempotencyKey,
		},
		requestHash,
		release.ID,
		createdAt,
	)
	if err != nil {
		return Acceptance{}, err
	}
	if !isNew {
		return replayReleaseCreate(ctx, tx, resourceID)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO releases
		 (id, deployment_target_id, image_reference, target_snapshot, created_by, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		release.ID,
		release.DeploymentTargetID,
		release.ImageReference,
		release.TargetSnapshot,
		release.CreatedBy,
		release.CreatedAt,
	); err != nil {
		return Acceptance{}, fmt.Errorf("insert release: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO operations
		 (id, operation_type, release_id, actor_id, idempotency_key, status,
		  attempt_count, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		operation.ID,
		operation.Type,
		operation.ReleaseID,
		operation.CreatedBy,
		operation.IdempotencyKey,
		operation.Status,
		operation.AttemptCount,
		operation.CreatedAt,
		operation.UpdatedAt,
	); err != nil {
		return Acceptance{}, fmt.Errorf("insert release operation: %w", err)
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
				"operationId":        operation.ID.String(),
			},
			CreatedAt: createdAt,
		},
	); err != nil {
		return Acceptance{}, err
	}

	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit create release: %w", err)
	}

	return Acceptance{Release: release, Operation: operation}, nil
}

func (m *Module) GetRelease(ctx context.Context, id uuid.UUID) (Release, error) {
	var release Release
	if err := m.db.GetContext(
		ctx,
		&release,
		`SELECT id, deployment_target_id, image_reference, target_snapshot,
		        created_by, created_at
		 FROM releases
		 WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Release{}, ErrReleaseNotFound
		}
		return Release{}, fmt.Errorf("get release: %w", err)
	}

	return release, nil
}

func (m *Module) GetOperation(ctx context.Context, id uuid.UUID) (Operation, error) {
	var operation Operation
	if err := m.db.GetContext(
		ctx,
		&operation,
		operationSelect+` WHERE id = $1`,
		id,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, ErrOperationNotFound
		}
		return Operation{}, fmt.Errorf("get operation: %w", err)
	}

	return operation, nil
}

func replayReleaseCreate(
	ctx context.Context,
	tx *sqlx.Tx,
	releaseID uuid.UUID,
) (Acceptance, error) {
	var release Release
	if err := tx.GetContext(
		ctx,
		&release,
		`SELECT id, deployment_target_id, image_reference, target_snapshot,
		        created_by, created_at
		 FROM releases
		 WHERE id = $1`,
		releaseID,
	); err != nil {
		return Acceptance{}, fmt.Errorf("load idempotent release result: %w", err)
	}

	var operation Operation
	if err := tx.GetContext(
		ctx,
		&operation,
		operationSelect+` WHERE release_id = $1`,
		releaseID,
	); err != nil {
		return Acceptance{}, fmt.Errorf("load idempotent operation result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit release replay: %w", err)
	}

	return Acceptance{Release: release, Operation: operation}, nil
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

const operationSelect = `SELECT id, operation_type, release_id, actor_id, idempotency_key,
       status, attempt_count,
       error_category, error_summary, created_at, updated_at, started_at, finished_at
 FROM operations`
