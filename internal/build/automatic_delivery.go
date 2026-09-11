package build

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type DeliveryCreateCommand struct {
	BuildID           uuid.UUID
	BuildOperationID  uuid.UUID
	DeliveryRunID     uuid.UUID
	PipelineID        uuid.UUID
	PipelineRevision  int
	WebhookDeliveryID uuid.UUID
	SourceCommit      string
	ActorID           string
	TraceParent       string
	TraceState        string
	CreatedAt         time.Time
}

// CreateForDelivery 依据锁定的 Pipeline Revision 和可信 Webhook 证据创建 Build。
// 调用方不能传入 Application、仓库或构建路径，从而不能把 system 身份变成通用绕权入口。
func (m *Module) CreateForDelivery(ctx context.Context, tx *sqlx.Tx, command DeliveryCreateCommand) (Acceptance, error) {
	var evidence struct {
		ProjectID            uuid.UUID `db:"project_id"`
		ApplicationID        uuid.UUID `db:"application_id"`
		RepositoryURL        string    `db:"repository_url"`
		DockerfilePath       string    `db:"dockerfile_path"`
		ContextPath          string    `db:"context_path"`
		ActivationGeneration int64     `db:"activation_generation"`
		ConfiguredBy         string    `db:"configured_by"`
	}
	if err := tx.GetContext(ctx, &evidence, `SELECT p.project_id,p.application_id,r.repository_url,
		r.dockerfile_path,r.context_path,p.activation_generation,r.created_by AS configured_by
		FROM delivery_pipelines p
		JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=$2
		JOIN webhook_deliveries w ON w.id=$3
		WHERE p.id=$1 AND p.enabled AND p.current_revision=$2 AND w.state='pending'
		AND w.endpoint_key=r.endpoint_key AND w.repository_id=r.repository_id AND w.repository_owner_id=r.repository_owner_id
		AND w.git_ref=r.git_ref AND w.after_commit=$4`, command.PipelineID, command.PipelineRevision, command.WebhookDeliveryID, command.SourceCommit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrInvalidInput
		}
		return Acceptance{}, fmt.Errorf("load automatic build evidence: %w", err)
	}
	input, err := m.normalize(CreateCommand{ApplicationID: evidence.ApplicationID, RepositoryURL: evidence.RepositoryURL, SourceCommit: command.SourceCommit, DockerfilePath: evidence.DockerfilePath, ContextPath: evidence.ContextPath}, evidence.ProjectID)
	if err != nil {
		return Acceptance{}, err
	}
	fingerprint, err := idempotency.Fingerprint(input)
	if err != nil {
		return Acceptance{}, fmt.Errorf("fingerprint automatic build: %w", err)
	}
	record := Record{ID: command.BuildID, ProjectID: evidence.ProjectID, ApplicationID: evidence.ApplicationID, RepositoryURL: input.RepositoryURL, SourceCommit: input.SourceCommit, DockerfilePath: input.DockerfilePath, ContextPath: input.ContextPath, Platform: input.Platform, DestinationRepository: input.DestinationRepository, InputDigest: digestInput(fingerprint), CreatedBy: command.ActorID, CreatedAt: command.CreatedAt}
	if _, err := tx.ExecContext(ctx, `INSERT INTO builds
		(id,project_id,application_id,repository_url,source_commit,dockerfile_path,context_path,
		 platform,destination_repository,input_digest,created_by,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, record.ID, record.ProjectID, record.ApplicationID, record.RepositoryURL, record.SourceCommit, record.DockerfilePath, record.ContextPath, record.Platform, record.DestinationRepository, record.InputDigest, record.CreatedBy, record.CreatedAt); err != nil {
		return Acceptance{}, fmt.Errorf("insert automatic build: %w", err)
	}
	operation, err := m.buildOperations.CreatePending(ctx, tx, buildoperation.CreatePendingCommand{ID: command.BuildOperationID, BuildID: record.ID, ActorID: command.ActorID, IdempotencyKey: "delivery-run:" + command.DeliveryRunID.String(), TraceParent: command.TraceParent, TraceState: command.TraceState, CreatedAt: command.CreatedAt})
	if err != nil {
		return Acceptance{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.ActorID, ActorKind: audit.ActorKindSystem, Action: "build.create", TargetType: "build", TargetID: record.ID, Summary: map[string]string{"applicationId": record.ApplicationID.String(), "buildOperationId": operation.ID.String(), "configuredBy": evidence.ConfiguredBy, "deliveryPipelineId": command.PipelineID.String(), "deliveryRunId": command.DeliveryRunID.String(), "webhookDeliveryId": command.WebhookDeliveryID.String()}, CreatedAt: command.CreatedAt}); err != nil {
		return Acceptance{}, err
	}
	return Acceptance{Build: record, BuildOperation: operation}, nil
}
