package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	sessionAbsoluteLifetime = 8 * time.Hour
	sessionIdleLifetime     = 30 * time.Minute
)

// Caller 只能由 Module 根据当前 Session 建立；零值不具认证能力。
// 此证据不缓存最终角色，业务接纳时仍通过 AuthorizeInTx 读取当前事实。
type Caller struct {
	userID             uuid.UUID
	externalIdentityID uuid.UUID
	sessionID          uuid.UUID
	authVersion        int64
}

type callerContextKey struct{}

// WithCaller 只传播由正常 Session 解析得到的证据；Caller 的私有字段不能由协议输入构造。
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

func CallerFromContext(ctx context.Context) Caller {
	caller, _ := ctx.Value(callerContextKey{}).(Caller)
	return caller
}

func (c Caller) UserID() uuid.UUID             { return c.userID }
func (c Caller) ExternalIdentityID() uuid.UUID { return c.externalIdentityID }
func (c Caller) ActorID() string {
	if c.userID == uuid.Nil {
		return ""
	}
	return c.userID.String()
}

// SessionToken 仅允许 HTTP Cookie 写入者显式取值，普通格式化不能泄露。
type SessionToken struct{ value string }

func (t SessionToken) CookieValue() string { return t.value }
func (SessionToken) String() string        { return "SessionToken{redacted}" }
func (SessionToken) GoString() string      { return "SessionToken{redacted}" }

type CurrentUser struct {
	User               User `json:"user"`
	MustChangePassword bool `json:"mustChangePassword"`
}

type LoginResult struct {
	CurrentUser CurrentUser  `json:"currentUser"`
	Token       SessionToken `json:"-"`
	// ExpiresAt 只供 Cookie 写入者同步服务端期限，不是客户端授权依据。
	ExpiresAt time.Time `json:"-"`
}

// LocalLoginCommand 的密码既不进入 JSON，也不得参与幂等 Fingerprint。
type LocalLoginCommand struct {
	LoginName string
	Password  string `json:"-"`
	SourceIP  string
}

func (LocalLoginCommand) String() string   { return "LocalLoginCommand{redacted}" }
func (LocalLoginCommand) GoString() string { return "LocalLoginCommand{redacted}" }

type credentialRecord struct {
	UserID      uuid.UUID `db:"user_id"`
	Hash        string    `db:"password_hash"`
	AuthVersion int64     `db:"auth_version"`
	Status      string    `db:"status"`
}

