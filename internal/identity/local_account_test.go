package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
)

func resolveFixture(t *testing.T, module *identity.Module, result identity.LoginResult) identity.Caller {
	t.Helper()
	caller, err := module.ResolveSession(context.Background(), result.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve fixture session: %v", err)
	}
	return caller
}

func TestPasswordChangeCompletesTemporaryAccountAndRevokesEveryOldProof(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	user, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: admin, LoginName: "fixture-developer", DisplayName: "测试开发者", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("admit fixture user: %v", err)
	}
	loginCommand := identity.LocalLoginCommand{LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1"}
	first, err := module.LoginLocal(ctx, loginCommand)
	if err != nil {
		t.Fatalf("first device login: %v", err)
	}
	second, err := module.LoginLocal(ctx, loginCommand)
	if err != nil {
		t.Fatalf("second device login: %v", err)
	}
	caller := resolveFixture(t, module, first)
	command := identity.ChangePasswordCommand{Caller: caller,
		CurrentPassword: fixturePassword, NewPassword: fixturePassword + "-changed", SourceIP: "127.0.0.1"}
	command.CurrentPassword += "-wrong"
	if err := module.ChangePassword(ctx, command); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("wrong old password changed a credential: %v", err)
	}
	command.CurrentPassword = fixturePassword
	command.NewPassword = fixturePassword
	if err := module.ChangePassword(ctx, command); !errors.Is(err, identity.ErrInvalidPassword) {
		t.Fatalf("temporary password could be retained as a full credential: %v", err)
	}
	command.NewPassword = fixturePassword + "-changed"
	command.LoginName = "another-login"
	if err := module.ChangePassword(ctx, command); !errors.Is(err, identity.ErrInvalidCommand) {
		t.Fatalf("existing login name was changed through password API: %v", err)
	}
	command.LoginName = ""
	if err := module.ChangePassword(ctx, command); err != nil {
		t.Fatalf("change temporary password: %v", err)
	}
	for _, login := range []identity.LoginResult{first, second} {
		if _, err := module.ResolveSession(ctx, login.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("old device survived password change: %v", err)
		}
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("cached Caller survived password change: %v", err)
	}
	loginCommand.Password = command.NewPassword
	completed, err := module.LoginLocal(ctx, loginCommand)
	if err != nil || completed.CurrentUser.MustChangePassword || completed.CurrentUser.User.ID != user.ID {
		t.Fatalf("completed user login: %v", err)
	}
	full := resolveFixture(t, module, completed)
	tx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin full user acceptance fixture")
	}
	_, err = module.AuthorizeInTx(ctx, tx, full)
	_ = tx.Rollback()
	if err != nil {
		t.Fatalf("password change did not complete account: %v", err)
	}
	if _, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: full, LoginName: "forbidden-admission", DisplayName: "不能准入", TemporaryPassword: fixturePassword,
	}); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("ordinary completed user became platform administrator: %v", err)
	}
}

func TestAdministratorAdmitsLocalUserWithTemporaryPasswordRestriction(t *testing.T) {
	t.Parallel()
	module, _ := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	command := identity.CreateLocalUserCommand{
		Caller: admin, LoginName: " Fixture-Developer ", DisplayName: "测试开发者",
		TemporaryPassword: fixturePassword,
	}
	user, err := module.CreateLocalUser(ctx, command)
	if err != nil || user.PlatformRole != identity.RoleUser || user.Status != "active" {
		t.Fatalf("admit ordinary local user: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil || login.CurrentUser.User.ID != user.ID || !login.CurrentUser.MustChangePassword {
		t.Fatalf("new user must require password change: %v", err)
	}
	command.Caller = resolveFixture(t, module, login)
	command.LoginName = "another-account"
	if _, err := module.CreateLocalUser(ctx, command); !errors.Is(err, identity.ErrPasswordChangeRequired) {
		t.Fatalf("temporary account performed platform administration: %v", err)
	}
	command.Caller = identity.Caller{}
	if _, err := module.CreateLocalUser(ctx, command); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("untrusted caller admitted an account: %v", err)
	}
	command.Caller = admin
	command.LoginName = "FIXTURE-DEVELOPER"
	if _, err := module.CreateLocalUser(ctx, command); !errors.Is(err, identity.ErrLoginNameConflict) {
		t.Fatalf("canonical login name was not unique: %v", err)
	}
}

