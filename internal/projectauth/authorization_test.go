package projectauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const password = "fixture-only!ProjectRoles-September2026"

func roleFixture(t *testing.T) (*sqlx.DB, *identity.Module, identity.Caller) {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:17-alpine", postgres.WithDatabase("roles_fixture"),
		postgres.WithUsername("fixture"), postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal("start isolated role fixture")
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal("get role fixture connection")
	}
	if err := database.Migrate(dsn); err != nil {
		t.Fatal("migrate role fixture")
	}
	db, err := sqlx.ConnectContext(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal("connect role fixture")
	}
	t.Cleanup(func() { _ = db.Close() })
	identities, err := identity.New(db, projectauth.NewOwnershipGuard())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identities.InitializeAdmin(ctx, identity.InitializeAdminCommand{LoginName: "fixture-owner",
		DisplayName: "测试所有者", Password: password, MaintenanceRef: "fixture-project-roles"}); err != nil {
		t.Fatal(err)
	}
	login, err := identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "fixture-owner", Password: password, SourceIP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := identities.ResolveSession(ctx, login.Token.CookieValue())
	if err != nil {
		t.Fatal(err)
	}
	return db, identities, owner
}

func roleUser(t *testing.T, identities *identity.Module, owner identity.Caller, name string) identity.Caller {
	t.Helper()
	ctx := context.Background()
	if _, err := identities.CreateLocalUser(ctx, identity.CreateLocalUserCommand{Caller: owner,
		LoginName: name, DisplayName: "测试成员", TemporaryPassword: password}); err != nil {
		t.Fatal(err)
	}
	login, err := identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: name, Password: password, SourceIP: "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := identities.ResolveSession(ctx, login.Token.CookieValue())
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.ChangePassword(ctx, identity.ChangePasswordCommand{Caller: caller,
		CurrentPassword: password, NewPassword: password + "-completed", SourceIP: "127.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	login, err = identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: name, Password: password + "-completed", SourceIP: "127.0.0.2"})
	if err != nil {
		t.Fatal(err)
	}
	caller, err = identities.ResolveSession(ctx, login.Token.CookieValue())
	if err != nil {
		t.Fatal(err)
	}
	return caller
}

func TestFourRolesAuthorizeOnlyKnownProjectCapabilities(t *testing.T) {
	db, identities, owner := roleFixture(t)
	auth := projectauth.New(db, identities)
	ctx := context.Background()
	p, err := project.New(db, auth).Create(ctx, project.CreateCommand{Caller: owner,
		Name: "Yuuki", Slug: "yuuki", IdempotencyKey: "fixture-project"})
	if err != nil {
		t.Fatal(err)
	}
	roles := []struct {
		name    string
		caller  identity.Caller
		allowed map[projectauth.Permission]bool
	}{
		{projectauth.RoleOwner, owner, map[projectauth.Permission]bool{projectauth.PermissionRead: true, projectauth.PermissionReadLogs: true,
			projectauth.PermissionDevelop: true, projectauth.PermissionManageMembers: true, projectauth.PermissionManageOwners: true, projectauth.PermissionResolveUnknown: true}},
		{projectauth.RoleAdmin, roleUser(t, identities, owner, "fixture-admin"), map[projectauth.Permission]bool{projectauth.PermissionRead: true,
			projectauth.PermissionReadLogs: true, projectauth.PermissionDevelop: true, projectauth.PermissionManageMembers: true, projectauth.PermissionResolveUnknown: true}},
		{projectauth.RoleDeveloper, roleUser(t, identities, owner, "fixture-developer"), map[projectauth.Permission]bool{projectauth.PermissionRead: true,
			projectauth.PermissionReadLogs: true, projectauth.PermissionDevelop: true}},
		{projectauth.RoleViewer, roleUser(t, identities, owner, "fixture-viewer"), map[projectauth.Permission]bool{projectauth.PermissionRead: true}},
	}
	for _, role := range roles {
		if role.name != projectauth.RoleOwner {
			if _, err := auth.AddMember(ctx, projectauth.AddMemberCommand{Caller: owner, ProjectID: p.ID,
				UserID: role.caller.UserID(), Role: role.name, IdempotencyKey: "fixture-member-" + role.name}); err != nil {
				t.Fatal(err)
			}
		}
		for _, permission := range []projectauth.Permission{projectauth.PermissionRead, projectauth.PermissionReadLogs, projectauth.PermissionDevelop,
			projectauth.PermissionManageMembers, projectauth.PermissionManageOwners, projectauth.PermissionResolveUnknown, "future_undefined_permission"} {
			err := auth.Require(ctx, p.ID, role.caller, permission)
			if role.allowed[permission] && err != nil || !role.allowed[permission] && !errors.Is(err, projectauth.ErrForbidden) {
				t.Fatalf("role %s permission %s: %v", role.name, permission, err)
			}
		}
	}
	other, err := project.New(db, auth).Create(ctx, project.CreateCommand{Caller: roles[1].caller,
		Name: "Other", Slug: "other", IdempotencyKey: "fixture-other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Require(ctx, other.ID, owner, projectauth.PermissionRead); !errors.Is(err, projectauth.ErrNotMember) {
		t.Fatalf("platform admin bypassed project membership: %v", err)
	}
}
