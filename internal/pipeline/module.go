package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/catalog"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

var (
	ErrNotFound               = errors.New("delivery pipeline not found")
	ErrApplicationNotFound    = errors.New("application not found")
	ErrTargetNotFound         = errors.New("deployment target not found")
	ErrAutoReleaseTargetStage = errors.New("auto_release target must be development")
	ErrInvalidInput           = errors.New("invalid delivery pipeline input")
	ErrRevisionConflict       = errors.New("delivery pipeline revision conflict")
	ErrEnabledConflict        = errors.New("an enabled delivery pipeline already owns this source")
	ErrNameConflict           = errors.New("delivery pipeline name already exists")
)

var endpointKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type Config struct {
	Platform             string
	SourceRecoveryWindow time.Duration
	SourceRetryBaseDelay time.Duration
	Recorder             Recorder
}

// Recorder 只接受低基数编排结论，禁止传入资源 ID 或外部内容。
type Recorder interface {
	RecordGitHubRead(string)
	RecordTransition(string, string, string, time.Duration)
	RecordMaintenance(string)
}

type noopRecorder struct{}

func (noopRecorder) RecordGitHubRead(string)                                {}
func (noopRecorder) RecordTransition(string, string, string, time.Duration) {}
func (noopRecorder) RecordMaintenance(string)                               {}

type Module struct {
	db         *sqlx.DB
	config     Config
	builds     *build.Module
	releases   *delivery.Module
	authorizer *projectauth.Module
	inspector  GitSourceInspector
	recorder   Recorder
}

// New 建立深 Pipeline 模块；Git Provider 细节被限制在 Inspector 边界之外。
func New(db *sqlx.DB, config Config, builds *build.Module, releases *delivery.Module, authorizer *projectauth.Module, inspector GitSourceInspector) *Module {
	if inspector == nil {
		inspector = unavailableInspector{}
	}
	if config.SourceRecoveryWindow <= 0 {
		config.SourceRecoveryWindow = 15 * time.Minute
	}
	if config.SourceRetryBaseDelay <= 0 {
		config.SourceRetryBaseDelay = 5 * time.Second
	}
	if config.Recorder == nil {
		config.Recorder = noopRecorder{}
	}
	return &Module{db: db, config: config, builds: builds, releases: releases, authorizer: authorizer, inspector: inspector, recorder: config.Recorder}
}

// Create 创建禁用的 Pipeline 和首个不可变 Revision；启用必须走独立命令再次核验来源。
func (m *Module) Create(ctx context.Context, command CreateCommand) (Detail, error) {
	normalized, source, err := m.prepareSource(ctx, command.EndpointKey, command.RepositoryURL, command.Branch, command.DockerfilePath, command.ContextPath, command.Mode, command.DeploymentTargetID)
	if err != nil {
		return Detail{}, err
	}
	name := strings.TrimSpace(command.Name)
	if name == "" || len(name) > 100 {
		return Detail{}, fmt.Errorf("%w: name must contain 1 to 100 characters", ErrInvalidInput)
	}

	now := time.Now().UTC()
	pipelineID := uuid.New()
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "delivery_pipeline.create", Key: command.IdempotencyKey}
	fingerprint, err := idempotency.Fingerprint(struct {
		ApplicationID uuid.UUID
		Name          string
		Input         normalizedConfig
	}{command.ApplicationID, name, normalized})
	if err != nil {
		return Detail{}, fmt.Errorf("fingerprint create delivery pipeline: %w", err)
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Detail{}, fmt.Errorf("begin create delivery pipeline: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return Detail{}, err
	}

	projectID, err := m.applicationProject(ctx, tx, command.ApplicationID)
	if err != nil {
		return Detail{}, err
	}
	if err := m.authorizer.RequireInTransaction(ctx, tx, projectID, command.Caller, projectauth.PermissionDevelop); err != nil {
		return Detail{}, err
	}
	if err := m.validateTarget(ctx, tx, projectID, command.ApplicationID, normalized.Mode, normalized.DeploymentTargetID); err != nil {
		return Detail{}, err
	}
	resourceID, isNew, err := idempotency.Claim(ctx, tx, scope, fingerprint, pipelineID, now)
	if err != nil {
		return Detail{}, err
	}
	if !isNew {
		return m.getWith(ctx, tx, resourceID)
	}

	record := Record{ID: pipelineID, ProjectID: projectID, ApplicationID: command.ApplicationID, Name: name, CurrentRevision: 1, CreatedBy: command.Caller.ActorID(), CreatedAt: now, UpdatedAt: now}
	revision := makeRevision(record.ID, 1, normalized, source, command.Caller.ActorID(), now)
	// 当前 Revision 外键是延迟约束，因此根记录和首个 Revision 可以在同一事务建立。
	if _, err := tx.ExecContext(ctx, `INSERT INTO delivery_pipelines
		(id, project_id, application_id, name, current_revision, enabled, activation_generation, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,false,0,$6,$7,$7)`, record.ID, record.ProjectID, record.ApplicationID, record.Name, record.CurrentRevision, record.CreatedBy, now); err != nil {
		if isUniqueConstraint(err, "delivery_pipelines_application_id_name_key") {
			return Detail{}, ErrNameConflict
		}
		return Detail{}, fmt.Errorf("insert delivery pipeline: %w", err)
	}
	if err := insertRevision(ctx, tx, revision); err != nil {
		return Detail{}, err
	}
	if err := appendAudit(ctx, tx, command.Caller.ActorID(), "delivery_pipeline.create", record, revision, now); err != nil {
		return Detail{}, err
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, fmt.Errorf("commit create delivery pipeline: %w", err)
	}
	return Detail{Pipeline: record, Revision: revision}, nil
}