func TestLocalReauthenticationRotatesOnlyCurrentDeviceWithoutLiftingRestriction(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	first := loginFixture(t, module)
	second := loginFixture(t, module)
	caller := resolveFixture(t, module, first)
	fixtureSQL(t, db, `UPDATE auth_sessions SET primary_authenticated_at = statement_timestamp() - interval '6 minutes'
		WHERE user_id = $1`, user.ID)
	create := identity.CreateLocalUserCommand{Caller: caller, LoginName: "requires-fresh-proof",
		DisplayName: "需要近期认证", TemporaryPassword: fixturePassword}
	if _, err := module.CreateLocalUser(context.Background(), create); !errors.Is(err, identity.ErrRecentAuthenticationRequired) {
		t.Fatalf("last_seen activity was mistaken for recent primary authentication: %v", err)
	}
	reauthenticated, err := module.ReauthenticateLocal(context.Background(), identity.ReauthenticateLocalCommand{
		Caller: caller, Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("local reauthentication: %v", err)
	}
	if reauthenticated.Token.CookieValue() == first.Token.CookieValue() {
		t.Fatal("reauthentication retained the old Cookie")
	}
	if !reauthenticated.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("reauthentication extended the original absolute session lifetime")
	}
	if _, err := module.CurrentUser(context.Background(), caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Caller survived session rotation: %v", err)
	}
	if _, err := module.ResolveSession(context.Background(), first.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Cookie survived session rotation: %v", err)
	}
	resolveFixture(t, module, second)
	create.Caller = resolveFixture(t, module, reauthenticated)
	if _, err := module.CreateLocalUser(context.Background(), create); err != nil {
		t.Fatalf("reauthentication did not establish recent proof: %v", err)
	}
	// 已验证近期密码不是“解除临时限制”；用真实凭据状态作为 fixture 前提。
	fixtureSQL(t, db, `UPDATE local_credentials SET must_change_password = true WHERE user_id = $1`, user.ID)
	restricted, err := module.ReauthenticateLocal(context.Background(), identity.ReauthenticateLocalCommand{
		Caller: create.Caller, Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil || !restricted.CurrentUser.MustChangePassword {
		t.Fatalf("reauthentication bypassed temporary credential restriction: %v", err)
	}
	create.Caller = resolveFixture(t, module, restricted)
	create.LoginName = "still-forbidden"
	if _, err := module.CreateLocalUser(context.Background(), create); !errors.Is(err, identity.ErrPasswordChangeRequired) {
		t.Fatalf("recent proof bypassed password change requirement: %v", err)
	}
}

func TestAdministratorResetMakesNewPasswordTemporaryAndRevokesExistingDevices(t *testing.T) {
	t.Parallel()
	module, _ := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	user, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: admin, LoginName: "fixture-developer", DisplayName: "测试开发者", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("create reset fixture user: %v", err)
	}
	loginCommand := identity.LocalLoginCommand{LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1"}
	first, err := module.LoginLocal(ctx, loginCommand)
	if err != nil {
		t.Fatalf("first fixture device: %v", err)
	}
	second, err := module.LoginLocal(ctx, loginCommand)
	if err != nil {
		t.Fatalf("second fixture device: %v", err)
	}
	caller := resolveFixture(t, module, first)
	reset := identity.ResetLocalPasswordCommand{Caller: admin, UserID: user.ID,
		TemporaryPassword: fixturePassword + "-reset"}
	reset.LoginName = "renamed-account"
	if err := module.ResetLocalPassword(ctx, reset); !errors.Is(err, identity.ErrInvalidCommand) {
		t.Fatalf("reset renamed an existing local account: %v", err)
	}
	reset.LoginName = ""
	if err := module.ResetLocalPassword(ctx, reset); err != nil {
		t.Fatalf("administrator reset: %v", err)
	}
	for _, device := range []identity.LoginResult{first, second} {
		if _, err := module.ResolveSession(ctx, device.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("old device survived password reset: %v", err)
		}
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Caller survived password reset: %v", err)
	}
	if _, err := module.LoginLocal(ctx, loginCommand); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old password survived reset: %v", err)
	}
	loginCommand.Password = reset.TemporaryPassword
	newDevice, err := module.LoginLocal(ctx, loginCommand)
	if err != nil || !newDevice.CurrentUser.MustChangePassword || newDevice.CurrentUser.User.ID != user.ID {
		t.Fatalf("reset credential did not become temporary: %v", err)
	}
	// 重置目标不影响管理员自身设备。
	if _, err := module.CurrentUser(ctx, admin); err != nil {
		t.Fatalf("target reset revoked administrator: %v", err)
	}
}

func TestAdministratorCanReadUserSummaryWithoutRecentProofButNotFromRestrictedSession(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	adminUser := initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	user, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: admin, LoginName: "fixture-developer", DisplayName: "测试开发者", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("create summary fixture user: %v", err)
	}
	fixtureSQL(t, db, `UPDATE auth_sessions SET primary_authenticated_at = statement_timestamp() - interval '6 minutes'
		WHERE user_id = $1`, adminUser.ID)
	summary, err := module.GetUser(ctx, admin, user.ID)
	if err != nil || summary.ID != user.ID || summary.DisplayName != "测试开发者" {
		t.Fatalf("administrator user summary required unnecessary reauthentication: %v", err)
	}
	if _, err := module.GetUser(ctx, admin, uuid.New()); !errors.Is(err, identity.ErrUserNotFound) {
		t.Fatalf("missing user summary: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("summary fixture user login: %v", err)
	}
	caller := resolveFixture(t, module, login)
	if _, err := module.GetUser(ctx, caller, adminUser.ID); !errors.Is(err, identity.ErrPasswordChangeRequired) {
		t.Fatalf("restricted user read platform account summary: %v", err)
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET must_change_password = false WHERE user_id = $1`, user.ID)
	if _, err := module.GetUser(ctx, caller, adminUser.ID); !errors.Is(err, identity.ErrForbidden) {
		t.Fatalf("ordinary user accessed platform account directory: %v", err)
	}
	fixtureSQL(t, db, `UPDATE users SET status = 'disabled' WHERE id = $1`, user.ID)
	if err := module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{Caller: admin,
		UserID: user.ID, TemporaryPassword: fixturePassword + "-disabled-reset",
	}); !errors.Is(err, identity.ErrRecentAuthenticationRequired) {
		t.Fatalf("stale administrator proof reset a password: %v", err)
	}
	// 重新登录取得近期证明；重置不应该被当成账号启用。
	admin = resolveFixture(t, module, loginFixture(t, module))
	if err := module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{Caller: admin,
		UserID: user.ID, TemporaryPassword: fixturePassword + "-disabled-reset",
	}); err != nil {
		t.Fatalf("reset disabled account credential: %v", err)
	}
	summary, err = module.GetUser(ctx, admin, user.ID)
	if err != nil || summary.Status != "disabled" {
		t.Fatalf("password reset implicitly enabled disabled user: %v", err)
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword + "-disabled-reset", SourceIP: "127.0.0.1",
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("disabled reset account could log in: %v", err)
	}
}
