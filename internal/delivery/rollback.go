package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
)

type RollbackCommand struct {
	SourceReleaseID uuid.UUID
	ActorID         string
	IdempotencyKey  string
	TraceParent     string
	TraceState      string
}

// Rollback 从来源 Release 的完整不可变快照创建新意图，不读取或修改当前目标配置。
func (m *Module) Rollback(ctx context.Context, command RollbackCommand) (Acceptance, error) {
	requestHash, err := idempotency.Fingerprint(struct {
		SourceReleaseID uuid.UUID `json:"sourceReleaseId"`
	}{command.SourceReleaseID})
	if err != nil {
		return Acceptance{}, fmt.Errorf("fingerprint release rollback: %w", err)
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Acceptance{}, fmt.Errorf("begin release rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var source Release
	if err := tx.GetContext(
		ctx,
		&source,
		releaseSelect+` WHERE id = $1 FOR SHARE`,
		command.SourceReleaseID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrReleaseNotFound
		}
		return Acceptance{}, fmt.Errorf("load rollback source: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		source.TargetSnapshot.ProjectID,
		command.ActorID,
		projectauth.PermissionDevelop,
	); err != nil {
		return Acceptance{}, err
	}

	createdAt := time.Now().UTC()
	rollbackOf := source.ID
	release := Release{
		ID:                  uuid.New(),
		DeploymentTargetID:  source.DeploymentTargetID,
		ImageReference:      source.ImageReference,
		TargetSnapshot:      source.TargetSnapshot,
		RollbackOfReleaseID: &rollbackOf,
		CreatedBy:           command.ActorID,
		CreatedAt:           createdAt,
	}
	resourceID, isNew, err := idempotency.Claim(
		ctx,
		tx,
		idempotency.Scope{
			ActorID: command.ActorID, CommandType: "release.rollback", Key: command.IdempotencyKey,
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
		return Acceptance{}, fmt.Errorf("insert rollback release: %w", err)
	}
	createdOperation, err := m.operations.CreatePending(ctx, tx, operation.CreatePendingCommand{
		ID:                 uuid.New(),
		ReleaseID:          release.ID,
		DeploymentTargetID: release.DeploymentTargetID,
		ActorID:            command.ActorID,
		IdempotencyKey:     command.IdempotencyKey,
		TraceParent:        command.TraceParent,
		TraceState:         command.TraceState,
		CreatedAt:          createdAt,
	})
	if err != nil {
		return Acceptance{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID:    command.ActorID,
		Action:     "release.rollback",
		TargetType: "release",
		TargetID:   release.ID,
		Summary: map[string]string{
			"deploymentTargetId":  release.DeploymentTargetID.String(),
			"idempotencyKey":      command.IdempotencyKey,
			"operationId":         createdOperation.ID.String(),
			"rollbackOfReleaseId": source.ID.String(),
		},
		CreatedAt: createdAt,
	}); err != nil {
		return Acceptance{}, err
	}
	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit release rollback: %w", err)
	}
	return Acceptance{Release: release, Operation: createdOperation}, nil
}
