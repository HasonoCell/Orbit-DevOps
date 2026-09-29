package access_test

import (
	"context"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type acceptedTLSSecret struct{}

func (acceptedTLSSecret) VerifyTLSSecret(context.Context, string, string, string) error { return nil }

func TestSecretOptionsAreProjectScopedAndRevocationVisible(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17-alpine", postgres.WithDatabase("access_options"),
		postgres.WithUsername("fixture"), postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(dsn); err != nil {
		t.Fatal(err)
	}
	db, err := sqlx.ConnectContext(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	identities, err := identity.New(db, projectauth.NewOwnershipGuard())
	if err != nil {
		t.Fatal(err)
	}
	const fixturePassword = "fixture-only!AccessOptions-September2026"
	if _, err := identities.InitializeAdmin(ctx, identity.InitializeAdminCommand{LoginName: "access-admin",
		DisplayName: "测试管理员", Password: fixturePassword, MaintenanceRef: "fixture-access-options"}); err != nil {
		t.Fatal(err)
	}
	login, err := identities.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "access-admin", Password: fixturePassword, SourceIP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := identities.ResolveSession(ctx, login.Token.CookieValue())
	if err != nil {
		t.Fatal(err)
	}
	authorizer := projectauth.New(db, identities)
	projects := project.New(db, authorizer)
	p, err := projects.Create(ctx, project.CreateCommand{Caller: owner, Name: "Yuuki", Slug: "yuuki-options", IdempotencyKey: "access-options-project"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := projects.Create(ctx, project.CreateCommand{Caller: owner, Name: "Other", Slug: "other-options", IdempotencyKey: "access-options-other"})
	if err != nil {
		t.Fatal(err)
	}
	module, err := access.New(db, authorizer, access.Config{ClusterRef: "kind-local", Namespace: "orbit-e2e"}, identities, acceptedTLSSecret{})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := module.RegisterSecret(ctx, access.RegisterSecretCommand{ProjectID: p.ID, Hostname: "PAYMENT.example.com",
		SecretName: "payment-cert", Caller: owner, IdempotencyKey: "register-payment-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		projectID uuid.UUID
		hostname  string
		count     int
	}{
		{p.ID, "payment.example.com", 1}, {p.ID, "other.example.com", 0}, {other.ID, "payment.example.com", 0},
	} {
		options, err := module.ListSecretBindingOptions(ctx, check.projectID, owner, check.hostname, access.Page{Limit: 20})
		if err != nil || len(options) != check.count {
			t.Fatalf("options %+v: %+v, %v", check, options, err)
		}
		if check.count == 1 && (options[0].ID != binding.ID || options[0].SecretName != "payment-cert") {
			t.Fatalf("unexpected safe option: %+v", options[0])
		}
	}
	host, err := module.CreateHost(ctx, access.HostCommand{ProjectID: p.ID, Caller: owner,
		IdempotencyKey: "host-payment-secret", Input: access.HostInput{Hostname: "payment.example.com",
			TLSMode: "existing_secret", SecretBindingID: &binding.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.RevokeSecret(ctx, access.RevokeSecretCommand{BindingID: binding.ID, Caller: owner,
		IdempotencyKey: "revoke-payment-secret"}); err != nil {
		t.Fatal(err)
	}
	options, err := module.ListSecretBindingOptions(ctx, p.ID, owner, "payment.example.com", access.Page{Limit: 20})
	if err != nil || len(options) != 0 {
		t.Fatalf("revoked option remained: %+v, %v", options, err)
	}
	current, err := module.GetHost(ctx, p.ID, host.ID, owner)
	if err != nil || current.SecretBindingState == nil || *current.SecretBindingState != "revoked" {
		t.Fatalf("host binding state: %+v, %v", current, err)
	}
}
