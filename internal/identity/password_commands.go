package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// ChangePasswordCommand 的登录名仅供首次建立本地凭据；已有凭据不得借改密改名。
// SourceIP 由传输层从受信任连接解析，不接受请求体/未经信任代理 Header。
type ChangePasswordCommand struct {
	Caller          Caller
	CurrentPassword string `json:"-"`
	NewPassword     string `json:"-"`
	LoginName       string
	SourceIP        string
}

func (ChangePasswordCommand) String() string   { return "ChangePasswordCommand{redacted}" }
func (ChangePasswordCommand) GoString() string { return "ChangePasswordCommand{redacted}" }

// ReauthenticateLocalCommand 只证明当前 User 的密码，不接受可切换账号的登录名。
type ReauthenticateLocalCommand struct {
	Caller   Caller
	Password string `json:"-"`
	SourceIP string
}

func (ReauthenticateLocalCommand) String() string   { return "ReauthenticateLocalCommand{redacted}" }
func (ReauthenticateLocalCommand) GoString() string { return "ReauthenticateLocalCommand{redacted}" }

// ResetLocalPasswordCommand 不输出管理员提供的密码，也不改变用户准入/停用状态。
type ResetLocalPasswordCommand struct {
	Caller            Caller
	UserID            uuid.UUID
	TemporaryPassword string `json:"-"`
	LoginName         string
}

func (ResetLocalPasswordCommand) String() string   { return "ResetLocalPasswordCommand{redacted}" }
func (ResetLocalPasswordCommand) GoString() string { return "ResetLocalPasswordCommand{redacted}" }