// Update 以乐观 Revision 前置条件追加配置；旧 Revision 继续为历史 Run 提供证据。
func (m *Module) Update(ctx context.Context, command UpdateCommand) (Detail, error) {
	normalized, source, err := m.prepareSource(ctx, command.EndpointKey, command.RepositoryURL, command.Branch, command.DockerfilePath, command.ContextPath, command.Mode, command.DeploymentTargetID)
	if err != nil {
		return Detail{}, err
	}
	fingerprint, err := idempotency.Fingerprint(struct {
		PipelineID       uuid.UUID
		ExpectedRevision int
		Input            normalizedConfig
	}{command.PipelineID, command.ExpectedRevision, normalized})
	if err != nil {
		return Detail{}, fmt.Errorf("fingerprint update delivery pipeline: %w", err)
	}
	return m.change(ctx, command.Caller, command.IdempotencyKey, "delivery_pipeline.update", command.PipelineID, fingerprint, func(tx *sqlx.Tx, current Detail, now time.Time) (Detail, error) {
		if current.Pipeline.CurrentRevision != command.ExpectedRevision {
			return Detail{}, ErrRevisionConflict
		}
		if err := m.validateTarget(ctx, tx, current.Pipeline.ProjectID, current.Pipeline.ApplicationID, normalized.Mode, normalized.DeploymentTargetID); err != nil {
			return Detail{}, err
		}
		if current.Pipeline.Enabled {
			if err := m.lockApplicationAndCheckSource(ctx, tx, current.Pipeline, source.RepositoryID, source.GitRef); err != nil {
				return Detail{}, err
			}
		}
		current.Pipeline.CurrentRevision++
		current.Pipeline.UpdatedAt = now
		current.Revision = makeRevision(current.Pipeline.ID, current.Pipeline.CurrentRevision, normalized, source, command.Caller.ActorID(), now)
		if err := insertRevision(ctx, tx, current.Revision); err != nil {
			return Detail{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_pipelines SET current_revision=$2, updated_at=$3 WHERE id=$1`, current.Pipeline.ID, current.Pipeline.CurrentRevision, now); err != nil {
			return Detail{}, fmt.Errorf("advance delivery pipeline revision: %w", err)
		}
		if err := appendAudit(ctx, tx, command.Caller.ActorID(), "delivery_pipeline.update", current.Pipeline, current.Revision, now); err != nil {
			return Detail{}, err
		}
		return current, nil
	})
}

// Enable 在网络核验完成后重新锁定当前 Revision，并原子检查同应用源码所有权。
func (m *Module) Enable(ctx context.Context, command StateCommand) (Detail, error) {
	before, err := m.Get(ctx, command.PipelineID, command.Caller)
	if err != nil {
		return Detail{}, err
	}
	if err := m.authorizer.Require(ctx, before.Pipeline.ProjectID, command.Caller, projectauth.PermissionDevelop); err != nil {
		return Detail{}, err
	}
	resolved, err := m.inspector.Resolve(ctx, SourceRequest{EndpointKey: before.Revision.EndpointKey, RepositoryURL: before.Revision.RepositoryURL, Branch: strings.TrimPrefix(before.Revision.GitRef, "refs/heads/")})
	if err != nil {
		return Detail{}, fmt.Errorf("verify delivery pipeline source: %w", err)
	}
	if resolved.RepositoryID != before.Revision.RepositoryID || resolved.OwnerID != before.Revision.RepositoryOwnerID || resolved.GitRef != before.Revision.GitRef {
		return Detail{}, fmt.Errorf("%w: repository identity changed; create a new revision", ErrInvalidInput)
	}
	fingerprint, err := idempotency.Fingerprint(struct{ PipelineID uuid.UUID }{command.PipelineID})
	if err != nil {
		return Detail{}, fmt.Errorf("fingerprint enable delivery pipeline: %w", err)
	}
	return m.change(ctx, command.Caller, command.IdempotencyKey, "delivery_pipeline.enable", command.PipelineID, fingerprint, func(tx *sqlx.Tx, current Detail, now time.Time) (Detail, error) {
		if current.Pipeline.CurrentRevision != before.Pipeline.CurrentRevision {
			return Detail{}, ErrRevisionConflict
		}
		if err := m.validateTarget(ctx, tx, current.Pipeline.ProjectID, current.Pipeline.ApplicationID, current.Revision.Mode, current.Revision.DeploymentTargetID); err != nil {
			return Detail{}, err
		}
		if current.Pipeline.Enabled {
			return current, nil
		}
		// 锁定 Application 可串行化所有 Pipeline 的启用判定，避免并发绕过重叠检查。
		if err := m.lockApplicationAndCheckSource(ctx, tx, current.Pipeline, current.Revision.RepositoryID, current.Revision.GitRef); err != nil {
			return Detail{}, err
		}
		current.Pipeline.Enabled = true
		current.Pipeline.ActivationGeneration++
		current.Pipeline.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_pipelines SET enabled=true, activation_generation=$2, updated_at=$3 WHERE id=$1`, current.Pipeline.ID, current.Pipeline.ActivationGeneration, now); err != nil {
			return Detail{}, fmt.Errorf("enable delivery pipeline: %w", err)
		}
		return current, appendAudit(ctx, tx, command.Caller.ActorID(), "delivery_pipeline.enable", current.Pipeline, current.Revision, now)
	})
}

// Disable 递增 Activation Generation，使禁用前产生的 Run 永久失去自动发布资格。
func (m *Module) Disable(ctx context.Context, command StateCommand) (Detail, error) {
	fingerprint, err := idempotency.Fingerprint(struct{ PipelineID uuid.UUID }{command.PipelineID})
	if err != nil {
		return Detail{}, fmt.Errorf("fingerprint disable delivery pipeline: %w", err)
	}
	return m.change(ctx, command.Caller, command.IdempotencyKey, "delivery_pipeline.disable", command.PipelineID, fingerprint, func(tx *sqlx.Tx, current Detail, now time.Time) (Detail, error) {
		if !current.Pipeline.Enabled {
			return current, nil
		}
		current.Pipeline.Enabled = false
		current.Pipeline.ActivationGeneration++
		current.Pipeline.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE delivery_pipelines SET enabled=false, activation_generation=$2, updated_at=$3 WHERE id=$1`, current.Pipeline.ID, current.Pipeline.ActivationGeneration, now); err != nil {
			return Detail{}, fmt.Errorf("disable delivery pipeline: %w", err)
		}
		return current, appendAudit(ctx, tx, command.Caller.ActorID(), "delivery_pipeline.disable", current.Pipeline, current.Revision, now)
	})
}

type normalizedConfig struct {
	EndpointKey        string
	DockerfilePath     string
	ContextPath        string
	Platform           string
	Mode               string
	DeploymentTargetID *uuid.UUID
}

func (m *Module) prepareSource(ctx context.Context, endpointKey, repositoryURL, branch, dockerfilePath, contextPath, mode string, targetID *uuid.UUID) (normalizedConfig, SourceIdentity, error) {
	endpointKey = strings.TrimSpace(endpointKey)
	branch = strings.TrimSpace(strings.TrimPrefix(branch, "refs/heads/"))
	if endpointKey == "" || len(endpointKey) > 128 || !endpointKeyPattern.MatchString(endpointKey) || !validBranch(branch) {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: endpoint key and branch are required", ErrInvalidInput)
	}
	dockerfilePath, err := normalizePath(dockerfilePath, "Dockerfile", false)
	if err != nil {
		return normalizedConfig{}, SourceIdentity{}, err
	}
	contextPath, err = normalizePath(contextPath, ".", true)
	if err != nil {
		return normalizedConfig{}, SourceIdentity{}, err
	}
	if contextPath != "." && dockerfilePath != contextPath && !strings.HasPrefix(dockerfilePath, contextPath+"/") {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: Dockerfile must be inside the build context", ErrInvalidInput)
	}
	mode = strings.TrimSpace(mode)
	if mode != ModeBuildOnly && mode != ModeAutoRelease {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: unsupported delivery mode", ErrInvalidInput)
	}
	if (mode == ModeAutoRelease) != (targetID != nil) {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: auto_release requires exactly one deployment target", ErrInvalidInput)
	}
	platform := strings.TrimSpace(m.config.Platform)
	if platform == "" {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: build platform is not configured", ErrInvalidInput)
	}
	source, err := m.inspector.Resolve(ctx, SourceRequest{EndpointKey: endpointKey, RepositoryURL: repositoryURL, Branch: branch})
	if err != nil {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("resolve delivery pipeline source: %w", err)
	}
	if source.RepositoryID <= 0 || source.OwnerID <= 0 || source.RepositoryURL == "" || source.RepositoryName == "" || source.GitRef != "refs/heads/"+branch {
		return normalizedConfig{}, SourceIdentity{}, fmt.Errorf("%w: source inspector returned an invalid identity", ErrInvalidInput)
	}
	return normalizedConfig{endpointKey, dockerfilePath, contextPath, platform, mode, targetID}, source, nil
}

func normalizePath(value, fallback string, allowDot bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%w: build paths must be repository-relative", ErrInvalidInput)
	}
	value = path.Clean(value)
	if value == ".." || strings.HasPrefix(value, "../") || (!allowDot && value == ".") {
		return "", fmt.Errorf("%w: build path escapes the repository", ErrInvalidInput)
	}
	return value, nil
}

// validBranch 拒绝 Git check-ref-format 中会改变路径或协议解释的危险形式。
func validBranch(value string) bool {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.HasSuffix(value, ".lock") || strings.Contains(value, "//") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	return !strings.ContainsAny(value, " ~^:?*[\\")
}

func (m *Module) applicationProject(ctx context.Context, tx *sqlx.Tx, applicationID uuid.UUID) (uuid.UUID, error) {
	var projectID uuid.UUID
	if err := tx.GetContext(ctx, &projectID, `SELECT project_id FROM applications WHERE id=$1`, applicationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return uuid.Nil, ErrApplicationNotFound
		}
		return uuid.Nil, fmt.Errorf("load delivery pipeline application: %w", err)
	}
	return projectID, nil
}

func (m *Module) validateTarget(ctx context.Context, tx *sqlx.Tx, projectID, applicationID uuid.UUID, mode string, targetID *uuid.UUID) error {
	if mode == ModeBuildOnly {
		return nil
	}
	var stage string
	if err := tx.GetContext(ctx, &stage, `SELECT t.stage FROM deployment_targets t
		JOIN applications a ON a.id=t.application_id
		WHERE t.id=$1 AND t.application_id=$2 AND a.project_id=$3`, targetID, applicationID, projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTargetNotFound
		}
		return fmt.Errorf("validate delivery pipeline target: %w", err)
	}
	// 自动发布的配置入口与最终接纳共同限制目的地，避免系统身份绕开人工生产发布。
	if stage != catalog.DevelopmentStage {
		return ErrAutoReleaseTargetStage
	}
	return nil
}

func (m *Module) lockApplicationAndCheckSource(ctx context.Context, tx *sqlx.Tx, record Record, repositoryID int64, gitRef string) error {
	if _, err := tx.ExecContext(ctx, `SELECT id FROM applications WHERE id=$1 FOR UPDATE`, record.ApplicationID); err != nil {
		return fmt.Errorf("lock delivery pipeline application: %w", err)
	}
	var conflict bool
	if err := tx.GetContext(ctx, &conflict, `SELECT EXISTS (
		SELECT 1 FROM delivery_pipelines p
		JOIN delivery_pipeline_revisions r ON r.delivery_pipeline_id=p.id AND r.revision=p.current_revision
		WHERE p.application_id=$1 AND p.enabled AND p.id<>$2 AND r.repository_id=$3 AND r.git_ref=$4)`, record.ApplicationID, record.ID, repositoryID, gitRef); err != nil {
		return fmt.Errorf("check enabled delivery pipeline overlap: %w", err)
	}
	if conflict {
		return ErrEnabledConflict
	}
	return nil
}

func makeRevision(pipelineID uuid.UUID, number int, input normalizedConfig, source SourceIdentity, actorID string, now time.Time) Revision {
	return Revision{PipelineID: pipelineID, Revision: number, Provider: ProviderGitHub, EndpointKey: input.EndpointKey, RepositoryID: source.RepositoryID, RepositoryOwnerID: source.OwnerID, RepositoryFullName: source.RepositoryName, RepositoryURL: source.RepositoryURL, GitRef: source.GitRef, DockerfilePath: input.DockerfilePath, ContextPath: input.ContextPath, Platform: input.Platform, Mode: input.Mode, DeploymentTargetID: input.DeploymentTargetID, CreatedBy: actorID, CreatedAt: now}
}

func insertRevision(ctx context.Context, tx *sqlx.Tx, revision Revision) error {
	if revision.Platform == "" {
		// Platform 由调用模块在构造 Revision 后补齐，防止客户端选择 Worker 架构。
		return errors.New("delivery pipeline revision platform is empty")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO delivery_pipeline_revisions
		(delivery_pipeline_id, revision, provider, endpoint_key, repository_id, repository_owner_id, repository_full_name, repository_url, git_ref, dockerfile_path, context_path, platform, mode, deployment_target_id, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, revision.PipelineID, revision.Revision, revision.Provider, revision.EndpointKey, revision.RepositoryID, revision.RepositoryOwnerID, revision.RepositoryFullName, revision.RepositoryURL, revision.GitRef, revision.DockerfilePath, revision.ContextPath, revision.Platform, revision.Mode, revision.DeploymentTargetID, revision.CreatedBy, revision.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert delivery pipeline revision: %w", err)
	}
	return nil
}

func appendAudit(ctx context.Context, tx *sqlx.Tx, actorID, action string, record Record, revision Revision, now time.Time) error {
	return audit.Append(ctx, tx, audit.Entry{ActorID: actorID, Action: action, TargetType: "delivery_pipeline", TargetID: record.ID, Summary: map[string]any{"projectId": record.ProjectID, "applicationId": record.ApplicationID, "revision": record.CurrentRevision, "activationGeneration": record.ActivationGeneration, "repositoryId": revision.RepositoryID, "gitRef": revision.GitRef, "mode": revision.Mode}, CreatedAt: now})
}

func (m *Module) change(ctx context.Context, caller identity.Caller, key, commandType string, pipelineID uuid.UUID, fingerprint []byte, mutate func(*sqlx.Tx, Detail, time.Time) (Detail, error)) (Detail, error) {
	actorID := caller.ActorID()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Detail{}, fmt.Errorf("begin %s: %w", commandType, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, caller); err != nil {
		return Detail{}, err
	}
	current, err := m.getWithLock(ctx, tx, pipelineID)
	if err != nil {
		return Detail{}, err
	}
	if err := m.authorizer.RequireInTransaction(ctx, tx, current.Pipeline.ProjectID, caller, projectauth.PermissionDevelop); err != nil {
		return Detail{}, err
	}
	now := time.Now().UTC()
	scope := idempotency.Scope{ActorID: actorID, CommandType: commandType, Key: key}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, fingerprint, pipelineID, now)
	if err != nil {
		return Detail{}, err
	}
	if !isNew {
		var replay Detail
		if err := idempotency.LoadResponse(ctx, tx, scope, &replay); err != nil {
			return Detail{}, err
		}
		return replay, tx.Commit()
	}
	updated, err := mutate(tx, current, now)
	if err != nil {
		return Detail{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, updated); err != nil {
		return Detail{}, err
	}
	if err := tx.Commit(); err != nil {
		return Detail{}, fmt.Errorf("commit %s: %w", commandType, err)
	}
	return updated, nil
}

func isUniqueConstraint(err error, constraint string) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505" && databaseError.ConstraintName == constraint
}