// LoginLocal 先在事务外验证密码，再取得共享安全锁重读 Hash/版本/状态。
// 重置已提交时，旧验证结果不能签发新 Session；登录不是可缓存凭据的业务幂等命令。
func (m *Module) LoginLocal(ctx context.Context, command LocalLoginCommand) (LoginResult, error) {
	if net.ParseIP(command.SourceIP) == nil {
		return LoginResult{}, ErrInvalidCommand
	}
	if len(command.Password) > 512 || !utf8.ValidString(command.Password) {
		return LoginResult{}, ErrUnauthenticated
	}
	loginName, nameErr := normalizeLoginName(command.LoginName)
	if err := m.limitLocalLogin(ctx, loginName, net.ParseIP(command.SourceIP).String()); err != nil {
		if errors.Is(err, ErrRateLimited) {
			return LoginResult{}, m.rejectLocalLogin(ctx, err)
		}
		return LoginResult{}, err
	}
	record := credentialRecord{Hash: m.dummyHash}
	if nameErr == nil {
		err := m.db.GetContext(ctx, &record, `SELECT lc.user_id, lc.password_hash, u.auth_version, u.status
			FROM local_credentials lc JOIN users u ON u.id = lc.user_id WHERE lc.login_name = $1`, loginName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return LoginResult{}, dependencyError(err)
		}
	}
	verified, err := m.verifyPassword(ctx, command.Password, record.Hash)
	if err != nil {
		return LoginResult{}, err
	}
	if !verified || record.UserID == uuid.Nil || record.Status != "active" {
		return LoginResult{}, m.rejectLocalLogin(ctx, ErrUnauthenticated)
	}
	token, err := randomSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return LoginResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current currentRecord
	if err := tx.GetContext(ctx, &current, `SELECT u.id, u.display_name, u.status, u.platform_role,
		u.created_at, lc.must_change_password FROM users u JOIN local_credentials lc ON lc.user_id = u.id
		WHERE u.id = $1 AND u.auth_version = $2 AND u.status = 'active' AND lc.password_hash = $3`,
		record.UserID, record.AuthVersion, record.Hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 失败审计使用独立短事务，先释放共享锁/连接，避免连接池自等待。
			_ = tx.Rollback()
			return LoginResult{}, m.rejectLocalLogin(ctx, ErrUnauthenticated)
		}
		return LoginResult{}, authenticationQueryError(err)
	}
	var now time.Time
	if err := tx.GetContext(ctx, &now, `SELECT clock_timestamp()`); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	hash := sha256.Sum256([]byte(token.CookieValue()))
	sessionID := uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions
		(id, token_hash, user_id, auth_version, auth_method, primary_authenticated_at,
		 created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4, 'password', $5, $5, $5, $6)`,
		sessionID, hash[:], current.ID, record.AuthVersion, now, now.Add(sessionAbsoluteLifetime)); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(),
		Action: "auth.login", TargetType: "session", TargetID: sessionID,
		Summary: map[string]any{"method": "password"}, CreatedAt: now}); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	return LoginResult{CurrentUser: current.public(), Token: token, ExpiresAt: now.Add(sessionAbsoluteLifetime)}, nil
}

type currentRecord struct {
	User
	MustChangePassword     bool         `db:"must_change_password"`
	SessionID              uuid.UUID    `db:"session_id"`
	AuthVersion            int64        `db:"auth_version"`
	PrimaryAuthenticatedAt sql.NullTime `db:"primary_authenticated_at"`
	ExpiresAt              time.Time    `db:"expires_at"`
}

func (r currentRecord) public() CurrentUser {
	return CurrentUser{User: r.User, MustChangePassword: r.MustChangePassword}
}

const currentSessionQuery = `SELECT u.id, u.display_name, u.status, u.platform_role, u.created_at,
	lc.must_change_password, s.id AS session_id, u.auth_version, s.primary_authenticated_at, s.expires_at` + currentSessionFacts

// 凭据证明读取与最终 Caller 校验共享会话有效性条件，但证明读取不持锁等 Hash。
const currentSessionFacts = ` FROM auth_sessions s JOIN users u ON u.id = s.user_id
	JOIN local_credentials lc ON lc.user_id = u.id
	WHERE u.status = 'active' AND s.revoked_at IS NULL AND s.auth_version = u.auth_version
	AND s.expires_at > clock_timestamp() AND s.last_seen_at > clock_timestamp() - interval '30 minutes'`

// ResolveSession 是 Cookie 到 Caller 的认证 Seam，不返回 Hash 或提供方凭据。
func (m *Module) ResolveSession(ctx context.Context, cookie string) (Caller, error) {
	hash, valid := hashSessionCookie(cookie)
	if !valid {
		return Caller{}, ErrUnauthenticated
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return Caller{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var session struct {
		ID                 uuid.UUID     `db:"id"`
		Method             string        `db:"auth_method"`
		UserID             uuid.NullUUID `db:"user_id"`
		ExternalIdentityID uuid.NullUUID `db:"external_identity_id"`
	}
	if err := tx.GetContext(ctx, &session, `SELECT id,auth_method,user_id,external_identity_id FROM auth_sessions
		WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>clock_timestamp()
		AND last_seen_at>clock_timestamp()-interval '30 minutes'`, hash); err != nil {
		return Caller{}, authenticationQueryError(err)
	}
	caller := Caller{sessionID: session.ID}
	if session.Method == "password" && session.UserID.Valid {
		var current currentRecord
		if err := tx.GetContext(ctx, &current, currentSessionQuery+` AND s.id=$1`, session.ID); err != nil {
			return Caller{}, authenticationQueryError(err)
		}
		caller.userID, caller.authVersion = current.ID, current.AuthVersion
	} else if session.Method == "oidc" && session.ExternalIdentityID.Valid {
		var linked struct {
			ExternalIdentityID uuid.UUID     `db:"external_identity_id"`
			Status             string        `db:"identity_status"`
			UserID             uuid.NullUUID `db:"user_id"`
			AuthVersion        sql.NullInt64 `db:"auth_version"`
		}
		if err := tx.GetContext(ctx, &linked, `SELECT ei.id external_identity_id,ei.status identity_status,ei.user_id,u.auth_version
			FROM auth_sessions s JOIN external_identities ei ON ei.id=s.external_identity_id
			JOIN auth_providers ap ON ap.id=ei.provider_id
			LEFT JOIN users u ON u.id=ei.user_id
			WHERE s.id=$1 AND ap.enabled AND (ei.status='pending' OR (ei.status='linked' AND u.status='active'))`, session.ID); err != nil {
			return Caller{}, authenticationQueryError(err)
		}
		caller.externalIdentityID = linked.ExternalIdentityID
		if linked.Status == "linked" && linked.UserID.Valid && linked.AuthVersion.Valid {
			caller.userID, caller.authVersion = linked.UserID.UUID, linked.AuthVersion.Int64
		}
	} else {
		return Caller{}, ErrUnauthenticated
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at = clock_timestamp() WHERE id = $1`, session.ID); err != nil {
		return Caller{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return Caller{}, dependencyError(err)
	}
	return caller, nil
}

