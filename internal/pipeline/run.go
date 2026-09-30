package pipeline

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrRunNotFound   = errors.New("delivery run not found")
	ErrInvalidCursor = errors.New("invalid delivery run cursor")
)

type RunRecord struct {
	ID                      uuid.UUID  `db:"id" json:"id"`
	PipelineID              uuid.UUID  `db:"delivery_pipeline_id" json:"deliveryPipelineId"`
	PipelineRevision        int        `db:"pipeline_revision" json:"pipelineRevision"`
	ActivationGeneration    int64      `db:"activation_generation" json:"activationGeneration"`
	WebhookDeliveryID       *uuid.UUID `db:"webhook_delivery_id" json:"webhookDeliveryId,omitempty"`
	SourceCommit            string     `db:"source_commit" json:"sourceCommit"`
	RepositoryURL           string     `db:"repository_url" json:"repositoryUrl"`
	Phase                   string     `db:"phase" json:"phase"`
	PhaseVersion            int64      `db:"phase_version" json:"phaseVersion"`
	BuildID                 uuid.UUID  `db:"build_id" json:"buildId"`
	ImageArtifactID         *uuid.UUID `db:"image_artifact_id" json:"imageArtifactId,omitempty"`
	ReleaseID               *uuid.UUID `db:"release_id" json:"releaseId,omitempty"`
	ReasonCode              *string    `db:"reason_code" json:"reasonCode,omitempty"`
	SourceCheckAttemptCount int        `db:"source_check_attempt_count" json:"-"`
	SourceCheckNextAt       *time.Time `db:"source_check_next_at" json:"-"`
	CreatedAt               time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt               time.Time  `db:"updated_at" json:"updatedAt"`
	FinishedAt              *time.Time `db:"finished_at" json:"finishedAt,omitempty"`
}

type RunDetail struct {
	Run         RunRecord  `json:"run"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	ActiveStage *string    `json:"activeStage,omitempty"`
	Trigger     RunTrigger `json:"trigger"`
}

// RunTrigger 是可以安全返回给项目成员的最小可信触发摘要。
type RunTrigger struct {
	EventType          string    `db:"event_type" json:"eventType"`
	RepositoryFullName string    `db:"repository_full_name" json:"repositoryFullName"`
	GitRef             string    `db:"git_ref" json:"gitRef"`
	Forced             bool      `db:"forced" json:"forced"`
	ReceivedAt         time.Time `db:"received_at" json:"receivedAt"`
}

type RunPage struct {
	Items      []RunDetail
	NextCursor *string
}

type ListRunsQuery struct {
	PipelineID uuid.UUID
	Caller     identity.Caller
	Limit      int
	Cursor     string
}

type ReconcileRunCommand struct {
	RunID          uuid.UUID
	Caller         identity.Caller
	IdempotencyKey string
	TraceParent    string
	TraceState     string
}

type runRow struct {
	RunRecord
	RunTrigger
	ProjectID              uuid.UUID `db:"project_id"`
	PipelineMode           string    `db:"pipeline_mode"`
	BuildOperationStatus   string    `db:"build_operation_status"`
	ReleaseOperationStatus *string   `db:"release_operation_status"`
}

const runSelect = `SELECT r.id, r.delivery_pipeline_id, r.pipeline_revision, r.activation_generation,
	r.webhook_delivery_id, r.source_commit, r.repository_url, r.phase, r.phase_version,
	r.build_id, r.image_artifact_id, r.release_id, r.reason_code,r.source_check_attempt_count,
	r.source_check_next_at,r.created_at, r.updated_at,
	r.finished_at, p.project_id, pr.mode AS pipeline_mode, bo.status AS build_operation_status,
	ro.status AS release_operation_status, r.trigger_event_type AS event_type,
	r.trigger_repository_full_name AS repository_full_name,r.trigger_git_ref AS git_ref,
	r.trigger_forced AS forced,r.trigger_received_at AS received_at
	FROM delivery_runs r
	JOIN delivery_pipelines p ON p.id=r.delivery_pipeline_id
	JOIN delivery_pipeline_revisions pr ON pr.delivery_pipeline_id=r.delivery_pipeline_id AND pr.revision=r.pipeline_revision
	JOIN build_operations bo ON bo.build_id=r.build_id
	LEFT JOIN release_operations ro ON ro.release_id=r.release_id`

// GetRun 投影编排 Phase 与底层 Operation 权威状态，不复制 Attempt 详情。
func (m *Module) GetRun(ctx context.Context, id uuid.UUID, caller identity.Caller) (RunDetail, error) {
	var row runRow
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		var err error
		row, err = m.getRunWith(ctx, tx, id)
		if err != nil {
			return err
		}
		return m.authorizer.RequireAuthorizedInTransaction(ctx, tx, row.ProjectID, caller, projectauth.PermissionRead)
	})
	if err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return RunDetail{}, ErrRunNotFound
		}
		return RunDetail{}, err
	}
	return projectRun(row), nil
}

// ListRuns 使用创建时间和 UUID 组成稳定游标，避免新 Run 导致翻页重复。
func (m *Module) ListRuns(ctx context.Context, query ListRunsQuery) (RunPage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	args := []any{query.PipelineID, limit + 1}
	where := ` WHERE r.delivery_pipeline_id=$1`
	if query.Cursor != "" {
		cursor, err := decodeRunCursor(query.Cursor, query.PipelineID)
		if err != nil {
			return RunPage{}, err
		}
		where += ` AND (r.created_at, r.id) < ($3, $4)`
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	rows := make([]runRow, 0)
	err := m.authorizer.Read(ctx, query.Caller, func(tx *sqlx.Tx) error {
		pipeline, err := m.getWith(ctx, tx, query.PipelineID)
		if err != nil {
			return err
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, pipeline.Pipeline.ProjectID, query.Caller, projectauth.PermissionRead); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &rows, runSelect+where+` ORDER BY r.created_at DESC, r.id DESC LIMIT $2`, args...)
	})
	if err != nil {
		return RunPage{}, err
	}
	page := RunPage{Items: make([]RunDetail, 0, min(len(rows), limit))}
	for _, row := range rows[:min(len(rows), limit)] {
		page.Items = append(page.Items, projectRun(row))
	}
	if len(rows) > limit {
		value := encodeRunCursor(runCursor{PipelineID: query.PipelineID,
			CreatedAt: rows[limit-1].CreatedAt, ID: rows[limit-1].ID})
		page.NextCursor = &value
	}
	return page, nil
}

// ReconcileRun 只持久化一次推进请求；它不改变底层 Operation，也不绕过发布门禁。
func (m *Module) ReconcileRun(ctx context.Context, command ReconcileRunCommand) (RunDetail, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return RunDetail{}, fmt.Errorf("begin reconcile delivery run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return RunDetail{}, err
	}
	row, err := m.getRunWith(ctx, tx, command.RunID)
	if err != nil {
		return RunDetail{}, err
	}
	if err := m.authorizer.RequireInTransaction(ctx, tx, row.ProjectID, command.Caller, projectauth.PermissionDevelop); err != nil {
		return RunDetail{}, err
	}
	fingerprint, err := idempotency.Fingerprint(struct{ RunID uuid.UUID }{command.RunID})
	if err != nil {
		return RunDetail{}, fmt.Errorf("fingerprint reconcile delivery run: %w", err)
	}
	now := time.Now().UTC()
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "delivery_run.reconcile", Key: command.IdempotencyKey}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, fingerprint, command.RunID, now)
	if err != nil {
		return RunDetail{}, err
	}
	if isNew {
		if _, err := tx.ExecContext(ctx, `INSERT INTO internal_event_outbox
			(id, topic, aggregate_id, protocol_version, state, available_at, next_dispatch_at,
			 traceparent, tracestate, created_at, updated_at)
			VALUES ($1,'delivery_run.reconcile.v1',$2,1,'pending',$3,$3,$4,$5,$3,$3)`, uuid.New(), command.RunID, now, command.TraceParent, command.TraceState); err != nil {
			return RunDetail{}, fmt.Errorf("enqueue delivery run reconcile: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return RunDetail{}, fmt.Errorf("commit reconcile delivery run: %w", err)
	}
	return projectRun(row), nil
}

func (m *Module) getRunWith(ctx context.Context, queryer sqlx.QueryerContext, id uuid.UUID) (runRow, error) {
	var row runRow
	if err := sqlx.GetContext(ctx, queryer, &row, runSelect+` WHERE r.id=$1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runRow{}, ErrRunNotFound
		}
		return runRow{}, fmt.Errorf("get delivery run: %w", err)
	}
	return row, nil
}

