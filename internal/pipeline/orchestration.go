package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/google/uuid"
)

const systemActorID = "orbit-devops-pipeline"

// HandleEvent 只把内部事件作为唤醒信号；每次处理都重新读取 PostgreSQL 权威事实。
func (m *Module) HandleEvent(ctx context.Context, event internalevent.Ref) error {
	switch event.Topic {
	case "webhook_delivery.received.v1":
		return m.processWebhookDelivery(ctx, event.AggregateID)
	case "build_operation.changed.v1":
		return m.processBuildOperation(ctx, event.AggregateID)
	case "release_operation.changed.v1":
		return m.processReleaseOperation(ctx, event.AggregateID)
	case "delivery_run.reconcile.v1":
		return m.advanceRun(ctx, event.AggregateID, true)
	default:
		return nil
	}
}

type acceptedWebhook struct {
	ID                uuid.UUID `db:"id"`
	EndpointKey       string    `db:"endpoint_key"`
	State             string    `db:"state"`
	RepositoryID      *int64    `db:"repository_id"`
	RepositoryOwnerID *int64    `db:"repository_owner_id"`
	GitRef            *string   `db:"git_ref"`
	AfterCommit       *string   `db:"after_commit"`
	TraceParent       string    `db:"traceparent"`
	TraceState        string    `db:"tracestate"`
}