// AuthorizeInTx 是人工业务接纳的用户级门禁，不替代项目权限校验。
// 调用者须用 READ COMMITTED，且在任何资源排他锁前调用；方法只取得共享控制锁。
func (m *Module) AuthorizeInTx(ctx context.Context, tx *sqlx.Tx, caller Caller) (User, error) {
	return m.authorizeInTx(ctx, tx, caller, false)
}

// AuthorizeSecurityInTx 由成员安全变更消费，直接取得排他控制锁，不可先走共享门禁。
func (m *Module) AuthorizeSecurityInTx(ctx context.Context, tx *sqlx.Tx, caller Caller) (User, error) {
	return m.authorizeInTx(ctx, tx, caller, true)
}

func (m *Module) authorizeInTx(ctx context.Context, tx *sqlx.Tx, caller Caller, exclusive bool) (User, error) {
	if caller.userID == uuid.Nil || caller.sessionID == uuid.Nil || caller.authVersion <= 0 {
		return User{}, ErrUnauthenticated
	}
	if tx == nil {
		return User{}, ErrUnavailable
	}
	if _, err := lockControl(ctx, tx, exclusive); err != nil {
		return User{}, err
	}
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return User{}, err
	}
	if current.MustChangePassword {
		return User{}, ErrPasswordChangeRequired
	}
	return current.User, nil
}

