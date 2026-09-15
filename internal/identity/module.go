// Package identity 拥有内部用户、凭据和可信会话，业务只接收已验证 Caller。
// 身份校验与撤销通过短事务控制锁排序；密码和外部网络不在锁内运行。
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrUnavailable                  = errors.New("identity dependency unavailable")
	ErrInvalidCommand               = errors.New("invalid identity command")
	ErrInvalidPassword              = errors.New("password does not meet policy")
	ErrAlreadyInitialized           = errors.New("identity already initialized")
	ErrTransactionMode              = errors.New("identity requires read committed transaction")
	ErrUnauthenticated              = errors.New("authentication required")
	ErrPasswordChangeRequired       = errors.New("password change required")
	ErrRateLimited                  = errors.New("authentication rate limited")
	ErrForbidden                    = errors.New("identity permission denied")
	ErrRecentAuthenticationRequired = errors.New("recent authentication required")
	ErrLoginNameConflict            = errors.New("login name already in use")
	ErrUserNotFound                 = errors.New("user not found")
)

const (
	RolePlatformAdmin = "platform_admin"
	RoleUser          = "user"
	maintenanceActor  = "orbit-devops-identity-maintenance"
)

// User 只含可输出的用户事实，密码 Hash 和认证版本不进入此投影。
type User struct {
	ID           uuid.UUID `db:"id" json:"id"`
	DisplayName  string    `db:"display_name" json:"displayName"`
	Status       string    `db:"status" json:"status"`
	PlatformRole string    `db:"platform_role" json:"platformRole"`
	CreatedAt    time.Time `db:"created_at" json:"createdAt"`
}

type Module struct {
	db        *sqlx.DB
	hashSlots chan struct{}
	dummyHash string
}

// New 不写数据库、不自动 bootstrap，也不创建默认共享账号。
func New(db *sqlx.DB) (*Module, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	m := &Module{db: db, hashSlots: make(chan struct{}, 2)}
	secret, err := randomSessionToken()
	if err != nil {
		return nil, err
	}
	m.dummyHash, err = m.hashPassword(context.Background(), secret.CookieValue())
	if err != nil {
		return nil, ErrUnavailable
	}
	return m, nil
}

// InitializeAdminCommand 含明文密码，禁止日志、Trace、幂等 Fingerprint/响应缓存。
type InitializeAdminCommand struct {
	LoginName      string
	DisplayName    string
	Password       string `json:"-"`
	MaintenanceRef string
}

// InitializeAdmin 仅在永久未初始化时创建首个本地管理员；不创建项目或会话。
// 重复/并发调用只允许一次提交；受信任恢复必须使用独立命令，不能复用这里。
func (m *Module) InitializeAdmin(ctx context.Context, command InitializeAdminCommand) (User, error) {
	loginName, err := normalizeLoginName(command.LoginName)
	if err != nil || !validLabel(command.DisplayName, 128) || !validLabel(command.MaintenanceRef, 256) {
		return User{}, ErrInvalidCommand
	}
	hash, err := m.hashPassword(ctx, command.Password)
	if err != nil {
		return User{}, err
	}
	tx, marker, err := m.beginControlled(ctx, true)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var hasUsers bool
	if err := tx.GetContext(ctx, &hasUsers, `SELECT EXISTS (SELECT 1 FROM users)`); err != nil {
		return User{}, dependencyError(err)
	}
	if marker.Valid || hasUsers {
		return User{}, ErrAlreadyInitialized
	}
	var now time.Time
	if err := tx.GetContext(ctx, &now, `SELECT clock_timestamp()`); err != nil {
		return User{}, dependencyError(err)
	}
	user := User{ID: uuid.New(), DisplayName: command.DisplayName, Status: "active",
		PlatformRole: RolePlatformAdmin, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(id, display_name, status, platform_role, admitted_by, admitted_at, created_at, updated_at)
		VALUES ($1, $2, 'active', 'platform_admin', $3, $4, $4, $4)`,
		user.ID, user.DisplayName, maintenanceActor, now); err != nil {
		return User{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_credentials
		(user_id, login_name, password_hash, must_change_password, updated_at)
		VALUES ($1, $2, $3, false, $4)`, user.ID, loginName, hash, now); err != nil {
		return User{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identity_control SET initialized_at = $1 WHERE id = 1`, now); err != nil {
		return User{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID: maintenanceActor, ActorKind: audit.ActorKindSystem,
		Action: "identity.initialize_admin", TargetType: "user", TargetID: user.ID,
		Summary: map[string]any{"maintenance_ref": command.MaintenanceRef}, CreatedAt: now,
	}); err != nil {
		return User{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	return user, nil
}

// beginControlled 取得锁之后才允许以新 SQL 查询权限，不复用等待前的快照。
func (m *Module) beginControlled(ctx context.Context, exclusive bool) (*sqlx.Tx, sql.NullTime, error) {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, sql.NullTime{}, dependencyError(err)
	}
	marker, err := lockControl(ctx, tx, exclusive)
	if err != nil {
		_ = tx.Rollback()
		return nil, sql.NullTime{}, err
	}
	return tx, marker, nil
}

// lockControl 必须是调用者事务的第一类锁，不可共享→排他升级。
func lockControl(ctx context.Context, tx *sqlx.Tx, exclusive bool) (sql.NullTime, error) {
	var isolation string
	if err := tx.GetContext(ctx, &isolation, `SHOW transaction_isolation`); err != nil {
		return sql.NullTime{}, dependencyError(err)
	}
	if isolation != "read committed" {
		return sql.NullTime{}, ErrTransactionMode
	}
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', '5s', true)`); err != nil {
		return sql.NullTime{}, dependencyError(err)
	}
	query := `SELECT initialized_at FROM identity_control WHERE id = 1 FOR SHARE`
	if exclusive {
		query = `SELECT initialized_at FROM identity_control WHERE id = 1 FOR UPDATE`
	}
	var marker sql.NullTime
	if err := tx.GetContext(ctx, &marker, query); err != nil {
		return sql.NullTime{}, dependencyError(err)
	}
	return marker, nil
}

// dependencyError 不传播驱动细节，避免约束 DETAIL 中的凭据 Hash/连接材料泄露。
func dependencyError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return ErrUnavailable
}

func normalizeLoginName(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 128 {
		return "", ErrInvalidCommand
	}
	for i, c := range []byte(value) {
		alphanumeric := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alphanumeric && (i == 0 || !strings.ContainsRune("._@+-", rune(c))) {
			return "", ErrInvalidCommand
		}
	}
	return value, nil
}

func validLabel(value string, maximum int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maximum {
		return false
	}
	for _, c := range value {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// String 阻止一般错误/观测输出以结构体形式泄露维护密码。
func (InitializeAdminCommand) String() string   { return "InitializeAdminCommand{redacted}" }
func (InitializeAdminCommand) GoString() string { return "InitializeAdminCommand{redacted}" }

// 保留显式 fmt.Stringer 契约，调用者记录命令时只能看到脱敏标记。
var _ fmt.Stringer = InitializeAdminCommand{}