func (m *Module) processWebhookDelivery(ctx context.Context, deliveryID uuid.UUID) error {
	var webhook acceptedWebhook
	if err := m.db.GetContext(ctx, &webhook, `SELECT id,endpoint_key,state,repository_id,
		repository_owner_id,git_ref,after_commit,traceparent,tracestate
		FROM webhook_deliveries WHERE id=$1`, deliveryID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("load webhook delivery for processing: %w", err)
	}
	if webhook.State != "pending" || webhook.RepositoryID == nil || webhook.RepositoryOwnerID == nil || webhook.GitRef == nil || webhook.AfterCommit == nil {
		return nil
	}
	var pipelineIDs []uuid.UUID
	if err := m.db.SelectContext(ctx, &pipelineIDs, `SELECT p.id FROM delivery_pipelines p
		JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=p.current_revision
		WHERE p.enabled AND r.provider='github' AND r.endpoint_key=$1 AND r.repository_id=$2
		AND r.repository_owner_id=$3 AND r.git_ref=$4 ORDER BY p.id`, webhook.EndpointKey,
		*webhook.RepositoryID, *webhook.RepositoryOwnerID, *webhook.GitRef); err != nil {
		return fmt.Errorf("match webhook delivery pipelines: %w", err)
	}
	handled := 0
	for _, pipelineID := range pipelineIDs {
		matched, err := m.createRunAndBuild(ctx, pipelineID, webhook)
		if err != nil {
			return err
		}
		if matched {
			handled++
		}
	}
	now := time.Now().UTC()
	state := "processed"
	var reason any
	if handled == 0 {
		state, reason = "ignored", "no_matching_pipeline"
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE webhook_deliveries SET state=$2,reason_code=$3,
		processed_at=$4 WHERE id=$1 AND state='pending'`, deliveryID, state, reason, now); err != nil {
		return fmt.Errorf("finish webhook delivery processing: %w", err)
	}
	return nil
}

type pipelineEvidence struct {
	ProjectID            uuid.UUID `db:"project_id"`
	ApplicationID        uuid.UUID `db:"application_id"`
	CurrentRevision      int       `db:"current_revision"`
	ActivationGeneration int64     `db:"activation_generation"`
	EndpointKey          string    `db:"endpoint_key"`
	RepositoryID         int64     `db:"repository_id"`
	RepositoryOwnerID    int64     `db:"repository_owner_id"`
	RepositoryURL        string    `db:"repository_url"`
	GitRef               string    `db:"git_ref"`
}

// createRunAndBuild 在锁定当前 Pipeline 后，把 Run、Build、Operation、Dispatch 与审计一次提交。
func (m *Module) createRunAndBuild(ctx context.Context, pipelineID uuid.UUID, webhook acceptedWebhook) (bool, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin automatic build: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var evidence pipelineEvidence
	if err := tx.GetContext(ctx, &evidence, `SELECT p.project_id,p.application_id,p.current_revision,
		p.activation_generation,r.endpoint_key,r.repository_id,r.repository_owner_id,r.repository_url,r.git_ref
		FROM delivery_pipelines p JOIN delivery_pipeline_revisions r
		ON r.delivery_pipeline_id=p.id AND r.revision=p.current_revision
		WHERE p.id=$1 AND p.enabled FOR UPDATE OF p`, pipelineID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("lock matched delivery pipeline: %w", err)
	}
	if webhook.RepositoryID == nil || webhook.RepositoryOwnerID == nil || webhook.GitRef == nil || webhook.AfterCommit == nil ||
		evidence.EndpointKey != webhook.EndpointKey || evidence.RepositoryID != *webhook.RepositoryID ||
		evidence.RepositoryOwnerID != *webhook.RepositoryOwnerID || evidence.GitRef != *webhook.GitRef {
		return false, nil
	}
	var existing bool
	if err := tx.GetContext(ctx, &existing, `SELECT EXISTS (SELECT 1 FROM delivery_runs
		WHERE delivery_pipeline_id=$1 AND pipeline_revision=$2 AND source_commit=$3)`, pipelineID,
		evidence.CurrentRevision, *webhook.AfterCommit); err != nil {
		return false, err
	}
	if existing {
		return true, tx.Commit()
	}
	if m.builds == nil {
		return false, errors.New("automatic build accepter is unavailable")
	}
	now := time.Now().UTC()
	runID, buildID, operationID := uuid.New(), uuid.New(), uuid.New()
	if _, err := m.builds.CreateForDelivery(ctx, tx, build.DeliveryCreateCommand{
		BuildID: buildID, BuildOperationID: operationID, DeliveryRunID: runID,
		PipelineID: pipelineID, PipelineRevision: evidence.CurrentRevision,
		WebhookDeliveryID: webhook.ID, SourceCommit: *webhook.AfterCommit,
		ActorID: systemActorID, TraceParent: webhook.TraceParent, TraceState: webhook.TraceState, CreatedAt: now,
	}); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_runs
		(id,delivery_pipeline_id,pipeline_revision,activation_generation,webhook_delivery_id,
		 source_commit,repository_url,phase,phase_version,build_id,trigger_event_type,
		 trigger_repository_full_name,trigger_git_ref,trigger_forced,trigger_received_at,
		 traceparent,tracestate,created_at,updated_at)
		SELECT $1,$2,$3,$4,w.id,$6,$7,'build_created',1,$8,w.event_type,
		 COALESCE(w.repository_full_name,''),COALESCE(w.git_ref,''),w.forced,w.received_at,
		 w.traceparent,w.tracestate,$9,$9 FROM webhook_deliveries w WHERE w.id=$5`, runID, pipelineID,
		evidence.CurrentRevision, evidence.ActivationGeneration, webhook.ID, *webhook.AfterCommit,
		evidence.RepositoryURL, buildID, now); err != nil {
		return false, fmt.Errorf("insert delivery run: %w", err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: systemActorID, ActorKind: audit.ActorKindSystem,
		Action: "delivery_run.create", TargetType: "delivery_run", TargetID: runID,
		Summary: map[string]string{"deliveryPipelineId": pipelineID.String(), "webhookDeliveryId": webhook.ID.String(), "buildId": buildID.String()}, CreatedAt: now}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit automatic build: %w", err)
	}
	m.recorder.RecordTransition("webhook", "build_created", "", 0)
	return true, nil
}

func (m *Module) processBuildOperation(ctx context.Context, operationID uuid.UUID) error {
	var runID uuid.UUID
	err := m.db.GetContext(ctx, &runID, `SELECT dr.id FROM delivery_runs dr
		JOIN build_operations o ON o.build_id=dr.build_id WHERE o.id=$1`, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find delivery run for build operation: %w", err)
	}
	if err := m.advanceBuild(ctx, runID); err != nil {
		return err
	}
	return m.advanceRun(ctx, runID, false)
}

