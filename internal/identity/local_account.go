package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

// CreateLocalUserCommand 是管理员准入，不接受平台角色或项目角色。
// 临时密码不进观测、幂等 Fingerprint 或响应缓存；管理员不替用户签发 Session。
type CreateLocalUserCommand struct {
	Caller            Caller
	LoginName         string
	DisplayName       string
	TemporaryPassword string `json:"-"`
}

func (CreateLocalUserCommand) String() string   { return "CreateLocalUserCommand{redacted}" }
func (CreateLocalUserCommand) GoString() string { return "CreateLocalUserCommand{redacted}" }

// CreateLocalUser 只准入普通 User。凭据、责任与成功审计原子提交，不创建默认项目。
func (m *Module) CreateLocalUser(ctx context.Context, command CreateLocalUserCommand) (User, error) {
	loginName, err := normalizeLoginName(command.LoginName)
	if err != nil || !validLabel(command.DisplayName, 128) {
		return User{}, ErrInvalidCommand
	}
	// 昂贵 Hash 在控制锁外完成；事务内重新核验当前管理员及近期主认证。
	hash, err := m.hashPassword(ctx, command.TemporaryPassword)
	if err != nil {
		return User{}, err
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	admin, err := requireAdministrator(ctx, tx, command.Caller, true)
	if err != nil {
		return User{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return User{}, err
	}
	user := User{ID: uuid.New(), DisplayName: command.DisplayName, Status: "active",
		PlatformRole: RoleUser, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(id, display_name, status, platform_role, admitted_by, admitted_at, created_at, updated_at)
		VALUES ($1, $2, 'active', 'user', $3, $4, $4, $4)`,
		user.ID, user.DisplayName, admin.ID.String(), now); err != nil {
		return User{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_credentials
		(user_id, login_name, password_hash, must_change_password, updated_at)
		VALUES ($1, $2, $3, true, $4)`, user.ID, loginName, hash, now); err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrLoginNameConflict
		}
		return User{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: admin.ID.String(), Action: "user.create",
		TargetType: "user", TargetID: user.ID,
		Summary: map[string]any{"method": "password", "mustChangePassword": true}, CreatedAt: now}); err != nil {
		return User{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	return user, nil
}

// GetUser 是平台管理员的最小安全投影；本人路由使用 CurrentUser 而非目录权限。
// 只读不强制近期认证，但仍拒绝临时密码与已撤销会话，返回快照期间持共享控制锁。
func (m *Module) GetUser(ctx context.Context, caller Caller, userID uuid.UUID) (User, error) {
	if userID == uuid.Nil {
		return User{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := requireAdministrator(ctx, tx, caller, false); err != nil {
		return User{}, err
	}
	var user User
	if err := tx.GetContext(ctx, &user, `SELECT id, display_name, status, platform_role, created_at
		FROM users WHERE id = $1`, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	return user, nil
}

// requireAdministrator 在调用者已取得控制锁后读最新身份，不相信 Caller 缓存角色。
func requireAdministrator(ctx context.Context, tx *sqlx.Tx, caller Caller, recent bool) (currentRecord, error) {
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return currentRecord{}, err
	}
	if current.MustChangePassword {
		return currentRecord{}, ErrPasswordChangeRequired
	}
	if current.PlatformRole != RolePlatformAdmin {
		return currentRecord{}, ErrForbidden
	}
	if recent {
		if err := requireRecentAuthentication(ctx, tx, current); err != nil {
			return currentRecord{}, err
		}
	}
	return current, nil
}

// requireRecentAuthentication 使用已记录的主认证与数据库时钟，不用 Cookie 活动时间替代。
func requireRecentAuthentication(ctx context.Context, tx *sqlx.Tx, current currentRecord) error {
	if !current.PrimaryAuthenticatedAt.Valid {
		return ErrRecentAuthenticationRequired
	}
	var recent bool
	if err := tx.GetContext(ctx, &recent, `SELECT $1::timestamptz <= clock_timestamp()
		AND $1::timestamptz > clock_timestamp() - interval '5 minutes'`, current.PrimaryAuthenticatedAt.Time); err != nil {
		return dependencyError(err)
	}
	if !recent {
		return ErrRecentAuthenticationRequired
	}
	return nil
}

func databaseNow(ctx context.Context, tx *sqlx.Tx) (time.Time, error) {
	var now time.Time
	if err := tx.GetContext(ctx, &now, `SELECT clock_timestamp()`); err != nil {
		return time.Time{}, dependencyError(err)
	}
	return now, nil
}

func isUniqueViolation(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505"
}