// ResetLocalPassword 必须由完整且近期认证的管理员明确执行。
// 设置临时密码与撤销同事务；不启用被停用账号、不赋项目成员或替目标签发会话。
func (m *Module) ResetLocalPassword(ctx context.Context, command ResetLocalPasswordCommand) error {
	if command.UserID == uuid.Nil || command.LoginName != "" {
		// OIDC-only 首次建立登录名随真实外部身份路径加入，不提前做绕过准入的 upsert。
		return ErrInvalidCommand
	}
	hash, err := m.hashPassword(ctx, command.TemporaryPassword)
	if err != nil {
		return err
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	admin, err := requireAdministrator(ctx, tx, command.Caller, true)
	if err != nil {
		return err
	}
	var target uuid.UUID
	if err := tx.GetContext(ctx, &target, `SELECT u.id FROM users u
		JOIN local_credentials lc ON lc.user_id = u.id WHERE u.id = $1 FOR UPDATE OF u`, command.UserID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return dependencyError(err)
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_credentials SET password_hash = $1,
		must_change_password = true, updated_at = $2 WHERE user_id = $3`, hash, now, target); err != nil {
		return dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, target, now); err != nil {
		return err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: admin.ID.String(), Action: "user.password.reset",
		TargetType: "user", TargetID: target,
		Summary: map[string]any{"method": "password", "mustChangePassword": true}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}

// ReauthenticateLocal 验证本人密码后仅旋转当前会话，不提升权限/解除临时限制。
// 保留原绝对期限，防止不断 reauth 变成隐式 remember-me；其他设备和 User 版本不变。
func (m *Module) ReauthenticateLocal(ctx context.Context, command ReauthenticateLocalCommand) (LoginResult, error) {
	proof, err := m.verifyLocalProof(ctx, command.Caller, command.Password, command.SourceIP, "auth.reauth_failed")
	if err != nil {
		return LoginResult{}, err
	}
	token, err := randomSessionToken()
	if err != nil {
		return LoginResult{}, err
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return LoginResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, command.Caller)
	if err != nil {
		return LoginResult{}, err
	}
	if err := requireUnchangedLocalProof(ctx, tx, current.ID, proof); err != nil {
		return LoginResult{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return LoginResult{}, err
	}
	if !current.ExpiresAt.After(now) {
		return LoginResult{}, ErrUnauthenticated
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = $1 WHERE id = $2`, now, current.SessionID); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	sessionID := uuid.New()
	hash := sha256.Sum256([]byte(token.CookieValue()))
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions
		(id, token_hash, user_id, auth_version, auth_method, primary_authenticated_at, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4, 'password', $5, $5, $5, $6)`,
		sessionID, hash[:], current.ID, current.AuthVersion, now, current.ExpiresAt); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(), Action: "auth.reauth",
		TargetType: "session", TargetID: sessionID, Summary: map[string]any{"method": "password"}, CreatedAt: now}); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return LoginResult{}, dependencyError(err)
	}
	return LoginResult{CurrentUser: current.public(), Token: token, ExpiresAt: current.ExpiresAt}, nil
}

type localProof struct {
	Hash      string `db:"password_hash"`
	LoginName string `db:"login_name"`
}

// ChangePassword 允许临时密码账号完成改密；只证明本人，不能修改其他 User。
// Hash 在控制锁外，最后重新核验会话及旧 Hash；成功推进版本并撤销全部设备。
func (m *Module) ChangePassword(ctx context.Context, command ChangePasswordCommand) error {
	if command.LoginName != "" {
		// OIDC-only 的首次凭据在 OIDC 切片实现；本地路径不接受登录名重命名。
		return ErrInvalidCommand
	}
	if command.NewPassword == command.CurrentPassword {
		return ErrInvalidPassword
	}
	proof, err := m.verifyLocalProof(ctx, command.Caller, command.CurrentPassword, command.SourceIP, "auth.password_change_failed")
	if err != nil {
		return err
	}
	hash, err := m.hashPassword(ctx, command.NewPassword)
	if err != nil {
		return err
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, command.Caller)
	if err != nil {
		return err
	}
	if err := requireUnchangedLocalProof(ctx, tx, current.ID, proof); err != nil {
		return err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_credentials SET password_hash = $1,
		must_change_password = false, updated_at = $2 WHERE user_id = $3`, hash, now, current.ID); err != nil {
		return dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, current.ID, now); err != nil {
		return err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(), Action: "user.password.change",
		TargetType: "user", TargetID: current.ID,
		Summary: map[string]any{"method": "password", "mustChangePassword": false}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}

// verifyLocalProof 先验证当前 Session，再验证它的 User 本地密码，不接受另一个登录名。
// 这是候选证明；事务内仍须重读 Session、版本与 Hash，不能据此直接接纳。
func (m *Module) verifyLocalProof(ctx context.Context, caller Caller, password, sourceIP, failureAction string) (localProof, error) {
	ip := net.ParseIP(sourceIP)
	if ip == nil {
		return localProof{}, ErrInvalidCommand
	}
	var proof localProof
	if err := m.db.GetContext(ctx, &proof, `SELECT lc.password_hash, lc.login_name`+currentSessionFacts+
		` AND s.id = $1 AND u.id = $2 AND u.auth_version = $3`, caller.sessionID, caller.userID, caller.authVersion); err != nil {
		return localProof{}, authenticationQueryError(err)
	}
	if err := m.limitLocalLogin(ctx, proof.LoginName, ip.String()); err != nil {
		if err == ErrRateLimited {
			return localProof{}, m.rejectPasswordAuthentication(ctx, failureAction, err)
		}
		return localProof{}, err
	}
	if len(password) > 512 || !utf8.ValidString(password) {
		return localProof{}, m.rejectPasswordAuthentication(ctx, failureAction, ErrUnauthenticated)
	}
	verified, err := m.verifyPassword(ctx, password, proof.Hash)
	if err != nil {
		return localProof{}, err
	}
	if !verified {
		return localProof{}, m.rejectPasswordAuthentication(ctx, failureAction, ErrUnauthenticated)
	}
	return proof, nil
}

func requireUnchangedLocalProof(ctx context.Context, tx *sqlx.Tx, userID uuid.UUID, proof localProof) error {
	var unchanged bool
	if err := tx.GetContext(ctx, &unchanged, `SELECT EXISTS (SELECT 1 FROM local_credentials
		WHERE user_id = $1 AND password_hash = $2 AND login_name = $3)`, userID, proof.Hash, proof.LoginName); err != nil {
		return dependencyError(err)
	}
	if !unchanged {
		return ErrUnauthenticated
	}
	return nil
}

// advanceAndRevoke 在已有排他控制锁下执行；版本与设备撤销同业务安全变更提交。
func advanceAndRevoke(ctx context.Context, tx *sqlx.Tx, userID uuid.UUID, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE users SET auth_version = auth_version + 1,
		updated_at = $1 WHERE id = $2`, now, userID); err != nil {
		return dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = $1
		WHERE user_id = $2 AND revoked_at IS NULL`, now, userID); err != nil {
		return dependencyError(err)
	}
	return nil
}
