// Package projectauth 统一管理项目成员、角色与授权决策。
package projectauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/OrbitOps/internal/audit"
	"github.com/HasonoCell/OrbitOps/internal/idempotency"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

const (
	RoleOwner     = "owner"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

type Permission string

const (
	PermissionRead           Permission = "read"
	PermissionDevelop        Permission = "develop"
	PermissionManageMembers  Permission = "manage_members"
	PermissionResolveUnknown Permission = "resolve_unknown"
)

var (
	ErrForbidden           = errors.New("project permission denied")
	ErrIdempotencyConflict = idempotency.ErrConflict
	ErrInvalidActorID      = errors.New("invalid actor ID")
	ErrInvalidRole         = errors.New("invalid project role")
	ErrLastOwner           = errors.New("project must retain at least one owner")
	ErrMemberExists        = errors.New("project member already exists")
	ErrMemberNotFound      = errors.New("project member not found")
	ErrNotMember           = errors.New("project is not visible to actor")
)

type Member struct {
	ProjectID uuid.UUID `db:"project_id" json:"projectId"`
	ActorID   string    `db:"actor_id" json:"actorId"`
	Role      string    `db:"role" json:"role"`
	CreatedBy string    `db:"created_by" json:"createdBy"`
	CreatedAt time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt time.Time `db:"updated_at" json:"updatedAt"`
}

type AddMemberCommand struct {
	ProjectID      uuid.UUID
	MemberActorID  string
	Role           string
	ActorID        string
	IdempotencyKey string
}

type UpdateMemberCommand struct {
	ProjectID      uuid.UUID
	MemberActorID  string
	Role           string
	ActorID        string
	IdempotencyKey string
}

type RemoveMemberCommand struct {
	ProjectID      uuid.UUID
	MemberActorID  string
	ActorID        string
	IdempotencyKey string
}

type Module struct {
	db *sqlx.DB
}

// New 创建以 PostgreSQL 成员关系为事实权威的项目权限模块。
func New(db *sqlx.DB) *Module {
	return &Module{db: db}
}

// ValidateActorID 校验本地身份和成员命令共享的稳定 Actor ID 约束。
func ValidateActorID(actorID string) error {
	return validateActorID(actorID)
}

// CreateInitialOwner 在项目创建事务中写入首个 owner，避免出现无所有者项目。
func (m *Module) CreateInitialOwner(
	ctx context.Context,
	tx *sqlx.Tx,
	projectID uuid.UUID,
	actorID string,
	createdAt time.Time,
) error {
	if err := validateActorID(actorID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO project_members
		 (project_id, actor_id, role, created_by, created_at, updated_at)
		 VALUES ($1, $2, 'owner', $2, $3, $3)`,
		projectID,
		actorID,
		createdAt,
	); err != nil {
		return fmt.Errorf("insert initial project owner: %w", err)
	}
	return nil
}

// Require 判断 Actor 是否拥有指定项目能力。
func (m *Module) Require(
	ctx context.Context,
	projectID uuid.UUID,
	actorID string,
	permission Permission,
) error {
	return require(ctx, m.db, projectID, actorID, permission)
}

// RequireInTransaction 在领域写事务中执行同一套授权规则。
func (m *Module) RequireInTransaction(
	ctx context.Context,
	tx *sqlx.Tx,
	projectID uuid.UUID,
	actorID string,
	permission Permission,
) error {
	return require(ctx, tx, projectID, actorID, permission)
}

// ListMembers 返回项目成员的稳定列表；任一项目成员都可以读取。
func (m *Module) ListMembers(
	ctx context.Context,
	projectID uuid.UUID,
	actorID string,
) ([]Member, error) {
	if err := m.Require(ctx, projectID, actorID, PermissionRead); err != nil {
		return nil, err
	}

	members := make([]Member, 0)
	if err := m.db.SelectContext(
		ctx,
		&members,
		memberSelect+` WHERE project_id = $1 ORDER BY actor_id`,
		projectID,
	); err != nil {
		return nil, fmt.Errorf("list project members: %w", err)
	}
	return members, nil
}

// AddMember 由 owner 幂等地添加项目成员。
func (m *Module) AddMember(
	ctx context.Context,
	command AddMemberCommand,
) (Member, error) {
	if err := validateActorID(command.MemberActorID); err != nil {
		return Member{}, err
	}
	if err := validateRole(command.Role); err != nil {
		return Member{}, err
	}

	requestHash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID `json:"projectId"`
		ActorID   string    `json:"actorId"`
		Role      string    `json:"role"`
	}{command.ProjectID, command.MemberActorID, command.Role})
	if err != nil {
		return Member{}, fmt.Errorf("fingerprint add project member: %w", err)
	}

	now := time.Now().UTC()
	member := Member{
		ProjectID: command.ProjectID,
		ActorID:   command.MemberActorID,
		Role:      command.Role,
		CreatedBy: command.ActorID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Member{}, fmt.Errorf("begin add project member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.lockForMemberChange(ctx, tx, command.ProjectID, command.ActorID); err != nil {
		return Member{}, err
	}
	scope := idempotency.Scope{
		ActorID:     command.ActorID,
		CommandType: "project_member.add",
		Key:         command.IdempotencyKey,
	}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, command.ProjectID, now)
	if err != nil {
		return Member{}, err
	}
	if !isNew {
		return replayMember(ctx, tx, scope, "add")
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO project_members
		 (project_id, actor_id, role, created_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		member.ProjectID,
		member.ActorID,
		member.Role,
		member.CreatedBy,
		member.CreatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return Member{}, ErrMemberExists
		}
		return Member{}, fmt.Errorf("insert project member: %w", err)
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, member); err != nil {
		return Member{}, err
	}
	if err := appendMemberAudit(ctx, tx, command.ActorID, "project_member.add", member, now); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, fmt.Errorf("commit add project member: %w", err)
	}
	return member, nil
}

// UpdateMember 由 owner 幂等地修改成员角色，并保护项目最后一个 owner。
func (m *Module) UpdateMember(
	ctx context.Context,
	command UpdateMemberCommand,
) (Member, error) {
	if err := validateActorID(command.MemberActorID); err != nil {
		return Member{}, err
	}
	if err := validateRole(command.Role); err != nil {
		return Member{}, err
	}

	requestHash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID `json:"projectId"`
		ActorID   string    `json:"actorId"`
		Role      string    `json:"role"`
	}{command.ProjectID, command.MemberActorID, command.Role})
	if err != nil {
		return Member{}, fmt.Errorf("fingerprint update project member: %w", err)
	}

	now := time.Now().UTC()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Member{}, fmt.Errorf("begin update project member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.lockForMemberChange(ctx, tx, command.ProjectID, command.ActorID); err != nil {
		return Member{}, err
	}
	scope := idempotency.Scope{
		ActorID:     command.ActorID,
		CommandType: "project_member.update",
		Key:         command.IdempotencyKey,
	}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, command.ProjectID, now)
	if err != nil {
		return Member{}, err
	}
	if !isNew {
		return replayMember(ctx, tx, scope, "update")
	}

	current, err := loadMemberForUpdate(ctx, tx, command.ProjectID, command.MemberActorID)
	if err != nil {
		return Member{}, err
	}
	if current.Role == RoleOwner && command.Role != RoleOwner {
		if err := requireAnotherOwner(ctx, tx, command.ProjectID); err != nil {
			return Member{}, err
		}
	}
	current.Role = command.Role
	current.UpdatedAt = now
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE project_members SET role = $1, updated_at = $2
		 WHERE project_id = $3 AND actor_id = $4`,
		current.Role,
		current.UpdatedAt,
		current.ProjectID,
		current.ActorID,
	); err != nil {
		return Member{}, fmt.Errorf("update project member: %w", err)
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, current); err != nil {
		return Member{}, err
	}
	if err := appendMemberAudit(ctx, tx, command.ActorID, "project_member.update", current, now); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, fmt.Errorf("commit update project member: %w", err)
	}
	return current, nil
}