// CurrentUser 为受限会话也提供本人最小信息，每次仍读当前 Session 与账号事实。
func (m *Module) CurrentUser(ctx context.Context, caller Caller) (CurrentUser, error) {
	if caller.userID == uuid.Nil || caller.sessionID == uuid.Nil || caller.authVersion <= 0 {
		return CurrentUser{}, ErrUnauthenticated
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return CurrentUser{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return CurrentUser{}, err
	}
	if err := tx.Commit(); err != nil {
		return CurrentUser{}, dependencyError(err)
	}
	return current.public(), nil
}

func loadCaller(ctx context.Context, queryer sqlx.QueryerContext, caller Caller) (currentRecord, error) {
	var current currentRecord
	var err error
	if caller.externalIdentityID != uuid.Nil {
		err = sqlx.GetContext(ctx, queryer, &current, `SELECT u.id,u.display_name,u.status,u.platform_role,u.created_at,
			COALESCE(lc.must_change_password,false) must_change_password,s.id session_id,u.auth_version,
			s.primary_authenticated_at,s.expires_at
			FROM auth_sessions s JOIN external_identities ei ON ei.id=s.external_identity_id
			JOIN auth_providers ap ON ap.id=ei.provider_id JOIN users u ON u.id=ei.user_id
			LEFT JOIN local_credentials lc ON lc.user_id=u.id
			WHERE s.id=$1 AND ei.id=$2 AND u.id=$3 AND u.auth_version=$4 AND ei.status='linked'
			AND ap.enabled AND u.status='active' AND s.revoked_at IS NULL
			AND s.expires_at>clock_timestamp() AND s.last_seen_at>clock_timestamp()-interval '30 minutes'`,
			caller.sessionID, caller.externalIdentityID, caller.userID, caller.authVersion)
	} else {
		err = sqlx.GetContext(ctx, queryer, &current, currentSessionQuery+` AND s.id = $1 AND u.id = $2 AND u.auth_version = $3`,
			caller.sessionID, caller.userID, caller.authVersion)
	}
	return current, authenticationQueryError(err)
}

// LogoutCurrent 缺失/无效/已撤销 Cookie 自然成功，但不授权任何业务。
func (m *Module) LogoutCurrent(ctx context.Context, cookie string) error {
	hash, valid := hashSessionCookie(cookie)
	if !valid {
		return nil
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var revoked struct {
		ID                 uuid.UUID     `db:"id"`
		UserID             uuid.NullUUID `db:"user_id"`
		ExternalIdentityID uuid.NullUUID `db:"external_identity_id"`
		At                 time.Time     `db:"revoked_at"`
	}
	err = tx.GetContext(ctx, &revoked, `UPDATE auth_sessions SET revoked_at = clock_timestamp()
		WHERE token_hash = $1 AND revoked_at IS NULL RETURNING id,user_id,external_identity_id,revoked_at`, hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return dependencyError(err)
	}
	if err == nil {
		actorID := "external:" + revoked.ExternalIdentityID.UUID.String()
		if revoked.UserID.Valid {
			actorID = revoked.UserID.UUID.String()
		}
		if err := audit.Append(ctx, tx, audit.Entry{ActorID: actorID,
			Action: "auth.logout", TargetType: "session", TargetID: revoked.ID,
			Summary: map[string]any{}, CreatedAt: revoked.At}); err != nil {
			return dependencyError(err)
		}
	}
	return dependencyErrorOrNil(tx.Commit())
}

// LogoutAll 要求当前本人的有效证明，受临时密码限制的 User 仍可退出。
// 验证与撤销共持排他控制锁，不能复用 Middleware 或旧 Caller 绕过撤销。
func (m *Module) LogoutAll(ctx context.Context, caller Caller) error {
	if caller.sessionID == uuid.Nil || caller.userID == uuid.Nil && caller.externalIdentityID == uuid.Nil {
		return ErrUnauthenticated
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	actorID := caller.ActorID()
	targetID := caller.userID
	where := `user_id=$2 OR external_identity_id IN (SELECT id FROM external_identities WHERE user_id=$2)`
	argument := any(caller.userID)
	if caller.userID != uuid.Nil {
		if _, err := loadCaller(ctx, tx, caller); err != nil {
			return err
		}
	} else {
		var pending bool
		if err := tx.GetContext(ctx, &pending, `SELECT EXISTS(SELECT 1 FROM auth_sessions s JOIN external_identities ei ON ei.id=s.external_identity_id
			WHERE s.id=$1 AND ei.id=$2 AND ei.status='pending' AND s.revoked_at IS NULL
			AND s.expires_at>clock_timestamp() AND s.last_seen_at>clock_timestamp()-interval '30 minutes')`, caller.sessionID, caller.externalIdentityID); err != nil {
			return dependencyError(err)
		}
		if !pending {
			return ErrUnauthenticated
		}
		actorID = "external:" + caller.externalIdentityID.String()
		targetID = caller.externalIdentityID
		where, argument = `external_identity_id=$2`, caller.externalIdentityID
	}
	var now time.Time
	if err := tx.GetContext(ctx, &now, `SELECT clock_timestamp()`); err != nil {
		return dependencyError(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at=$1 WHERE (`+where+`) AND revoked_at IS NULL`, now, argument)
	if err != nil {
		return dependencyError(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: actorID,
		Action: "auth.logout_all", TargetType: "identity", TargetID: targetID,
		Summary: map[string]any{"revoked_count": count}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}

func authenticationQueryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthenticated
	}
	return dependencyError(err)
}

func dependencyErrorOrNil(err error) error {
	if err == nil {
		return nil
	}
	return dependencyError(err)
}

func randomSessionToken() (SessionToken, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return SessionToken{}, ErrUnavailable
	}
	defer clear(value)
	return SessionToken{value: base64.RawURLEncoding.EncodeToString(value)}, nil
}

func hashSessionCookie(cookie string) ([]byte, bool) {
	if len(cookie) != 43 {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cookie)
	if err != nil || len(decoded) != 32 {
		return nil, false
	}
	defer clear(decoded)
	hash := sha256.Sum256([]byte(cookie))
	return hash[:], true
}