func (m *Module) advanceBuild(ctx context.Context, runID uuid.UUID) error {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current struct {
		Phase      string     `db:"phase"`
		Status     string     `db:"status"`
		ArtifactID *uuid.UUID `db:"artifact_id"`
		UpdatedAt  time.Time  `db:"updated_at"`
	}
	if err := tx.GetContext(ctx, &current, `SELECT dr.phase,o.status,a.id AS artifact_id,dr.updated_at
		FROM delivery_runs dr JOIN build_operations o ON o.build_id=dr.build_id
		LEFT JOIN image_artifacts a ON a.build_id=dr.build_id WHERE dr.id=$1 FOR UPDATE OF dr`, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	advanced := current.Phase == "build_created" && current.Status == "succeeded" && current.ArtifactID != nil
	if advanced {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET phase='artifact_ready',
			phase_version=phase_version+1,image_artifact_id=$2,updated_at=$3 WHERE id=$1`, runID, *current.ArtifactID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if advanced {
		m.recorder.RecordTransition("build", "artifact_ready", "", time.Since(current.UpdatedAt))
	}
	return nil
}

func (m *Module) processReleaseOperation(ctx context.Context, operationID uuid.UUID) error {
	var runID uuid.UUID
	err := m.db.GetContext(ctx, &runID, `SELECT dr.id FROM delivery_runs dr
		JOIN release_operations o ON o.release_id=dr.release_id WHERE o.id=$1`, operationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.advanceRelease(ctx, runID)
}

func (m *Module) advanceRelease(ctx context.Context, runID uuid.UUID) error {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var current struct {
		Phase     string    `db:"phase"`
		Status    string    `db:"status"`
		UpdatedAt time.Time `db:"updated_at"`
	}
	if err := tx.GetContext(ctx, &current, `SELECT dr.phase,o.status,dr.updated_at FROM delivery_runs dr
		JOIN release_operations o ON o.release_id=dr.release_id WHERE dr.id=$1 FOR UPDATE OF dr`, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	advanced := current.Phase == "release_created" && current.Status == "succeeded"
	if advanced {
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET phase='completed',
			phase_version=phase_version+1,updated_at=$2,finished_at=$2 WHERE id=$1`, runID, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if advanced {
		m.recorder.RecordTransition("release", "completed", "", time.Since(current.UpdatedAt))
	}
	return nil
}

func (m *Module) advanceRun(ctx context.Context, runID uuid.UUID, force bool) error {
	var phase string
	if err := m.db.GetContext(ctx, &phase, `SELECT phase FROM delivery_runs WHERE id=$1`, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	switch phase {
	case "build_created":
		if err := m.advanceBuild(ctx, runID); err != nil {
			return err
		}
		if err := m.db.GetContext(ctx, &phase, `SELECT phase FROM delivery_runs WHERE id=$1`, runID); err != nil {
			return err
		}
		if phase == "artifact_ready" {
			return m.verifySourceAndRelease(ctx, runID, force)
		}
		return nil
	case "artifact_ready":
		return m.verifySourceAndRelease(ctx, runID, force)
	case "release_created":
		return m.advanceRelease(ctx, runID)
	default:
		return nil
	}
}

type sourceCheck struct {
	RunID                uuid.UUID  `db:"id"`
	PhaseVersion         int64      `db:"phase_version"`
	PipelineID           uuid.UUID  `db:"delivery_pipeline_id"`
	PipelineRevision     int        `db:"pipeline_revision"`
	ActivationGeneration int64      `db:"activation_generation"`
	RepositoryID         int64      `db:"repository_id"`
	OwnerID              int64      `db:"repository_owner_id"`
	RepositoryURL        string     `db:"repository_url"`
	GitRef               string     `db:"git_ref"`
	SourceCommit         string     `db:"source_commit"`
	Mode                 string     `db:"mode"`
	Enabled              bool       `db:"enabled"`
	CurrentRevision      int        `db:"current_revision"`
	CurrentGeneration    int64      `db:"current_generation"`
	AttemptCount         int        `db:"source_check_attempt_count"`
	StartedAt            *time.Time `db:"source_check_started_at"`
	NextAt               *time.Time `db:"source_check_next_at"`
	TraceParent          string     `db:"traceparent"`
	TraceState           string     `db:"tracestate"`
	UpdatedAt            time.Time  `db:"updated_at"`
}

func (m *Module) verifySourceAndRelease(ctx context.Context, runID uuid.UUID, force bool) error {
	check, proceed, err := m.prepareSourceCheck(ctx, runID, force)
	if err != nil || !proceed {
		return err
	}
	identity, inspectErr := m.inspector.Head(ctx, HeadRequest{RepositoryID: check.RepositoryID,
		OwnerID: check.OwnerID, RepositoryURL: check.RepositoryURL, GitRef: check.GitRef})
	if inspectErr != nil {
		outcome := "unavailable"
		if errors.Is(inspectErr, ErrSourceOwnerChanged) {
			outcome = "owner_changed"
		} else if errors.Is(inspectErr, ErrSourceNotFound) {
			outcome = "not_found"
		}
		m.recorder.RecordGitHubRead(outcome)
		return m.deferOrBlockSourceCheck(ctx, check, inspectErr)
	}
	if identity.RepositoryID != check.RepositoryID || identity.OwnerID != check.OwnerID {
		m.recorder.RecordGitHubRead("identity_changed")
		return m.finishRun(ctx, check.RunID, check.PhaseVersion, "blocked", "repository_identity_changed")
	}
	if identity.RepositoryURL == "" {
		m.recorder.RecordGitHubRead("unavailable")
		return m.deferOrBlockSourceCheck(ctx, check, ErrSourceUnavailable)
	}
	if identity.HeadCommit != check.SourceCommit {
		m.recorder.RecordGitHubRead("superseded")
		return m.finishRun(ctx, check.RunID, check.PhaseVersion, "superseded", "source_commit_superseded")
	}
	m.recorder.RecordGitHubRead("current")
	return m.createAutomaticRelease(ctx, check, identity.RepositoryURL)
}

// prepareSourceCheck 在网络调用前提交短事务，记录尝试并冻结本次 phase_version。
func (m *Module) prepareSourceCheck(ctx context.Context, runID uuid.UUID, force bool) (sourceCheck, bool, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return sourceCheck{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var check sourceCheck
	if err := tx.GetContext(ctx, &check, `SELECT dr.id,dr.phase_version,dr.delivery_pipeline_id,
		dr.pipeline_revision,dr.activation_generation,r.repository_id,r.repository_owner_id,
		dr.repository_url,r.git_ref,dr.source_commit,r.mode,p.enabled,p.current_revision,
		p.activation_generation AS current_generation,dr.source_check_attempt_count,
		dr.source_check_started_at,dr.source_check_next_at,dr.traceparent,dr.tracestate,dr.updated_at
		FROM delivery_runs dr JOIN delivery_pipelines p ON p.id=dr.delivery_pipeline_id
		JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=dr.delivery_pipeline_id AND r.revision=dr.pipeline_revision
		WHERE dr.id=$1 AND dr.phase='artifact_ready' FOR UPDATE OF dr`, runID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sourceCheck{}, false, nil
		}
		return sourceCheck{}, false, err
	}
	if check.Mode == ModeBuildOnly {
		return check, false, tx.Commit()
	}
	now := time.Now().UTC()
	if !check.Enabled || check.CurrentRevision != check.PipelineRevision || check.CurrentGeneration != check.ActivationGeneration {
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET phase='superseded',
			phase_version=phase_version+1,reason_code='pipeline_configuration_changed',updated_at=$2,finished_at=$2
			WHERE id=$1`, runID, now); err != nil {
			return check, false, err
		}
		if err := tx.Commit(); err != nil {
			return check, false, err
		}
		m.recorder.RecordTransition("source_verification", "superseded", "pipeline_configuration_changed", time.Since(check.UpdatedAt))
		return check, false, nil
	}
	if !force && check.NextAt != nil && check.NextAt.After(now) {
		return check, false, tx.Commit()
	}
	if check.StartedAt == nil {
		check.StartedAt = &now
	}
	check.AttemptCount++
	check.PhaseVersion++
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET source_check_attempt_count=$2,
		source_check_started_at=$3,source_check_next_at=NULL,phase_version=phase_version+1,updated_at=$4
		WHERE id=$1`, runID, check.AttemptCount, check.StartedAt, now); err != nil {
		return check, false, err
	}
	return check, true, tx.Commit()
}

func (m *Module) deferOrBlockSourceCheck(ctx context.Context, check sourceCheck, inspectErr error) error {
	if errors.Is(inspectErr, ErrSourceOwnerChanged) {
		return m.finishRun(ctx, check.RunID, check.PhaseVersion, "blocked", "repository_owner_changed")
	}
	if errors.Is(inspectErr, ErrSourceNotFound) {
		return m.finishRun(ctx, check.RunID, check.PhaseVersion, "blocked", "repository_or_branch_not_found")
	}
	now := time.Now().UTC()
	if check.StartedAt != nil && now.Sub(*check.StartedAt) >= m.config.SourceRecoveryWindow {
		return m.finishRun(ctx, check.RunID, check.PhaseVersion, "blocked", "source_verification_unavailable")
	}
	delay := m.config.SourceRetryBaseDelay
	for attempt := 1; attempt < check.AttemptCount && delay < 5*time.Minute; attempt++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	next := now.Add(delay)
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET source_check_next_at=$3,
		reason_code='source_verification_unavailable',updated_at=$4
		WHERE id=$1 AND phase='artifact_ready' AND phase_version=$2`, check.RunID, check.PhaseVersion, next, now)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 1 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO internal_event_outbox
			(id,topic,aggregate_id,protocol_version,state,available_at,next_dispatch_at,
			 traceparent,tracestate,created_at,updated_at)
			VALUES($1,'delivery_run.reconcile.v1',$2,1,'pending',$3,$3,$4,$5,$6,$6)`, uuid.New(),
			check.RunID, next, check.TraceParent, check.TraceState, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Module) finishRun(ctx context.Context, runID uuid.UUID, version int64, phase, reason string) error {
	now := time.Now().UTC()
	result, err := m.db.ExecContext(ctx, `UPDATE delivery_runs SET phase=$3,phase_version=phase_version+1,
		reason_code=$4,updated_at=$5,finished_at=$5
		WHERE id=$1 AND phase='artifact_ready' AND phase_version=$2`, runID, version, phase, reason, now)
	if err != nil {
		return err
	}
	if count, countErr := result.RowsAffected(); countErr == nil && count == 1 {
		m.recorder.RecordTransition("source_verification", phase, reason, 0)
	}
	return nil
}

// createAutomaticRelease 在网络调用返回后重新核验全部门禁，再原子创建 Release 并推进 Run。
func (m *Module) createAutomaticRelease(ctx context.Context, check sourceCheck, repositoryURL string) error {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// 与人工启停使用同一 Pipeline 行作为最终围栏。谁先取得该锁，谁决定本次自动发布是否已被接纳。
	if _, err := tx.ExecContext(ctx, `SELECT id FROM delivery_pipelines WHERE id=$1 FOR UPDATE`, check.PipelineID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var valid bool
	if err := tx.GetContext(ctx, &valid, `SELECT (dr.phase='artifact_ready' AND dr.phase_version=$2
		AND dr.release_id IS NULL AND p.enabled AND p.current_revision=dr.pipeline_revision
		AND p.activation_generation=dr.activation_generation) AS valid
		FROM delivery_runs dr JOIN delivery_pipelines p ON p.id=dr.delivery_pipeline_id
		WHERE dr.id=$1 AND p.id=$3 FOR UPDATE OF dr`, check.RunID, check.PhaseVersion, check.PipelineID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !valid {
		return tx.Commit()
	}
	if m.releases == nil {
		return errors.New("automatic release accepter is unavailable")
	}
	now := time.Now().UTC()
	releaseID, operationID := uuid.New(), uuid.New()
	if _, err := m.releases.CreateForDeliveryRun(ctx, tx, delivery.DeliveryRunCreateCommand{
		ReleaseID: releaseID, ReleaseOperationID: operationID, DeliveryRunID: check.RunID,
		ActorID: systemActorID, TraceParent: check.TraceParent, TraceState: check.TraceState, CreatedAt: now,
	}); err != nil {
		if errors.Is(err, delivery.ErrAutomaticReleaseTargetStage) {
			const reason = "auto_release_target_stage_forbidden"
			// 接纳尚未写入 Release/Operation，同一事务可直接把 Run 终结为可诊断的 blocked。
			if _, updateErr := tx.ExecContext(ctx, `UPDATE delivery_runs SET phase='blocked',
				phase_version=phase_version+1,reason_code=$3,updated_at=$4,finished_at=$4
				WHERE id=$1 AND phase_version=$2`, check.RunID, check.PhaseVersion, reason, now); updateErr != nil {
				return updateErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return commitErr
			}
			m.recorder.RecordTransition("source_verification", "blocked", reason, 0)
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE delivery_runs SET phase='release_created',
		phase_version=phase_version+1,release_id=$3,repository_url=$4,reason_code=NULL,updated_at=$5
		WHERE id=$1 AND phase_version=$2`, check.RunID, check.PhaseVersion, releaseID, repositoryURL, now); err != nil {
		return err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: systemActorID, ActorKind: audit.ActorKindSystem,
		Action: "delivery_run.release_created", TargetType: "delivery_run", TargetID: check.RunID,
		Summary: map[string]string{"releaseId": releaseID.String(), "deliveryPipelineId": check.PipelineID.String()}, CreatedAt: now}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	duration := time.Duration(0)
	if check.StartedAt != nil {
		duration = time.Since(*check.StartedAt)
	}
	m.recorder.RecordTransition("source_verification", "release_created", "", duration)
	return nil
}
