// Package projectauth 拥有项目成员、有限能力矩阵和当前身份授权，不拥有凭据。
package projectauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

const (
	RoleOwner     = "owner"
	RoleAdmin     = "admin"
	RoleDeveloper = "developer"
	RoleViewer    = "viewer"
)

type Permission string

const (
	PermissionRead               Permission = "read"
	PermissionReadLogs           Permission = "read_logs"
	PermissionDevelop            Permission = "develop"
	PermissionManageMembers      Permission = "manage_members"
	PermissionManageOwners       Permission = "manage_owners"
	PermissionResolveUnknown     Permission = "resolve_unknown"
	PermissionManageAccessHosts  Permission = "manage_access_hosts"
	PermissionManageAccessRoutes Permission = "manage_access_routes"
)

var (
	ErrForbidden           = errors.New("project permission denied")
	ErrIdempotencyConflict = idempotency.ErrConflict
	ErrInvalidCursor       = errors.New("invalid project member cursor")
	ErrInvalidMember       = errors.New("invalid project user")
	ErrInvalidRole         = errors.New("invalid project role")
	ErrLastOwner           = identity.ErrLastProjectOwner
	ErrMemberExists        = errors.New("project member already exists")
	ErrMemberNotFound      = errors.New("project member not found")
	ErrNotMember           = errors.New("project is not visible to user")
)

type denialRecorderKey struct{}
type DenialRecorder interface{ RecordAuthorizationDenial(string) }

func WithDenialRecorder(ctx context.Context, recorder DenialRecorder) context.Context {
	return context.WithValue(ctx, denialRecorderKey{}, recorder)
}
func recordDenial(ctx context.Context, reason string) {
	if recorder, ok := ctx.Value(denialRecorderKey{}).(DenialRecorder); ok {
		recorder.RecordAuthorizationDenial(reason)
	}
}

type Member struct {
	ProjectID   uuid.UUID `db:"project_id" json:"projectId"`
	UserID      uuid.UUID `db:"user_id" json:"userId"`
	DisplayName string    `db:"display_name" json:"displayName,omitempty"`
	Role        string    `db:"role" json:"role"`
	CreatedBy   string    `db:"created_by" json:"createdBy"`
	CreatedAt   time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt   time.Time `db:"updated_at" json:"updatedAt"`
}

type MemberCandidate struct {
	UserID      uuid.UUID `db:"user_id" json:"userId"`
	DisplayName string    `db:"display_name" json:"displayName"`
}

type CandidateResult struct {
	Status    string
	Candidate *MemberCandidate
}

type MemberPage struct {
	Items      []Member
	NextCursor *string
}

type memberCursor struct {
	Scope     string    `json:"scope"`
	ProjectID uuid.UUID `json:"projectId"`
	CreatedAt time.Time `json:"createdAt"`
	UserID    uuid.UUID `json:"userId"`
}
type AddMemberCommand struct {
	ProjectID      uuid.UUID
	UserID         uuid.UUID
	Role           string
	Caller         identity.Caller
	IdempotencyKey string
}
type UpdateMemberCommand struct {
	ProjectID      uuid.UUID
	UserID         uuid.UUID
	Role           string
	Caller         identity.Caller
	IdempotencyKey string
}
type RemoveMemberCommand struct {
	ProjectID      uuid.UUID
	UserID         uuid.UUID
	Caller         identity.Caller
	IdempotencyKey string
}

type Permissions struct {
	ProjectID uuid.UUID
	Role      string
	Allowed   []Permission
}

type Module struct {
	db         *sqlx.DB
	identities *identity.Module
}

func New(db *sqlx.DB, identities *identity.Module) *Module {
	return &Module{db: db, identities: identities}
}

// AuthorizeUserInTransaction 是人工接纳的第一类锁，资源排他锁前取得；最终项目授权仍另查。
func (m *Module) AuthorizeUserInTransaction(ctx context.Context, tx *sqlx.Tx, caller identity.Caller) error {
	if m.identities == nil {
		return identity.ErrUnavailable
	}
	_, err := m.identities.AuthorizeInTx(ctx, tx, caller)
	return err
}