func projectRun(row runRow) RunDetail {
	status, stage := DeriveRunState(row.Phase, row.PipelineMode, row.SourceCheckAttemptCount,
		row.BuildOperationStatus, row.ReleaseOperationStatus)
	var activeStage *string
	if stage != "" {
		activeStage = &stage
	}
	return RunDetail{Run: row.RunRecord, Mode: row.PipelineMode, Status: status, ActiveStage: activeStage, Trigger: row.RunTrigger}
}

// DeriveRunState 从持久化阶段与底层 Operation 的权威状态投影交付运行状态。
// 工作台批量摘要与运行详情共用同一规则，避免两处状态解释逐渐分叉。
func DeriveRunState(phase, mode string, sourceCheckAttempts int, buildStatus string,
	releaseStatus *string) (string, string) {
	status := "building"
	stage := "build"
	switch phase {
	case "build_created":
		switch buildStatus {
		case "failed":
			status = "build_failed"
			stage = ""
		case "canceled":
			status = "build_canceled"
			stage = ""
		case "attention_required":
			status = "attention_required"
			stage = "build"
		}
	case "artifact_ready":
		status = "candidate_ready"
		if mode != ModeBuildOnly && sourceCheckAttempts > 0 {
			status = "verifying_source"
		}
		if mode != ModeBuildOnly {
			stage = "source_verification"
		} else {
			stage = ""
		}
	case "release_created":
		status, stage = "releasing", "release"
		if releaseStatus != nil {
			switch *releaseStatus {
			case "failed":
				status, stage = "release_failed", ""
			case "canceled":
				status, stage = "release_canceled", ""
			case "attention_required":
				status = "attention_required"
			}
		}
	case "completed":
		status, stage = "succeeded", ""
	case "superseded":
		status, stage = "superseded", ""
	case "blocked":
		status, stage = "blocked", ""
	}
	return status, stage
}

type runCursor struct {
	PipelineID uuid.UUID `json:"pipelineId"`
	CreatedAt  time.Time `json:"createdAt"`
	ID         uuid.UUID `json:"id"`
}

func encodeRunCursor(cursor runCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}
func decodeRunCursor(value string, pipelineID uuid.UUID) (runCursor, error) {
	if len(value) > 512 {
		return runCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return runCursor{}, ErrInvalidCursor
	}
	var cursor runCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || cursor.PipelineID != pipelineID ||
		cursor.ID == uuid.Nil || cursor.CreatedAt.IsZero() {
		return runCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}