// RemoveMember 由 owner 幂等地移除成员，并保护项目最后一个 owner。
func (m *Module) RemoveMember(
	ctx context.Context,
	command RemoveMemberCommand,
) (Member, error) {
	if err := validateActorID(command.MemberActorID); err != nil {
		return Member{}, err
	}

	requestHash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID `json:"projectId"`
		ActorID   string    `json:"actorId"`
	}{command.ProjectID, command.MemberActorID})
	if err != nil {
		return Member{}, fmt.Errorf("fingerprint remove project member: %w", err)
	}

	now := time.Now().UTC()
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Member{}, fmt.Errorf("begin remove project member: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := m.lockForMemberChange(ctx, tx, command.ProjectID, command.ActorID); err != nil {
		return Member{}, err
	}
	scope := idempotency.Scope{
		ActorID:     command.ActorID,
		CommandType: "project_member.remove",
		Key:         command.IdempotencyKey,
	}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, requestHash, command.ProjectID, now)
	if err != nil {
		return Member{}, err
	}
	if !isNew {
		return replayMember(ctx, tx, scope, "remove")
	}

	member, err := loadMemberForUpdate(ctx, tx, command.ProjectID, command.MemberActorID)
	if err != nil {
		return Member{}, err
	}
	if member.Role == RoleOwner {
		if err := requireAnotherOwner(ctx, tx, command.ProjectID); err != nil {
			return Member{}, err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM project_members WHERE project_id = $1 AND actor_id = $2`,
		member.ProjectID,
		member.ActorID,
	); err != nil {
		return Member{}, fmt.Errorf("remove project member: %w", err)
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, member); err != nil {
		return Member{}, err
	}
	if err := appendMemberAudit(ctx, tx, command.ActorID, "project_member.remove", member, now); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, fmt.Errorf("commit remove project member: %w", err)
	}
	return member, nil
}

func (m *Module) lockForMemberChange(
	ctx context.Context,
	tx *sqlx.Tx,
	projectID uuid.UUID,
	actorID string,
) error {
	// 先锁定项目再授权，使成员角色变化与“最后一个 owner”检查共享同一串行边界。
	var lockedID uuid.UUID
	if err := tx.GetContext(
		ctx,
		&lockedID,
		`SELECT id FROM projects WHERE id = $1 FOR UPDATE`,
		projectID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		return fmt.Errorf("lock project membership: %w", err)
	}
	return m.RequireInTransaction(
		ctx,
		tx,
		projectID,
		actorID,
		PermissionManageMembers,
	)
}

func require(
	ctx context.Context,
	queryer sqlx.QueryerContext,
	projectID uuid.UUID,
	actorID string,
	permission Permission,
) error {
	var role string
	if err := sqlx.GetContext(
		ctx,
		queryer,
		&role,
		`SELECT role FROM project_members WHERE project_id = $1 AND actor_id = $2`,
		projectID,
		actorID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotMember
		}
		return fmt.Errorf("load project role: %w", err)
	}
	if !roleAllows(role, permission) {
		return ErrForbidden
	}
	return nil
}

func roleAllows(role string, permission Permission) bool {
	switch role {
	case RoleOwner:
		return true
	case RoleDeveloper:
		return permission == PermissionRead || permission == PermissionDevelop
	case RoleViewer:
		return permission == PermissionRead
	default:
		return false
	}
}

func loadMemberForUpdate(
	ctx context.Context,
	tx *sqlx.Tx,
	projectID uuid.UUID,
	actorID string,
) (Member, error) {
	var member Member
	if err := tx.GetContext(
		ctx,
		&member,
		memberSelect+` WHERE project_id = $1 AND actor_id = $2 FOR UPDATE`,
		projectID,
		actorID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Member{}, ErrMemberNotFound
		}
		return Member{}, fmt.Errorf("load project member: %w", err)
	}
	return member, nil
}

func requireAnotherOwner(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID) error {
	var count int
	if err := tx.GetContext(
		ctx,
		&count,
		`SELECT count(*) FROM project_members WHERE project_id = $1 AND role = 'owner'`,
		projectID,
	); err != nil {
		return fmt.Errorf("count project owners: %w", err)
	}
	if count <= 1 {
		return ErrLastOwner
	}
	return nil
}

func replayMember(
	ctx context.Context,
	tx *sqlx.Tx,
	scope idempotency.Scope,
	action string,
) (Member, error) {
	var member Member
	if err := idempotency.LoadResponse(ctx, tx, scope, &member); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, fmt.Errorf("commit project member %s replay: %w", action, err)
	}
	return member, nil
}

func appendMemberAudit(
	ctx context.Context,
	tx *sqlx.Tx,
	actorID string,
	action string,
	member Member,
	createdAt time.Time,
) error {
	return audit.Append(ctx, tx, audit.Entry{
		ActorID:    actorID,
		Action:     action,
		TargetType: "project",
		TargetID:   member.ProjectID,
		Summary: map[string]string{
			"memberActorId": member.ActorID,
			"role":          member.Role,
		},
		CreatedAt: createdAt,
	})
}

func validateActorID(actorID string) error {
	if actorID == "" || strings.TrimSpace(actorID) != actorID || utf8.RuneCountInString(actorID) > 128 {
		return ErrInvalidActorID
	}
	for _, value := range actorID {
		if value < 0x20 || value == 0x7f {
			return ErrInvalidActorID
		}
	}
	return nil
}

func validateRole(role string) error {
	switch role {
	case RoleOwner, RoleDeveloper, RoleViewer:
		return nil
	default:
		return ErrInvalidRole
	}
}

func isUniqueViolation(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505"
}

const memberSelect = `SELECT project_id, actor_id, role, created_by, created_at, updated_at
 FROM project_members`
