package workbench

import (
	"context"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Summary 是项目当前应用页的轻量只读投影，不包含 Kubernetes 观测或历史详情。
type Summary struct {
	ApplicationID uuid.UUID
	Targets       []Target
	Build         *Build
	Pipelines     []Pipeline
}

type Target struct {
	ID            uuid.UUID
	Stage         string
	ReleaseStatus *string
}

type Build struct {
	Status    string
	CreatedAt time.Time
}

type Pipeline struct {
	ID           uuid.UUID
	Name         string
	RunStatus    *string
	RunCreatedAt *time.Time
}

type Module struct {
	authorizer *projectauth.Module
}

func New(authorizer *projectauth.Module) *Module {
	return &Module{authorizer: authorizer}
}

// Summarize 在一次授权读取中批量查询当前页；每类记录只读一次，不按应用发起 N+1 查询。
// 应用列表游标由 Catalog 管理，这里只接受已经选出的应用 ID，并再次限定项目归属。
func (m *Module) Summarize(ctx context.Context, projectID uuid.UUID, caller identity.Caller,
	applicationIDs []uuid.UUID) (map[uuid.UUID]Summary, error) {
	summaries := make(map[uuid.UUID]Summary, len(applicationIDs))
	for _, id := range applicationIDs {
		summaries[id] = Summary{ApplicationID: id, Targets: []Target{}, Pipelines: []Pipeline{}}
	}
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		if len(applicationIDs) == 0 {
			return nil
		}
		var targets []targetRow
		if err := selectPage(ctx, tx, &targets, `SELECT d.application_id, d.id, d.stage,
			latest.status AS release_status
			FROM applications a
			JOIN deployment_targets d ON d.application_id=a.id
			LEFT JOIN LATERAL (
				SELECT o.status FROM releases r
				JOIN release_operations o ON o.release_id=r.id
				WHERE r.deployment_target_id=d.id
				ORDER BY r.created_at DESC,r.id DESC LIMIT 1
			) latest ON TRUE
			WHERE a.id IN (?) AND a.project_id=?
			ORDER BY d.application_id,d.created_at DESC,d.id DESC`, applicationIDs, projectID); err != nil {
			return fmt.Errorf("query workbench targets: %w", err)
		}
		for _, row := range targets {
			summary := summaries[row.ApplicationID]
			summary.Targets = append(summary.Targets, Target{ID: row.ID, Stage: row.Stage, ReleaseStatus: row.ReleaseStatus})
			summaries[row.ApplicationID] = summary
		}
		var builds []buildRow
		if err := selectPage(ctx, tx, &builds, `SELECT a.id AS application_id, latest.created_at,latest.status
			FROM applications a
			JOIN LATERAL (
				SELECT b.created_at,bo.status FROM builds b
				JOIN build_operations bo ON bo.build_id=b.id
				WHERE b.application_id=a.id
				ORDER BY b.created_at DESC,b.id DESC LIMIT 1
			) latest ON TRUE
			WHERE a.id IN (?) AND a.project_id=?`, applicationIDs, projectID); err != nil {
			return fmt.Errorf("query workbench builds: %w", err)
		}
		for _, row := range builds {
			summary := summaries[row.ApplicationID]
			summary.Build = &Build{Status: row.Status, CreatedAt: row.CreatedAt}
			summaries[row.ApplicationID] = summary
		}
		var pipelineRows []pipelineRow
		if err := selectPage(ctx, tx, &pipelineRows, `SELECT a.id AS application_id,p.id AS pipeline_id,p.name,
			latest.created_at AS run_created_at,latest.phase AS run_phase,
			latest.source_check_attempt_count,revision.mode AS run_mode,
			bo.status AS build_status,ro.status AS release_status
			FROM applications a
			JOIN LATERAL (
				SELECT id,name,created_at FROM delivery_pipelines
				WHERE application_id=a.id
				ORDER BY created_at DESC,id DESC LIMIT 3
			) p ON TRUE
			LEFT JOIN LATERAL (
				SELECT r.created_at,r.phase,r.source_check_attempt_count,
					r.build_id,r.release_id,r.pipeline_revision
				FROM delivery_runs r WHERE r.delivery_pipeline_id=p.id
				ORDER BY r.created_at DESC,r.id DESC LIMIT 1
			) latest ON TRUE
			LEFT JOIN delivery_pipeline_revisions revision
				ON revision.delivery_pipeline_id=p.id AND revision.revision=latest.pipeline_revision
			LEFT JOIN build_operations bo ON bo.build_id=latest.build_id
			LEFT JOIN release_operations ro ON ro.release_id=latest.release_id
			WHERE a.id IN (?) AND a.project_id=?
			ORDER BY a.id,p.created_at DESC,p.id DESC`, applicationIDs, projectID); err != nil {
			return fmt.Errorf("query workbench pipelines: %w", err)
		}
		for _, row := range pipelineRows {
			item := Pipeline{ID: row.PipelineID, Name: row.Name, RunCreatedAt: row.RunCreatedAt}
			if row.RunPhase != nil {
				if row.RunMode == nil || row.BuildStatus == nil || row.SourceCheckAttemptCount == nil {
					return fmt.Errorf("incomplete run state for pipeline %s", row.PipelineID)
				}
				status, _ := pipeline.DeriveRunState(*row.RunPhase, *row.RunMode,
					*row.SourceCheckAttemptCount, *row.BuildStatus, row.ReleaseStatus)
				item.RunStatus = &status
			}
			summary := summaries[row.ApplicationID]
			summary.Pipelines = append(summary.Pipelines, item)
			summaries[row.ApplicationID] = summary
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return summaries, nil
}

func selectPage(ctx context.Context, tx *sqlx.Tx, destination any, statement string,
	applicationIDs []uuid.UUID, projectID uuid.UUID) error {
	query, args, err := sqlx.In(statement, applicationIDs, projectID)
	if err != nil {
		return err
	}
	return tx.SelectContext(ctx, destination, tx.Rebind(query), args...)
}

type targetRow struct {
	ApplicationID uuid.UUID `db:"application_id"`
	ID            uuid.UUID `db:"id"`
	Stage         string    `db:"stage"`
	ReleaseStatus *string   `db:"release_status"`
}

type buildRow struct {
	ApplicationID uuid.UUID `db:"application_id"`
	Status        string    `db:"status"`
	CreatedAt     time.Time `db:"created_at"`
}

type pipelineRow struct {
	ApplicationID           uuid.UUID  `db:"application_id"`
	PipelineID              uuid.UUID  `db:"pipeline_id"`
	Name                    string     `db:"name"`
	RunCreatedAt            *time.Time `db:"run_created_at"`
	RunPhase                *string    `db:"run_phase"`
	SourceCheckAttemptCount *int       `db:"source_check_attempt_count"`
	RunMode                 *string    `db:"run_mode"`
	BuildStatus             *string    `db:"build_status"`
	ReleaseStatus           *string    `db:"release_status"`
}
