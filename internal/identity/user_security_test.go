package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// completeLocalFixture 始终经管理员准入、普通登录、本人改密和重新登录建立完整身份。
// 不通过直接写角色、伪造 Caller 或关闭生产门禁制造测试权限。
func completeLocalFixture(t *testing.T, module *identity.Module, admin identity.Caller, name string) (identity.User, identity.Caller, identity.LoginResult) {
	t.Helper()
	ctx := context.Background()
	user, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: admin, LoginName: name, DisplayName: "测试账号", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("admit security fixture: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: name, Password: fixturePassword, SourceIP: "127.0.0.2",
	})
	if err != nil {
		t.Fatalf("login temporary security fixture: %v", err)
	}
	if err := module.ChangePassword(ctx, identity.ChangePasswordCommand{
		Caller: resolveFixture(t, module, login), CurrentPassword: fixturePassword,
		NewPassword: fixturePassword + "-completed", SourceIP: "127.0.0.2",
	}); err != nil {
		t.Fatalf("complete security fixture: %v", err)
	}
	login = completedLoginFixture(t, module, name)
	return user, resolveFixture(t, module, login), login
}

func completedLoginFixture(t *testing.T, module *identity.Module, name string) identity.LoginResult {
	t.Helper()
	login, err := module.LoginLocal(context.Background(), identity.LocalLoginCommand{
		LoginName: name, Password: fixturePassword + "-completed", SourceIP: "127.0.0.2",
	})
	if err != nil {
		t.Fatalf("login completed security fixture: %v", err)
	}
	return login
}

func TestPlatformRoleChangeProtectsLastAdministratorAndRevokesOldProofs(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	if _, err := identity.New(db, nil); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("identity could run without mandatory owner guard: %v", err)
	}
	first := initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	command := identity.ChangePlatformRoleCommand{Caller: admin, UserID: first.ID, Role: identity.RoleUser}
	if _, err := module.ChangePlatformRole(ctx, command); !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("last administrator was demoted: %v", err)
	}
	second, ordinary, oldLogin := completeLocalFixture(t, module, admin, "fixture-second-admin")
	command.UserID = second.ID
	command.Role = identity.RolePlatformAdmin
	command.Caller = ordinary
	if _, err := module.ChangePlatformRole(ctx, command); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("ordinary user promoted itself: %v", err)
	}
	command.Caller = admin
	updated, err := module.ChangePlatformRole(ctx, command)
	if err != nil || updated.ID != second.ID || updated.PlatformRole != identity.RolePlatformAdmin {
		t.Fatalf("explicit promotion: %v", err)
	}
	if _, err := module.CurrentUser(ctx, ordinary); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("pre-promotion Caller inherited new administrator role: %v", err)
	}
	if _, err := module.ResolveSession(ctx, oldLogin.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("pre-promotion Cookie survived role change: %v", err)
	}
	secondAdmin := resolveFixture(t, module, completedLoginFixture(t, module, "fixture-second-admin"))
	command = identity.ChangePlatformRoleCommand{Caller: admin, UserID: first.ID, Role: identity.RoleUser}
	if _, err := module.ChangePlatformRole(ctx, command); err != nil {
		t.Fatalf("two administrators prevented ordinary self-demotion: %v", err)
	}
	if _, err := module.CurrentUser(ctx, admin); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("demotion did not revoke caller's own proof: %v", err)
	}
	command.Caller, command.UserID = secondAdmin, second.ID
	if _, err := module.ChangePlatformRole(ctx, command); !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("new last administrator was demoted: %v", err)
	}
	command.Role = identity.RolePlatformAdmin
	if _, err := module.ChangePlatformRole(ctx, command); err != nil {
		t.Fatalf("natural same-role replay failed: %v", err)
	}
	if _, err := module.CurrentUser(ctx, secondAdmin); err != nil {
		t.Fatalf("same-role replay unnecessarily revoked a valid proof: %v", err)
	}
}

func TestUserDisableAndEnableNeverRestoreAnOldSessionOrGrantRoles(t *testing.T) {
	t.Parallel()
	module, _ := newIdentity(t)
	first := initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: first.ID}); !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("last administrator was disabled: %v", err)
	}
	user, caller, login := completeLocalFixture(t, module, admin, "fixture-disable-target")
	second := completedLoginFixture(t, module, "fixture-disable-target")
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: caller, UserID: user.ID}); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("ordinary user performed account administration: %v", err)
	}
	command := identity.DisableUserCommand{Caller: admin, UserID: user.ID}
	disabled, err := module.DisableUser(ctx, command)
	if err != nil || disabled.Status != "disabled" || disabled.PlatformRole != identity.RoleUser {
		t.Fatalf("disable admitted user: %v", err)
	}
	for _, old := range []identity.LoginResult{login, second} {
		if _, err := module.ResolveSession(ctx, old.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("old device survived disable: %v", err)
		}
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("cached Caller survived disable: %v", err)
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "fixture-disable-target",
		Password: fixturePassword + "-completed", SourceIP: "127.0.0.2"}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("disabled user relogin reactivated account: %v", err)
	}
	if _, err := module.DisableUser(ctx, command); err != nil {
		t.Fatalf("natural disable replay: %v", err)
	}
	enabled, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: user.ID})
	if err != nil || enabled.Status != "active" || enabled.PlatformRole != identity.RoleUser {
		t.Fatalf("explicit enable granted roles or failed: %v", err)
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("enable resurrected an old Caller: %v", err)
	}
	current := resolveFixture(t, module, completedLoginFixture(t, module, "fixture-disable-target"))
	if _, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: user.ID}); err != nil {
		t.Fatalf("natural enable replay: %v", err)
	}
	if _, err := module.CurrentUser(ctx, current); err != nil {
		t.Fatalf("same-status replay revoked a fresh session: %v", err)
	}
}

