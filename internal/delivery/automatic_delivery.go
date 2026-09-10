package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type DeliveryRunCreateCommand struct {
	ReleaseID          uuid.UUID
	ReleaseOperationID uuid.UUID
	DeliveryRunID      uuid.UUID
	ActorID            string
	TraceParent        string
	TraceState         string
	CreatedAt          time.Time
}

// CreateForDeliveryRun 从已锁定 Run 的冻结 Artifact 与 Target 创建 Release，调用者不能注入镜像或目标。
func (m *Module) CreateForDeliveryRun(ctx context.Context, tx *sqlx.Tx, command DeliveryRunCreateCommand) (Acceptance, error) {
	var evidence struct {
		PipelineID        uuid.UUID  `db:"delivery_pipeline_id"`
		ProjectID         uuid.UUID  `db:"project_id"`
		ApplicationID     uuid.UUID  `db:"application_id"`
		TargetID          uuid.UUID  `db:"deployment_target_id"`
		ArtifactID        uuid.UUID  `db:"image_artifact_id"`
		ImageReference    string     `db:"image_reference"`
		Stage             string     `db:"stage"`
		ClusterRef        string     `db:"cluster_ref"`
		Namespace         string     `db:"namespace"`
		Replicas          int        `db:"replicas"`
		ContainerPort     int        `db:"container_port"`
		ConfiguredBy      string     `db:"configured_by"`
		WebhookDeliveryID *uuid.UUID `db:"webhook_delivery_id"`
	}
	if err := tx.GetContext(ctx, &evidence, `SELECT p.id AS delivery_pipeline_id,p.project_id,p.application_id,
		r.deployment_target_id,dr.image_artifact_id,a.image_reference,t.stage,t.cluster_ref,t.namespace,t.replicas,t.container_port,
		r.created_by AS configured_by,dr.webhook_delivery_id
		FROM delivery_runs dr JOIN delivery_pipelines p ON p.id=dr.delivery_pipeline_id
		JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=dr.pipeline_revision
		JOIN image_artifacts a ON a.id=dr.image_artifact_id
		JOIN deployment_targets t ON t.id=r.deployment_target_id
		WHERE dr.id=$1 AND dr.phase='artifact_ready' AND dr.release_id IS NULL
		AND p.enabled AND p.current_revision=dr.pipeline_revision AND p.activation_generation=dr.activation_generation
		AND r.mode='auto_release' AND a.project_id=p.project_id AND a.application_id=p.application_id
		AND t.application_id=p.application_id`, command.DeliveryRunID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrImageArtifactMismatch
		}
		return Acceptance{}, fmt.Errorf("load automatic release evidence: %w", err)
	}
	if err := validateImageReference(evidence.ImageReference); err != nil {
		return Acceptance{}, err
	}
	release := Release{ID: command.ReleaseID, DeploymentTargetID: evidence.TargetID, ImageReference: evidence.ImageReference, ImageArtifactID: &evidence.ArtifactID, TargetSnapshot: TargetSnapshot{ProjectID: evidence.ProjectID, ApplicationID: evidence.ApplicationID, Stage: evidence.Stage, ClusterRef: evidence.ClusterRef, Namespace: evidence.Namespace, Replicas: evidence.Replicas, ContainerPort: evidence.ContainerPort}, CreatedBy: command.ActorID, CreatedAt: command.CreatedAt}
	if _, err := tx.ExecContext(ctx, `INSERT INTO releases
		(id,deployment_target_id,image_reference,image_artifact_id,target_snapshot,rollback_of_release_id,created_by,created_at)
		VALUES ($1,$2,$3,$4,$5,NULL,$6,$7)`, release.ID, release.DeploymentTargetID, release.ImageReference, release.ImageArtifactID, release.TargetSnapshot, release.CreatedBy, release.CreatedAt); err != nil {
		return Acceptance{}, fmt.Errorf("insert automatic release: %w", err)
	}
	operation, err := m.releaseOperations.CreatePending(ctx, tx, releaseoperation.CreatePendingCommand{ID: command.ReleaseOperationID, ReleaseID: release.ID, DeploymentTargetID: release.DeploymentTargetID, ActorID: command.ActorID, IdempotencyKey: "delivery-run:" + command.DeliveryRunID.String(), TraceParent: command.TraceParent, TraceState: command.TraceState, CreatedAt: command.CreatedAt})
	if err != nil {
		return Acceptance{}, err
	}
	summary := map[string]string{"configuredBy": evidence.ConfiguredBy, "deploymentTargetId": evidence.TargetID.String(), "deliveryPipelineId": evidence.PipelineID.String(), "deliveryRunId": command.DeliveryRunID.String(), "operationId": operation.ID.String()}
	if evidence.WebhookDeliveryID != nil {
		summary["webhookDeliveryId"] = evidence.WebhookDeliveryID.String()
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.ActorID, ActorKind: audit.ActorKindSystem, Action: "release.create", TargetType: "release", TargetID: release.ID, Summary: summary, CreatedAt: command.CreatedAt}); err != nil {
		return Acceptance{}, err
	}
	return Acceptance{Release: release, ReleaseOperation: operation}, nil
}
