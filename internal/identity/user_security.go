package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// ChangePlatformRoleCommand 不复用项目角色；任免需要管理员的当前身份与近期主认证。
type ChangePlatformRoleCommand struct {
	Caller Caller
	UserID uuid.UUID
	Role   string
}

type DisableUserCommand struct {
	Caller Caller
	UserID uuid.UUID
}

type EnableUserCommand struct {
	Caller Caller
	UserID uuid.UUID
}

// DisableUser 只阻止后续人工访问，不删除项目成员、历史或已接纳的 Operation。
func (m *Module) DisableUser(ctx context.Context, command DisableUserCommand) (User, error) {
	return m.changeUserStatus(ctx, command.Caller, command.UserID, "disabled")
}

// EnableUser 是管理员明确恢复；不恢复旧 Cookie、不签发新会话、不授予任何角色。
func (m *Module) EnableUser(ctx context.Context, command EnableUserCommand) (User, error) {
	return m.changeUserStatus(ctx, command.Caller, command.UserID, "active")
}

func (m *Module) changeUserStatus(ctx context.Context, caller Caller, id uuid.UUID, status string) (User, error) {
	if id == uuid.Nil {
		return User{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	admin, err := requireAdministrator(ctx, tx, caller, true)
	if err != nil {
		return User{}, err
	}
	user, err := userForSecurityChange(ctx, tx, id)
	if err != nil {
		return User{}, err
	}
	if user.Status == status {
		if err := tx.Commit(); err != nil {
			return User{}, dependencyError(err)
		}
		return user, nil
	}
	if status == "disabled" {
		if user.PlatformRole == RolePlatformAdmin {
			if err := requireOtherAdministrator(ctx, tx, user.ID); err != nil {
				return User{}, err
			}
		}
		if err := m.ownership.RequireRemainingOwner(ctx, tx, user.ID); err != nil {
			if errors.Is(err, ErrLastProjectOwner) {
				return User{}, ErrLastProjectOwner
			}
			return User{}, dependencyError(err)
		}
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return User{}, err
	}
	previous := user.Status
	if _, err := tx.ExecContext(ctx, `UPDATE users SET status = $1 WHERE id = $2`, status, user.ID); err != nil {
		return User{}, dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, user.ID, now); err != nil {
		return User{}, err
	}
	action := "user.disable"
	if status == "active" {
		action = "user.enable"
	}
	if err := appendUserSecurityAudit(ctx, tx, admin.ID, user.ID, action,
		map[string]any{"previousStatus": previous, "status": status}, now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	user.Status = status
	return user, nil
}

// ChangePlatformRole 显式任免平台管理员，不授予任何项目成员关系。
// 控制排他锁在目标 User 锁前取得；改变角色、撤销所有会话、审计同事务。
func (m *Module) ChangePlatformRole(ctx context.Context, command ChangePlatformRoleCommand) (User, error) {
	if command.UserID == uuid.Nil || command.Role != RolePlatformAdmin && command.Role != RoleUser {
		return User{}, ErrInvalidCommand
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
	user, err := userForSecurityChange(ctx, tx, command.UserID)
	if err != nil {
		return User{}, err
	}
	// 自然重放也先验证当前管理员，不替已撤销身份提供重复调用旁路。
	if user.PlatformRole == command.Role {
		if err := tx.Commit(); err != nil {
			return User{}, dependencyError(err)
		}
		return user, nil
	}
	if user.PlatformRole == RolePlatformAdmin && command.Role == RoleUser {
		if err := requireOtherAdministrator(ctx, tx, user.ID); err != nil {
			return User{}, err
		}
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return User{}, err
	}
	previous := user.PlatformRole
	if _, err := tx.ExecContext(ctx, `UPDATE users SET platform_role = $1 WHERE id = $2`, command.Role, user.ID); err != nil {
		return User{}, dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, user.ID, now); err != nil {
		return User{}, err
	}
	if err := appendUserSecurityAudit(ctx, tx, admin.ID, user.ID, "user.platform_role.change",
		map[string]any{"previousRole": previous, "role": command.Role}, now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	user.PlatformRole = command.Role
	return user, nil
}

func userForSecurityChange(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) (User, error) {
	var user User
	if err := tx.GetContext(ctx, &user, `SELECT id, display_name, status, platform_role, created_at
		FROM users WHERE id = $1 FOR UPDATE`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrUserNotFound
		}
		return User{}, dependencyError(err)
	}
	return user, nil
}

// 只保护本次实际减少的有效管理员；应急已停用/无入口的目标不制造全局零前置条件。
func requireOtherAdministrator(ctx context.Context, tx *sqlx.Tx, excluded uuid.UUID) error {
	var safe bool
	if err := tx.GetContext(ctx, &safe, `SELECT
		NOT EXISTS (SELECT 1 FROM effective_identity_users WHERE id = $1)
		OR EXISTS (SELECT 1 FROM effective_identity_users e JOIN users u ON u.id = e.id
			WHERE u.platform_role = 'platform_admin' AND u.id <> $1)`, excluded); err != nil {
		return dependencyError(err)
	}
	if !safe {
		return ErrLastAdministrator
	}
	return nil
}

func appendUserSecurityAudit(ctx context.Context, tx *sqlx.Tx, actor, target uuid.UUID, action string,
	summary map[string]any, now time.Time) error {
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: actor.String(), Action: action,
		TargetType: "user", TargetID: target, Summary: summary, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return nil
}
