// Package build 接纳不可变源码构建输入，并提供 Build 与 ImageArtifact 的查询表面。
package build

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrApplicationNotFound = errors.New("application not found")
	ErrInvalidInput        = errors.New("invalid build input")
	ErrNotFound            = errors.New("build not found")
)

type Config struct {
	AllowedGitHosts []string
	Platform        string
	RegistryHost    string
	RegistryPrefix  string
}

type Record struct {
	ID                    uuid.UUID `db:"id"`
	ProjectID             uuid.UUID `db:"project_id"`
	ApplicationID         uuid.UUID `db:"application_id"`
	RepositoryURL         string    `db:"repository_url"`
	SourceCommit          string    `db:"source_commit"`
	DockerfilePath        string    `db:"dockerfile_path"`
	ContextPath           string    `db:"context_path"`
	Platform              string    `db:"platform"`
	DestinationRepository string    `db:"destination_repository"`
	InputDigest           string    `db:"input_digest"`
	CreatedBy             string    `db:"created_by"`
	CreatedAt             time.Time `db:"created_at"`
}

type Acceptance struct {
	Build          Record
	BuildOperation buildoperation.Record
}

type CreateCommand struct {
	ApplicationID  uuid.UUID
	RepositoryURL  string
	SourceCommit   string
	DockerfilePath string
	ContextPath    string
	ActorID        string
	IdempotencyKey string
	TraceParent    string
	TraceState     string
}

type normalizedInput struct {
	RepositoryURL         string    `json:"repositoryUrl"`
	SourceCommit          string    `json:"sourceCommit"`
	DockerfilePath        string    `json:"dockerfilePath"`
	ContextPath           string    `json:"contextPath"`
	Platform              string    `json:"platform"`
	DestinationRepository string    `json:"destinationRepository"`
	ApplicationID         uuid.UUID `json:"applicationId"`
}

type Module struct {
	db              *sqlx.DB
	config          Config
	buildOperations *buildoperation.Module
	authorizer      *projectauth.Module
}

func New(db *sqlx.DB, config Config, operations *buildoperation.Module, authorizer *projectauth.Module) *Module {
	return &Module{db: db, config: config, buildOperations: operations, authorizer: authorizer}
}

// Create 原子完成授权、输入冻结、幂等接纳、BuildOperation/Dispatch 创建和审计。
func (m *Module) Create(ctx context.Context, command CreateCommand) (Acceptance, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Acceptance{}, fmt.Errorf("begin create build: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var owner struct {
		ProjectID uuid.UUID `db:"project_id"`
	}
	if err := tx.GetContext(ctx, &owner, `SELECT project_id FROM applications WHERE id = $1`, command.ApplicationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Acceptance{}, ErrApplicationNotFound
		}
		return Acceptance{}, fmt.Errorf("load build application: %w", err)
	}
	if err := m.authorizer.RequireInTransaction(ctx, tx, owner.ProjectID, command.ActorID, projectauth.PermissionDevelop); err != nil {
		return Acceptance{}, err
	}

	input, err := m.normalize(command, owner.ProjectID)
	if err != nil {
		return Acceptance{}, err
	}
	requestHash, err := idempotency.Fingerprint(input)
	if err != nil {
		return Acceptance{}, fmt.Errorf("fingerprint create build: %w", err)
	}
	createdAt := time.Now().UTC()
	buildID := uuid.New()
	scope := idempotency.Scope{ActorID: command.ActorID, CommandType: "build.create", Key: command.IdempotencyKey}
	resourceID, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, buildID, createdAt)
	if err != nil {
		return Acceptance{}, err
	}
	if !isNew {
		return replayCreate(ctx, tx, resourceID)
	}

	record := Record{
		ID: buildID, ProjectID: owner.ProjectID, ApplicationID: command.ApplicationID,
		RepositoryURL: input.RepositoryURL, SourceCommit: input.SourceCommit,
		DockerfilePath: input.DockerfilePath, ContextPath: input.ContextPath,
		Platform: input.Platform, DestinationRepository: input.DestinationRepository,
		InputDigest: digestInput(requestHash), CreatedBy: command.ActorID, CreatedAt: createdAt,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO builds
	 (id, project_id, application_id, repository_url, source_commit, dockerfile_path,
	  context_path, platform, destination_repository, input_digest, created_by, created_at)
	 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, record.ID, record.ProjectID,
		record.ApplicationID, record.RepositoryURL, record.SourceCommit, record.DockerfilePath,
		record.ContextPath, record.Platform, record.DestinationRepository, record.InputDigest,
		record.CreatedBy, record.CreatedAt); err != nil {
		return Acceptance{}, fmt.Errorf("insert build: %w", err)
	}
	operation, err := m.buildOperations.CreatePending(ctx, tx, buildoperation.CreatePendingCommand{
		ID: uuid.New(), BuildID: record.ID, ActorID: command.ActorID,
		IdempotencyKey: command.IdempotencyKey, TraceParent: command.TraceParent,
		TraceState: command.TraceState, CreatedAt: createdAt,
	})
	if err != nil {
		return Acceptance{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID: command.ActorID, Action: "build.create", TargetType: "build", TargetID: record.ID,
		Summary:   map[string]string{"applicationId": record.ApplicationID.String(), "buildOperationId": operation.ID.String(), "sourceCommit": record.SourceCommit},
		CreatedAt: createdAt,
	}); err != nil {
		return Acceptance{}, err
	}
	if err := tx.Commit(); err != nil {
		return Acceptance{}, fmt.Errorf("commit create build: %w", err)
	}
	return Acceptance{Build: record, BuildOperation: operation}, nil
}

