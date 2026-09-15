package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// 所有凭据仅属于销毁后不可复用的合成 fixture，不连接开发账号数据库。
const fixturePassword = "fixture-only!Orbit-September-2026"

func newIdentity(t *testing.T) (*identity.Module, *sqlx.DB) {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("identity_fixture"), postgres.WithUsername("fixture"),
		postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start isolated PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate identity fixture: %v", err)
		}
	})
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal("get fixture connection string")
	}
	if err := database.Migrate(dsn); err != nil {
		t.Fatalf("migrate fixture: %v", err)
	}
	db, err := sqlx.ConnectContext(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal("connect fixture database")
	}
	t.Cleanup(func() { _ = db.Close() })
	module, err := identity.New(db)
	if err != nil {
		t.Fatalf("construct identity: %v", err)
	}
	return module, db
}

func TestWeakPasswordDoesNotConsumeInitialization(t *testing.T) {
	t.Parallel()
	module, _ := newIdentity(t)
	ctx := context.Background()
	command := identity.InitializeAdminCommand{
		LoginName: "fixture-admin", DisplayName: "测试管理员",
		Password: "short", MaintenanceRef: "fixture-initialization",
	}
	if _, err := module.InitializeAdmin(ctx, command); !errors.Is(err, identity.ErrInvalidPassword) {
		t.Fatalf("weak password error = %v", err)
	}
	command.Password = fixturePassword
	user, err := module.InitializeAdmin(ctx, command)
	if err != nil {
		t.Fatalf("initialize after rejected password: %v", err)
	}
	if user.ID.String() == "00000000-0000-0000-0000-000000000000" || user.PlatformRole != identity.RolePlatformAdmin {
		t.Fatalf("initial user must have a real UUID and platform admin role")
	}
	if _, err := module.InitializeAdmin(ctx, command); !errors.Is(err, identity.ErrAlreadyInitialized) {
		t.Fatalf("second initialize error = %v", err)
	}
}

func TestLocalLoginReturnsTrustedCallerWithoutExposingCredential(t *testing.T) {
	t.Parallel()
	module, _ := newIdentity(t)
	ctx := context.Background()
	admin, err := module.InitializeAdmin(ctx, identity.InitializeAdminCommand{
		LoginName: " Fixture-Admin ", DisplayName: "测试管理员", Password: fixturePassword,
		MaintenanceRef: "fixture-login",
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	result, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "FIXTURE-ADMIN", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("local login: %v", err)
	}
	caller, err := module.ResolveSession(ctx, result.Token.CookieValue())
	if err != nil || caller.UserID() != admin.ID || caller.ActorID() != admin.ID.String() {
		t.Fatalf("resolve must return the internally assigned user, error = %v", err)
	}
	current, err := module.CurrentUser(ctx, caller)
	if err != nil || current.User.ID != admin.ID || current.MustChangePassword {
		t.Fatalf("initial admin must have full account access, error = %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal("marshal public login result")
	}
	if strings.Contains(string(encoded), result.Token.CookieValue()) || strings.Contains(string(encoded), fixturePassword) || strings.Contains(string(encoded), "argon2id") {
		t.Fatal("public login result leaks credential material")
	}
	if _, err := module.CurrentUser(ctx, identity.Caller{}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("zero caller error = %v", err)
	}
	for _, command := range []identity.LocalLoginCommand{
		{LoginName: "fixture-admin", Password: fixturePassword + "!", SourceIP: "127.0.0.1"},
		{LoginName: "unknown-account", Password: fixturePassword, SourceIP: "127.0.0.1"},
	} {
		if _, err := module.LoginLocal(ctx, command); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("invalid login must use uniform authentication error, got %v", err)
		}
	}
	if err := module.LogoutCurrent(ctx, result.Token.CookieValue()); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := module.ResolveSession(ctx, result.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("revoked session resolved: %v", err)
	}
}

func TestAuthenticationRateLimitIsSharedAcrossModuleInstances(t *testing.T) {
	t.Parallel()
	first, db := newIdentity(t)
	second, err := identity.New(db)
	if err != nil {
		t.Fatal("construct second identity instance")
	}
	ctx := context.Background()
	command := identity.LocalLoginCommand{LoginName: "nonexistent-account", Password: fixturePassword, SourceIP: "127.0.0.1"}
	for i := 0; i < 10; i++ {
		module := first
		if i%2 == 1 {
			module = second
		}
		if _, err := module.LoginLocal(ctx, command); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("attempt %d error = %v", i+1, err)
		}
	}
	if _, err := second.LoginLocal(ctx, command); !errors.Is(err, identity.ErrRateLimited) {
		t.Fatalf("another instance bypassed shared account limit: %v", err)
	}
}

func TestLogoutAllRevokesBothDevicesAcrossInstances(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	ctx := context.Background()
	if _, err := module.InitializeAdmin(ctx, identity.InitializeAdminCommand{
		LoginName: "fixture-admin", DisplayName: "测试管理员", Password: fixturePassword,
		MaintenanceRef: "fixture-all-sessions",
	}); err != nil {
		t.Fatal("initialize fixture")
	}
	command := identity.LocalLoginCommand{LoginName: "fixture-admin", Password: fixturePassword, SourceIP: "127.0.0.1"}
	first, err := module.LoginLocal(ctx, command)
	if err != nil {
		t.Fatalf("first device login: %v", err)
	}
	second, err := module.LoginLocal(ctx, command)
	if err != nil {
		t.Fatalf("second device login: %v", err)
	}
	caller, err := module.ResolveSession(ctx, first.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve device: %v", err)
	}
	otherInstance, err := identity.New(db)
	if err != nil {
		t.Fatal("construct other instance")
	}
	if err := otherInstance.LogoutAll(ctx, caller); err != nil {
		t.Fatalf("logout all: %v", err)
	}
	for _, result := range []identity.LoginResult{first, second} {
		if _, err := module.ResolveSession(ctx, result.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
			t.Fatalf("one device remained authorized: %v", err)
		}
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Caller bypassed revocation: %v", err)
	}
	if err := module.LogoutCurrent(ctx, ""); err != nil {
		t.Fatalf("missing Cookie logout must succeed: %v", err)
	}
	if err := module.LogoutAll(ctx, identity.Caller{}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("unproven caller revoked all sessions: %v", err)
	}
}