// Read 在短 READ COMMITTED 事务里统一当前身份与读取快照；外部 K8s/网络读取不放入回调。
func (m *Module) Read(ctx context.Context, caller identity.Caller, read func(*sqlx.Tx) error) error {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: false})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.AuthorizeUserInTransaction(ctx, tx, caller); err != nil {
		return err
	}
	if err := read(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateInitialOwner 仅消费项目创建事务已证明的 User，不接受文本 Actor 或客户端身份。
func (m *Module) CreateInitialOwner(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID, userID uuid.UUID, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO project_members
 (project_id,user_id,role,created_by,created_at,updated_at) VALUES ($1,$2,'owner',$3,$4,$4)`,
		projectID, userID, userID.String(), at)
	return err
}
func (m *Module) Require(ctx context.Context, projectID uuid.UUID, caller identity.Caller, permission Permission) error {
	return m.Read(ctx, caller, func(tx *sqlx.Tx) error { return require(ctx, tx, projectID, caller.UserID(), permission) })
}

// RequireInTransaction 再读当前 Session/成员事实；重复共享锁仍是同一锁，不发生锁升级。
func (m *Module) RequireInTransaction(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID, caller identity.Caller, permission Permission) error {
	if err := m.AuthorizeUserInTransaction(ctx, tx, caller); err != nil {
		return err
	}
	return require(ctx, tx, projectID, caller.UserID(), permission)
}

// RequireAuthorizedInTransaction 仅供已经在当前事务首先完成 AuthorizeUserInTransaction 的模块使用。
func (m *Module) RequireAuthorizedInTransaction(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID, caller identity.Caller, permission Permission) error {
	return require(ctx, tx, projectID, caller.UserID(), permission)
}
func (m *Module) ListMembers(ctx context.Context, projectID uuid.UUID, caller identity.Caller,
	limit int, cursorValue string) (MemberPage, error) {
	if limit < 1 || limit > 100 || projectID == uuid.Nil {
		return MemberPage{}, ErrInvalidCursor
	}
	var cursor *memberCursor
	if cursorValue != "" {
		decoded, err := decodeMemberCursor(cursorValue, projectID)
		if err != nil {
			return MemberPage{}, err
		}
		cursor = &decoded
	}
	members := make([]Member, 0, limit+1)
	err := m.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := require(ctx, tx, projectID, caller.UserID(), PermissionRead); err != nil {
			return err
		}
		query := `SELECT pm.project_id,pm.user_id,u.display_name,pm.role,pm.created_by,pm.created_at,pm.updated_at
 FROM project_members pm JOIN users u ON u.id=pm.user_id WHERE pm.project_id=$1`
		args := []any{projectID, limit + 1}
		if cursor == nil {
			query += " ORDER BY pm.created_at DESC,pm.user_id DESC LIMIT $2"
		} else {
			query += " AND (pm.created_at,pm.user_id)<($2,$3) ORDER BY pm.created_at DESC,pm.user_id DESC LIMIT $4"
			args = []any{projectID, cursor.CreatedAt, cursor.UserID, limit + 1}
		}
		return tx.SelectContext(ctx, &members, query, args...)
	})
	if err != nil {
		return MemberPage{}, err
	}
	hasMore := len(members) > limit
	if hasMore {
		members = members[:limit]
	}
	page := MemberPage{Items: members}
	if hasMore {
		last := members[len(members)-1]
		value := encodeMemberCursor(memberCursor{Scope: "project-members", ProjectID: projectID,
			CreatedAt: last.CreatedAt, UserID: last.UserID})
		page.NextCursor = &value
	}
	return page, nil
}

// ResolveMemberCandidate 只在项目成员管理授权后精确查找有效正式用户；邮箱歧义时不泄露候选列表。
func (m *Module) ResolveMemberCandidate(ctx context.Context, projectID uuid.UUID, caller identity.Caller, kind, rawValue string) (CandidateResult, error) {
	value := strings.TrimSpace(rawValue)
	if projectID == uuid.Nil || value == "" || len(value) > 320 {
		return CandidateResult{}, ErrInvalidMember
	}
	var query string
	var arg any = value
	switch kind {
	case "login_name":
		value = strings.ToLower(value)
		if len(value) < 3 || len(value) > 128 {
			return CandidateResult{}, ErrInvalidMember
		}
		arg = value
		query = `SELECT u.id AS user_id,u.display_name FROM effective_identity_users e
 JOIN users u ON u.id=e.id JOIN local_credentials lc ON lc.user_id=u.id
 WHERE lc.login_name=$1 LIMIT 2`
	case "verified_email":
		if !strings.Contains(value, "@") || strings.ContainsAny(value, " \t\n\r") {
			return CandidateResult{}, ErrInvalidMember
		}
		query = `SELECT DISTINCT u.id AS user_id,u.display_name FROM effective_identity_users e
 JOIN users u ON u.id=e.id JOIN external_identities ei ON ei.user_id=u.id
 WHERE ei.status='linked' AND ei.email_verified AND lower(ei.email)=lower($1) LIMIT 2`
	case "user_id":
		id, err := uuid.Parse(value)
		if err != nil {
			return CandidateResult{}, ErrInvalidMember
		}
		arg = id
		query = `SELECT u.id AS user_id,u.display_name FROM effective_identity_users e
 JOIN users u ON u.id=e.id WHERE u.id=$1 LIMIT 2`
	default:
		return CandidateResult{}, ErrInvalidMember
	}
	var matches []MemberCandidate
	err := m.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := require(ctx, tx, projectID, caller.UserID(), PermissionManageMembers); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &matches, query, arg)
	})
	if err != nil {
		return CandidateResult{}, err
	}
	if len(matches) == 0 {
		return CandidateResult{Status: "not_found"}, nil
	}
	if len(matches) > 1 {
		return CandidateResult{Status: "ambiguous"}, nil
	}
	return CandidateResult{Status: "found", Candidate: &matches[0]}, nil
}

func encodeMemberCursor(cursor memberCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeMemberCursor(value string, projectID uuid.UUID) (memberCursor, error) {
	if len(value) > 512 {
		return memberCursor{}, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return memberCursor{}, ErrInvalidCursor
	}
	var cursor memberCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Scope != "project-members" || cursor.ProjectID != projectID ||
		cursor.CreatedAt.IsZero() || cursor.UserID == uuid.Nil {
		return memberCursor{}, ErrInvalidCursor
	}
	return cursor, nil
}

// GetPermissions 返回有限能力集合，客户端可以据此呈现操作，但服务端仍逐请求重新授权。
func (m *Module) GetPermissions(ctx context.Context, projectID uuid.UUID, caller identity.Caller) (Permissions, error) {
	result := Permissions{ProjectID: projectID, Allowed: make([]Permission, 0, 8)}
	err := m.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &result.Role, `SELECT pm.role FROM project_members pm JOIN projects p ON p.id=pm.project_id
			WHERE pm.project_id=$1 AND pm.user_id=$2 AND p.identity_state='governed'`, projectID, caller.UserID()); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				recordDenial(ctx, "not_member")
				return ErrNotMember
			}
			return err
		}
		for _, permission := range []Permission{PermissionRead, PermissionReadLogs, PermissionDevelop,
			PermissionManageMembers, PermissionManageOwners, PermissionResolveUnknown,
			PermissionManageAccessHosts, PermissionManageAccessRoutes} {
			if roleAllows(result.Role, permission) {
				result.Allowed = append(result.Allowed, permission)
			}
		}
		return nil
	})
	return result, err
}

func (m *Module) AddMember(ctx context.Context, c AddMemberCommand) (Member, error) {
	return m.changeMember(ctx, c.Caller, c.ProjectID, c.UserID, c.Role, c.IdempotencyKey, "add")
}
func (m *Module) UpdateMember(ctx context.Context, c UpdateMemberCommand) (Member, error) {
	return m.changeMember(ctx, c.Caller, c.ProjectID, c.UserID, c.Role, c.IdempotencyKey, "update")
}
func (m *Module) RemoveMember(ctx context.Context, c RemoveMemberCommand) (Member, error) {
	return m.changeMember(ctx, c.Caller, c.ProjectID, c.UserID, "", c.IdempotencyKey, "remove")
}

// changeMember 直接排他控制锁→项目锁，不从人工接纳的共享锁升级；授权始终在幂等前。
// admin 管非 owner，原角色或新角色为 owner 都必须额外通过 manage_owners。
func (m *Module) changeMember(ctx context.Context, caller identity.Caller, projectID, userID uuid.UUID, role, key, action string) (Member, error) {
	if userID == uuid.Nil || projectID == uuid.Nil {
		return Member{}, ErrInvalidMember
	}
	if action != "remove" {
		if err := validateRole(role); err != nil {
			return Member{}, err
		}
	}
	if m.identities == nil {
		return Member{}, identity.ErrUnavailable
	}
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Member{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := m.identities.AuthorizeSecurityInTx(ctx, tx, caller); err != nil {
		return Member{}, err
	}
	if err := require(ctx, tx, projectID, caller.UserID(), PermissionManageMembers); err != nil {
		return Member{}, err
	}
	var id uuid.UUID
	if err := tx.GetContext(ctx, &id, `SELECT id FROM projects WHERE id=$1 AND identity_state='governed' FOR UPDATE`, projectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Member{}, ErrNotMember
		}
		return Member{}, err
	}
	var current Member
	err = tx.GetContext(ctx, &current, memberSelect+" WHERE project_id=$1 AND user_id=$2", projectID, userID)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Member{}, err
	}
	if role == RoleOwner || exists && current.Role == RoleOwner {
		if err := require(ctx, tx, projectID, caller.UserID(), PermissionManageOwners); err != nil {
			return Member{}, err
		}
	}
	// 新授予/角色改变只能指向有效正式 User；删除保留处理已停用成员的能力。
	if action != "remove" {
		var valid bool
		if err := tx.GetContext(ctx, &valid, "SELECT EXISTS (SELECT 1 FROM effective_identity_users WHERE id=$1)", userID); err != nil {
			return Member{}, err
		}
		if !valid {
			return Member{}, ErrInvalidMember
		}
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID `json:"projectId"`
		UserID    uuid.UUID `json:"userId"`
		Role      string    `json:"role,omitempty"`
	}{projectID, userID, role})
	if err != nil {
		return Member{}, err
	}
	var now time.Time
	if err := tx.GetContext(ctx, &now, "SELECT clock_timestamp()"); err != nil {
		return Member{}, err
	}
	scope := idempotency.Scope{ActorID: caller.ActorID(), CommandType: "project_member." + action, Key: key}
	_, fresh, err := idempotency.Claim(ctx, tx, scope, hash, projectID, now)
	if err != nil {
		return Member{}, err
	}
	if !fresh {
		var replay Member
		if err := idempotency.LoadResponse(ctx, tx, scope, &replay); err != nil {
			return Member{}, err
		}
		if replay.Role == RoleOwner {
			if err := require(ctx, tx, projectID, caller.UserID(), PermissionManageOwners); err != nil {
				return Member{}, err
			}
		}
		return replay, tx.Commit()
	}
	if action == "add" && exists {
		return Member{}, ErrMemberExists
	}
	if action != "add" && !exists {
		return Member{}, ErrMemberNotFound
	}
	// 仅有效 owner 的实际减少需要保护；已经冻结/失效的缺失状态不制造额外全局前置条件。
	if exists && current.Role == RoleOwner && (action == "remove" || role != RoleOwner) {
		var safe bool
		if err := tx.GetContext(ctx, &safe, `SELECT NOT EXISTS(SELECT 1 FROM effective_identity_users WHERE id=$2)
   OR EXISTS(SELECT 1 FROM project_members pm JOIN effective_identity_users e ON e.id=pm.user_id
    WHERE pm.project_id=$1 AND pm.role='owner' AND pm.user_id<>$2)`, projectID, userID); err != nil {
			return Member{}, err
		}
		if !safe {
			return Member{}, ErrLastOwner
		}
	}
	result := current
	switch action {
	case "add":
		result = Member{ProjectID: projectID, UserID: userID, Role: role, CreatedBy: caller.ActorID(), CreatedAt: now, UpdatedAt: now}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_members
   (project_id,user_id,role,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, projectID, userID, role, caller.ActorID(), now)
	case "update":
		result.Role, result.UpdatedAt = role, now
		_, err = tx.ExecContext(ctx, "UPDATE project_members SET role=$3,updated_at=$4 WHERE project_id=$1 AND user_id=$2", projectID, userID, role, now)
	case "remove":
		_, err = tx.ExecContext(ctx, "DELETE FROM project_members WHERE project_id=$1 AND user_id=$2", projectID, userID)
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return Member{}, ErrMemberExists
		}
		return Member{}, fmt.Errorf("change project member: %w", err)
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, result); err != nil {
		return Member{}, err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: caller.ActorID(), Action: scope.CommandType, TargetType: "project", TargetID: projectID,
		Summary: map[string]string{"memberUserId": userID.String(), "role": result.Role}, CreatedAt: now}); err != nil {
		return Member{}, err
	}
	return result, tx.Commit()
}

func require(ctx context.Context, queryer sqlx.QueryerContext, projectID, userID uuid.UUID, permission Permission) error {
	var role string
	if err := sqlx.GetContext(ctx, queryer, &role, `SELECT pm.role FROM project_members pm JOIN projects p ON p.id=pm.project_id
  WHERE pm.project_id=$1 AND pm.user_id=$2 AND p.identity_state='governed'`, projectID, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			recordDenial(ctx, "not_member")
			return ErrNotMember
		}
		return err
	}
	if !roleAllows(role, permission) {
		recordDenial(ctx, "forbidden")
		return ErrForbidden
	}
	return nil
}
func roleAllows(role string, p Permission) bool {
	switch p {
	case PermissionRead:
		return role == RoleOwner || role == RoleAdmin || role == RoleDeveloper || role == RoleViewer
	case PermissionReadLogs, PermissionDevelop, PermissionManageAccessRoutes:
		return role == RoleOwner || role == RoleAdmin || role == RoleDeveloper
	case PermissionManageMembers, PermissionResolveUnknown, PermissionManageAccessHosts:
		return role == RoleOwner || role == RoleAdmin
	case PermissionManageOwners:
		return role == RoleOwner
	default:
		return false
	}
}
func validateRole(role string) error {
	switch role {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return nil
	default:
		return ErrInvalidRole
	}
}

const memberSelect = "SELECT project_id,user_id,role,created_by,created_at,updated_at FROM project_members"
