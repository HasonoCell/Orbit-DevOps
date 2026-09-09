package diagnostics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrReleaseNotFound          = errors.New("release not found")
	ErrInvalidRuntimeLogQuery   = errors.New("invalid runtime log query")
	ErrRuntimeLogSourceNotFound = errors.New("runtime log source not found")
	ErrKubernetesUnavailable    = errors.New("kubernetes unavailable")
)

const (
	DefaultRuntimeLogTailLines = 200
	MaximumRuntimeLogTailLines = 500
	MaximumRuntimeLogBytes     = 128 * 1024
)

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
	source     RuntimeSource
	logSource  RuntimeLogSource
	now        func() time.Time
}

// New 创建诊断深模块；调用方只需提供持久化、授权和运行时来源。
func New(db *sqlx.DB, authorizer *projectauth.Module, source RuntimeSource) *Module {
	if source == nil {
		source = UnavailableSource{}
	}
	logSource, ok := source.(RuntimeLogSource)
	if !ok {
		logSource = UnavailableSource{}
	}
	return &Module{
		db:         db,
		operations: operation.New(db),
		authorizer: authorizer,
		source:     source,
		logSource:  logSource,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// GetRuntimeLogs 授权后读取指定 Release 的临时日志，并统一执行脱敏与 UTF-8 字节上限。
func (m *Module) GetRuntimeLogs(
	ctx context.Context,
	query GetRuntimeLogsQuery,
) (LogExcerpt, error) {
	if strings.TrimSpace(query.PodName) == "" || strings.TrimSpace(query.Container) == "" ||
		query.TailLines < 0 || query.TailLines > MaximumRuntimeLogTailLines {
		return LogExcerpt{}, ErrInvalidRuntimeLogQuery
	}
	if query.TailLines == 0 {
		query.TailLines = DefaultRuntimeLogTailLines
	}

	release, err := m.loadReleaseForPermission(
		ctx,
		query.ReleaseID,
		query.ActorID,
		projectauth.PermissionReadRuntimeLogs,
	)
	if err != nil {
		return LogExcerpt{}, err
	}
	result, err := m.logSource.ReadRuntimeLogs(ctx, RuntimeLogQuery{
		ProjectID: release.TargetSnapshot.ProjectID, ApplicationID: release.TargetSnapshot.ApplicationID,
		TargetID: release.DeploymentTargetID, ReleaseID: release.ID,
		ClusterRef: release.TargetSnapshot.ClusterRef, Namespace: release.TargetSnapshot.Namespace,
		PodName: query.PodName, Container: query.Container,
		TailLines: query.TailLines, Previous: query.Previous,
	})
	if err != nil {
		return LogExcerpt{}, err
	}
	content, truncated := SanitizeRuntimeLog(result.Content, MaximumRuntimeLogBytes)
	return LogExcerpt{
		Source: SourceKubernetes, ObservedAt: result.ObservedAt,
		ProjectID: release.TargetSnapshot.ProjectID, ReleaseID: release.ID,
		PodName: query.PodName, Container: query.Container,
		TailLines: query.TailLines, Previous: query.Previous,
		Content: content, Truncated: result.Truncated || truncated,
	}, nil
}

func (m *Module) loadReleaseForPermission(
	ctx context.Context,
	releaseID uuid.UUID,
	actorID string,
	permission projectauth.Permission,
) (delivery.Release, error) {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return delivery.Release{}, fmt.Errorf("begin release permission query: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var release delivery.Release
	if err := tx.GetContext(ctx, &release, releaseSelect+` WHERE id = $1`, releaseID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return delivery.Release{}, ErrReleaseNotFound
		}
		return delivery.Release{}, fmt.Errorf("load release for permission: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(
		ctx, tx, release.TargetSnapshot.ProjectID, actorID, permission,
	); err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return delivery.Release{}, ErrReleaseNotFound
		}
		return delivery.Release{}, err
	}
	if err := tx.Commit(); err != nil {
		return delivery.Release{}, fmt.Errorf("commit release permission query: %w", err)
	}
	return release, nil
}

// GetReleaseReport 先取得一致的数据库上下文，再读取 Kubernetes，避免把两者伪装成全局原子快照。
func (m *Module) GetReleaseReport(
	ctx context.Context,
	query GetReleaseReportQuery,
) (Report, error) {
	release, current, operationRecord, err := m.loadControlPlane(ctx, query)
	if err != nil {
		return Report{}, err
	}

	observation := m.source.ObserveRelease(ctx, RuntimeQuery{
		ProjectID:     release.TargetSnapshot.ProjectID,
		ApplicationID: release.TargetSnapshot.ApplicationID,
		TargetID:      release.DeploymentTargetID,
		ReleaseID:     release.ID,
		ClusterRef:    release.TargetSnapshot.ClusterRef,
		Namespace:     release.TargetSnapshot.Namespace,
	})
	relation := relateRuntimeRelease(release.ID, observation.Workload)
	report := Report{
		Release:                release,
		Operation:              operationRecord,
		TargetDifferences:      compareTarget(release.TargetSnapshot, current),
		RuntimeReleaseRelation: relation,
		Workload:               observation.Workload,
		Events:                 observation.Events,
		GeneratedAt:            m.now(),
	}
	report.Signals = deriveSignals(report)
	return report, nil
}

// loadControlPlane 把 Release、Operation、Attempt 和当前目标读取固定在同一个可重复读快照中。
func (m *Module) loadControlPlane(
	ctx context.Context,
	query GetReleaseReportQuery,
) (delivery.Release, targetRecord, operation.Record, error) {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return delivery.Release{}, targetRecord{}, operation.Record{}, fmt.Errorf("begin diagnostic query: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var release delivery.Release
	if err := tx.GetContext(ctx, &release, releaseSelect+` WHERE id = $1`, query.ReleaseID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return delivery.Release{}, targetRecord{}, operation.Record{}, ErrReleaseNotFound
		}
		return delivery.Release{}, targetRecord{}, operation.Record{}, fmt.Errorf("load diagnostic release: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(
		ctx,
		tx,
		release.TargetSnapshot.ProjectID,
		query.ActorID,
		projectauth.PermissionRead,
	); err != nil {
		// Release 查询需要隐藏项目存在性，非成员与不存在统一表现为不可见。
		if errors.Is(err, projectauth.ErrNotMember) || errors.Is(err, projectauth.ErrForbidden) {
			return delivery.Release{}, targetRecord{}, operation.Record{}, ErrReleaseNotFound
		}
		return delivery.Release{}, targetRecord{}, operation.Record{}, err
	}

	operationRecord, err := m.operations.GetByReleaseInTransaction(ctx, tx, release.ID)
	if err != nil {
		return delivery.Release{}, targetRecord{}, operation.Record{}, err
	}
	var current targetRecord
	if err := tx.GetContext(ctx, &current, targetSelect, release.DeploymentTargetID); err != nil {
		return delivery.Release{}, targetRecord{}, operation.Record{}, fmt.Errorf("load current target for diagnostics: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return delivery.Release{}, targetRecord{}, operation.Record{}, fmt.Errorf("commit diagnostic query: %w", err)
	}
	return release, current, operationRecord, nil
}

func compareTarget(snapshot delivery.TargetSnapshot, current targetRecord) []TargetDifference {
	differences := make([]TargetDifference, 0)
	appendDifference := func(field string, releaseValue string, currentValue string) {
		if releaseValue != currentValue {
			differences = append(differences, TargetDifference{
				Field: field, ReleaseValue: releaseValue, CurrentValue: currentValue,
			})
		}
	}
	appendDifference("projectId", snapshot.ProjectID.String(), current.ProjectID.String())
	appendDifference("applicationId", snapshot.ApplicationID.String(), current.ApplicationID.String())
	appendDifference("stage", snapshot.Stage, current.Stage)
	appendDifference("clusterRef", snapshot.ClusterRef, current.ClusterRef)
	appendDifference("namespace", snapshot.Namespace, current.Namespace)
	appendDifference("replicas", strconv.Itoa(snapshot.Replicas), strconv.Itoa(current.Replicas))
	appendDifference("containerPort", strconv.Itoa(snapshot.ContainerPort), strconv.Itoa(current.ContainerPort))
	return differences
}

const releaseSelect = `SELECT id, deployment_target_id, image_reference,
       target_snapshot, rollback_of_release_id, created_by, created_at
 FROM releases`

const targetSelect = `SELECT applications.project_id, deployment_targets.application_id,
       deployment_targets.stage, deployment_targets.cluster_ref,
       deployment_targets.namespace, deployment_targets.replicas,
       deployment_targets.container_port
 FROM deployment_targets
 JOIN applications ON applications.id = deployment_targets.application_id
 WHERE deployment_targets.id = $1`
