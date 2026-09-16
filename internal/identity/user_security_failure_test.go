package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
)

func TestUserSecurityCommandsRequireCurrentCompleteRecentAdministrator(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	adminUser := initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	target, ordinary, _ := completeLocalFixture(t, module, admin, "fixture-security-ordinary")
	ctx := context.Background()
	temporary, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{
		Caller: admin, LoginName: "fixture-security-temporary", DisplayName: "受限管理员", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("admit restricted security fixture: %v", err)
	}
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: admin, UserID: temporary.ID, Role: identity.RolePlatformAdmin,
	}); err != nil {
		t.Fatalf("grant restricted administrator fixture: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-security-temporary", Password: fixturePassword, SourceIP: "127.0.0.3",
	})
	if err != nil {
		t.Fatalf("login restricted administrator fixture: %v", err)
	}
	restricted := resolveFixture(t, module, login)
	commands := []struct {
		name string
		run  func(identity.Caller, uuid.UUID) (identity.User, error)
	}{
		{"role", func(c identity.Caller, id uuid.UUID) (identity.User, error) {
			return module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{Caller: c, UserID: id, Role: identity.RoleUser})
		}},
		{"disable", func(c identity.Caller, id uuid.UUID) (identity.User, error) {
			return module.DisableUser(ctx, identity.DisableUserCommand{Caller: c, UserID: id})
		}},
		{"enable", func(c identity.Caller, id uuid.UUID) (identity.User, error) {
			return module.EnableUser(ctx, identity.EnableUserCommand{Caller: c, UserID: id})
		}},
	}
	for _, command := range commands {
		for _, proof := range []struct {
			name   string
			caller identity.Caller
			want   error
		}{
			{"zero", identity.Caller{}, identity.ErrUnauthenticated},
			{"ordinary", ordinary, identity.ErrForbidden},
			{"temporary administrator", restricted, identity.ErrPasswordChangeRequired},
		} {
			t.Run(command.name+"/"+proof.name, func(t *testing.T) {
				if _, err := command.run(proof.caller, target.ID); !errors.Is(err, proof.want) {
					t.Fatalf("security command admitted an ineligible proof: %v", err)
				}
			})
		}
		if _, err := command.run(admin, uuid.New()); !errors.Is(err, identity.ErrUserNotFound) {
			t.Fatalf("authorized command missing target: %v", err)
		}
	}
	fixtureSQL(t, db, `UPDATE auth_sessions SET primary_authenticated_at = clock_timestamp() - interval '6 minutes'
		WHERE user_id = $1 AND revoked_at IS NULL`, adminUser.ID)
	for _, command := range commands {
		if _, err := command.run(admin, target.ID); !errors.Is(err, identity.ErrRecentAuthenticationRequired) {
			t.Fatalf("%s accepted stale primary authentication: %v", command.name, err)
		}
	}
	if err := module.LogoutAll(ctx, admin); err != nil {
		t.Fatalf("revoke security fixture: %v", err)
	}
	for _, command := range commands {
		if _, err := command.run(admin, target.ID); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("%s accepted revoked cached Caller: %v", command.name, err)
		}
	}
}

func TestUserSecurityAuditFailureRollsBackStatusRoleAndEveryRevocation(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	target, caller, login := completeLocalFixture(t, module, admin, "fixture-security-audit")
	ctx := context.Background()
	fixtureSQL(t, db, `CREATE FUNCTION block_user_security_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.action IN ('user.platform_role.change', 'user.disable', 'user.enable') THEN
		 RAISE EXCEPTION 'fixture security audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER block_user_security_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_user_security_audit()`)
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: admin, UserID: target.ID, Role: identity.RolePlatformAdmin,
	}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("promotion succeeded without security audit: %v", err)
	}
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: target.ID}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("disable succeeded without security audit: %v", err)
	}
	current, err := module.CurrentUser(ctx, caller)
	if err != nil || current.User.Status != "active" || current.User.PlatformRole != identity.RoleUser {
		t.Fatalf("failed security changes committed status/role/version/revocation: %v", err)
	}
	resolveFixture(t, module, login)
	fixtureSQL(t, db, `DROP TRIGGER block_user_security_audit ON audit_records`)
	if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: target.ID}); err != nil {
		t.Fatalf("disable after dependency restored: %v", err)
	}
	fixtureSQL(t, db, `CREATE TRIGGER block_user_security_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_user_security_audit()`)
	if _, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: target.ID}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("enable succeeded without security audit: %v", err)
	}
	summary, err := module.GetUser(ctx, admin, target.ID)
	if err != nil || summary.Status != "disabled" {
		t.Fatalf("failed enable committed account status: %v", err)
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-security-audit", Password: fixturePassword + "-completed", SourceIP: "127.0.0.2",
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("failed enable allowed a new session: %v", err)
	}
}

func TestUserSecurityNaturalReplaysDoNotDuplicateAuditOrRevokeFreshSessions(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	target, _, _ := completeLocalFixture(t, module, admin, "fixture-security-replay")
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
			Caller: admin, UserID: target.ID, Role: identity.RolePlatformAdmin,
		}); err != nil {
			t.Fatalf("promotion/replay: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: target.ID}); err != nil {
			t.Fatalf("disable/replay: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := module.EnableUser(ctx, identity.EnableUserCommand{Caller: admin, UserID: target.ID}); err != nil {
			t.Fatalf("enable/replay: %v", err)
		}
	}
	for _, action := range []string{"user.platform_role.change", "user.disable", "user.enable"} {
		var count int
		if err := db.GetContext(ctx, &count, `SELECT count(*) FROM audit_records
			WHERE target_id = $1 AND actor_id = $2 AND actor_kind = 'user' AND action = $3`,
			target.ID, admin.UserID().String(), action); err != nil || count != 1 {
			t.Fatalf("natural replay duplicated/misidentified security audit for %s", action)
		}
	}
	current := resolveFixture(t, module, completedLoginFixture(t, module, "fixture-security-replay"))
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: admin, UserID: target.ID, Role: identity.RolePlatformAdmin,
	}); err != nil {
		t.Fatalf("fresh-session role replay: %v", err)
	}
	if _, err := module.CurrentUser(ctx, current); err != nil {
		t.Fatalf("natural replay unnecessarily revoked fresh device: %v", err)
	}
}