// ownerFixtureProject 只建立 guard 的数据库前提；不是 HTTP/四角色接线的验收。
// 现有成员 schema 原子切换前用真实 UUID 的责任字段，历史文本只用于不可治理范围 fixture。
func ownerFixtureProject(t *testing.T, db *sqlx.DB, owner string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fixtureSQL(t, db, `INSERT INTO projects (id, name, slug, created_by, created_at)
		VALUES ($1, 'Owner fixture', $2, $3, clock_timestamp())`, id, id.String(), owner)
	if _, err := uuid.Parse(owner); err == nil {
		fixtureSQL(t, db, `UPDATE projects SET identity_state='governed' WHERE id=$1`, id)
	}
	ownerFixtureMember(t, db, id, owner, "owner")
	return id
}

func ownerFixtureMember(t *testing.T, db *sqlx.DB, projectID uuid.UUID, user string, role string) {
	t.Helper()
	userID, err := uuid.Parse(user)
	if err != nil {
		fixtureSQL(t, db, `INSERT INTO legacy_project_members
			(project_id, actor_id, role, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, 'fixture-maintenance', clock_timestamp(), clock_timestamp())`, projectID, user, role)
		return
	}
	fixtureSQL(t, db, `INSERT INTO project_members
		(project_id, user_id, role, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, 'fixture-maintenance', clock_timestamp(), clock_timestamp())`, projectID, userID, role)
}

func TestUserDisableProtectsEveryAffectedEffectiveOwnerButNotUnrelatedLegacyProjects(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	target, targetCaller, _ := completeLocalFixture(t, module, admin, "fixture-owner-target")
	other, _, _ := completeLocalFixture(t, module, admin, "fixture-other-owner")
	legacy := ownerFixtureProject(t, db, "historical-unclaimed-owner")
	first := ownerFixtureProject(t, db, target.ID.String())
	second := ownerFixtureProject(t, db, target.ID.String())
	ownerFixtureMember(t, db, first, other.ID.String(), "viewer")
	command := identity.DisableUserCommand{Caller: admin, UserID: target.ID}
	if _, err := module.DisableUser(ctx, command); !errors.Is(err, identity.ErrLastProjectOwner) {
		t.Fatalf("non-owner membership counted as effective owner: %v", err)
	}
	fixtureSQL(t, db, `UPDATE project_members SET role = 'owner' WHERE project_id = $1 AND user_id = $2`, first, other.ID)
	if _, err := module.DisableUser(ctx, command); !errors.Is(err, identity.ErrLastProjectOwner) {
		t.Fatalf("guard checked only one affected project: %v", err)
	}
	ownerFixtureMember(t, db, second, other.ID.String(), "owner")
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: other.ID}); err != nil {
		t.Fatalf("two effective owners did not allow one disable: %v", err)
	}
	if _, err := module.DisableUser(ctx, command); !errors.Is(err, identity.ErrLastProjectOwner) {
		t.Fatalf("disabled member counted as effective owner: %v", err)
	}
	if _, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: other.ID}); err != nil {
		t.Fatalf("explicit owner account enable: %v", err)
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET password_hash = repeat('broken!', 6) WHERE user_id = $1`, other.ID)
	if _, err := module.DisableUser(ctx, command); !errors.Is(err, identity.ErrLastProjectOwner) {
		t.Fatalf("unusable credential counted as effective owner: %v", err)
	}
	if _, err := module.CurrentUser(ctx, targetCaller); err != nil {
		t.Fatalf("rejected disable partially revoked target: %v", err)
	}
	// 重置建立了可用入口；临时密码需要本人完成，但不是已删除的入口。
	if err := module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{
		Caller: admin, UserID: other.ID, TemporaryPassword: fixturePassword + "-owner-reset",
	}); err != nil {
		t.Fatalf("restore registered owner credential: %v", err)
	}
	if _, err := module.DisableUser(ctx, command); err != nil {
		t.Fatalf("effective alternative owners or unrelated legacy project blocked disable: %v", err)
	}
	var preserved string
	if err := db.GetContext(ctx, &preserved, `SELECT actor_id FROM legacy_project_members WHERE project_id = $1`, legacy); err != nil || preserved != "historical-unclaimed-owner" {
		t.Fatal("owner guard rewrote or admitted historical identity")
	}
}

func TestLastAdministratorCountsOnlyActiveSupportedLoginEntries(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	first := initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	second, _, _ := completeLocalFixture(t, module, admin, "fixture-factor-admin")
	ctx := context.Background()
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: admin, UserID: second.ID, Role: identity.RolePlatformAdmin,
	}); err != nil {
		t.Fatalf("grant factor fixture administrator: %v", err)
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET password_hash = repeat('broken!', 6) WHERE user_id = $1`, second.ID)
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: admin, UserID: first.ID, Role: identity.RoleUser,
	}); !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("damaged credential retained effective administrator: %v", err)
	}
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: second.ID}); err != nil {
		t.Fatalf("ineffective administrator change was treated as global zero precondition: %v", err)
	}
	if err := module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{
		Caller: admin, UserID: second.ID, TemporaryPassword: fixturePassword + "-admin-reset",
	}); err != nil {
		t.Fatalf("reset disabled administrator entry: %v", err)
	}
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: first.ID}); !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("disabled administrator with valid credential counted: %v", err)
	}
	if _, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: second.ID}); err != nil {
		t.Fatalf("restore disabled administrator: %v", err)
	}
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: first.ID}); err != nil {
		t.Fatalf("valid active credential did not preserve administrator entry: %v", err)
	}
}