// Get 返回一个 Build 及其当前 Operation；权限从冻结的 Project 归属判断。
func (m *Module) Get(ctx context.Context, id uuid.UUID, actorID string) (Acceptance, error) {
	record, err := m.GetForExecution(ctx, id)
	if err != nil {
		return Acceptance{}, err
	}
	if err := m.authorizer.Require(ctx, record.ProjectID, actorID, projectauth.PermissionRead); err != nil {
		return Acceptance{}, err
	}
	var operation buildoperation.Record
	if err := m.db.GetContext(ctx, &operation, buildOperationSelect+` WHERE build_id = $1`, record.ID); err != nil {
		return Acceptance{}, fmt.Errorf("get build operation: %w", err)
	}
	return Acceptance{Build: record, BuildOperation: operation}, nil
}

// GetForExecution 只供已经通过 BuildDispatch 取得业务 Lease 的 Worker 读取冻结输入。
// 它不接受用户身份，也不得从 HTTP Adapter 直接调用。
func (m *Module) GetForExecution(ctx context.Context, id uuid.UUID) (Record, error) {
	var record Record
	if err := m.db.GetContext(ctx, &record, buildSelect+` WHERE id = $1`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Record{}, ErrNotFound
		}
		return Record{}, fmt.Errorf("get build for execution: %w", err)
	}
	return record, nil
}

func (m *Module) normalize(command CreateCommand, projectID uuid.UUID) (normalizedInput, error) {
	repositoryURL, err := normalizeRepositoryURL(command.RepositoryURL, m.config.AllowedGitHosts)
	if err != nil {
		return normalizedInput{}, err
	}
	commit := strings.ToLower(strings.TrimSpace(command.SourceCommit))
	if !isCommit(commit) {
		return normalizedInput{}, fmt.Errorf("%w: source commit must be a full hexadecimal SHA", ErrInvalidInput)
	}
	dockerfilePath, err := normalizeBuildPath(command.DockerfilePath, "Dockerfile", false)
	if err != nil {
		return normalizedInput{}, err
	}
	contextPath, err := normalizeBuildPath(command.ContextPath, ".", true)
	if err != nil {
		return normalizedInput{}, err
	}
	if contextPath != "." && dockerfilePath != contextPath && !strings.HasPrefix(dockerfilePath, contextPath+"/") {
		return normalizedInput{}, fmt.Errorf("%w: Dockerfile must be inside the build context", ErrInvalidInput)
	}
	platform := strings.TrimSpace(m.config.Platform)
	registryHost := strings.TrimSuffix(strings.TrimSpace(m.config.RegistryHost), "/")
	registryPrefix := strings.Trim(strings.TrimSpace(m.config.RegistryPrefix), "/")
	if platform == "" || registryHost == "" || registryPrefix == "" {
		return normalizedInput{}, fmt.Errorf("%w: build runtime configuration is incomplete", ErrInvalidInput)
	}
	return normalizedInput{
		RepositoryURL: repositoryURL, SourceCommit: commit, DockerfilePath: dockerfilePath,
		ContextPath: contextPath, Platform: platform, ApplicationID: command.ApplicationID,
		DestinationRepository: registryHost + "/" + registryPrefix + "/" + projectID.String() + "/" + command.ApplicationID.String(),
	}, nil
}

func normalizeRepositoryURL(value string, allowedHosts []string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: repository URL must be credential-free HTTPS", ErrInvalidInput)
	}
	allowed := false
	for _, host := range allowedHosts {
		if strings.EqualFold(strings.TrimSpace(host), parsed.Host) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("%w: repository host is not allowed", ErrInvalidInput)
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

func normalizeBuildPath(value string, fallback string, allowDot bool) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%w: build paths must be repository-relative", ErrInvalidInput)
	}
	normalized := path.Clean(value)
	if normalized == ".." || strings.HasPrefix(normalized, "../") || (!allowDot && normalized == ".") {
		return "", fmt.Errorf("%w: build path escapes the repository", ErrInvalidInput)
	}
	return normalized, nil
}

func isCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digestInput(hash []byte) string {
	return "sha256:" + hex.EncodeToString(hash)
}

const buildSelect = `SELECT id, project_id, application_id, repository_url, source_commit,
 dockerfile_path, context_path, platform, destination_repository, input_digest, created_by, created_at FROM builds`

const buildOperationSelect = `SELECT id, build_id, actor_id, idempotency_key, traceparent, tracestate,
 status, attempt_count, automatic_retry_count, recovery_required, error_code, error_summary,
 retry_disposition, queued_at, available_at, current_dispatch_sequence, created_at, updated_at,
 started_at, finished_at FROM build_operations`

func replayCreate(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (Acceptance, error) {
	var record Record
	if err := tx.GetContext(ctx, &record, buildSelect+` WHERE id = $1`, id); err != nil {
		return Acceptance{}, fmt.Errorf("replay build: %w", err)
	}
	var operation buildoperation.Record
	if err := tx.GetContext(ctx, &operation, buildOperationSelect+` WHERE build_id = $1`, record.ID); err != nil {
		return Acceptance{}, fmt.Errorf("replay build operation: %w", err)
	}
	return Acceptance{Build: record, BuildOperation: operation}, nil
}
