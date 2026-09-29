package identity_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

type oidcFixtureAdapter struct {
	mu       sync.Mutex
	nonces   map[string]string
	subjects map[string]string
	recent   map[string]bool
}

func newOIDCFixtureAdapter() *oidcFixtureAdapter {
	return &oidcFixtureAdapter{nonces: make(map[string]string), subjects: map[string]string{
		"pending": "subject-one", "same-email": "subject-two", "bound": "bound-subject",
		"reauth": "bound-subject", "reauth-one": "subject-one",
	}, recent: map[string]bool{"bound": true, "reauth": true, "reauth-one": true, "recent-login": true}}
}

func (adapter *oidcFixtureAdapter) AuthorizationURL(_ context.Context, _ identity.OIDCProviderRuntime,
	state, nonce, verifier string, forceRecent bool) (string, error) {
	adapter.mu.Lock()
	adapter.nonces[verifier] = nonce
	adapter.mu.Unlock()
	values := url.Values{"state": {state}}
	if forceRecent {
		values.Set("max_age", "300")
	}
	return "https://issuer.fixture/authorize?" + values.Encode(), nil
}

func (adapter *oidcFixtureAdapter) Exchange(_ context.Context, _ identity.OIDCProviderRuntime,
	code, verifier string) (identity.OIDCClaims, error) {
	adapter.mu.Lock()
	nonce, found := adapter.nonces[verifier]
	adapter.mu.Unlock()
	if !found {
		return identity.OIDCClaims{}, identity.ErrOIDCProof
	}
	if code == "wrong-nonce" {
		nonce = "different-nonce"
	}
	subject := adapter.subjects[code]
	if subject == "" {
		subject = "subject-one"
	}
	claims := identity.OIDCClaims{Subject: subject, DisplayName: "OIDC 测试用户",
		Email: "shared@example.test", EmailVerified: true, Nonce: nonce}
	if adapter.recent[code] {
		now := time.Now().UTC()
		claims.AuthTime = &now
	}
	return claims, nil
}

func newOIDCIdentity(t *testing.T) (*identity.Module, *oidcFixtureAdapter) {
	t.Helper()
	base, database := newIdentity(t)
	initializeFixture(t, base)
	if _, err := base.ConfigureProvider(context.Background(), identity.ConfigureProviderCommand{
		ID: "fixture", DisplayName: "Fixture OIDC", Issuer: "https://issuer.fixture", ClientID: "orbit-client",
		ClientSecretRef: "fixture-secret", Enabled: true, MaintenanceRef: "fixture-provider",
	}); err != nil {
		t.Fatalf("configure fixture provider: %v", err)
	}
	adapter := newOIDCFixtureAdapter()
	module, err := identity.New(database, projectauth.NewOwnershipGuard(), identity.WithOIDC(identity.OIDCConfig{
		Adapter: adapter, ClientSecrets: map[string]string{"fixture-secret": "fixture-value"},
		RedirectURL: "https://orbit.example.test/api/v1/auth/oidc/callback", EncryptionKey: bytes.Repeat([]byte{7}, 32),
	}))
	if err != nil {
		t.Fatalf("construct OIDC fixture: %v", err)
	}
	return module, adapter
}

func TestOIDCAdmissionStateBrowserBindingAndNoEmailMerge(t *testing.T) {
	t.Parallel()
	module, _ := newOIDCIdentity(t)
	ctx := context.Background()
	started, state := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	if _, err := module.CompleteOIDC(ctx, state, "not-the-browser-token", "pending"); !errors.Is(err, identity.ErrOIDCTransaction) {
		t.Fatalf("wrong browser binding was accepted: %v", err)
	}
	completion, err := module.CompleteOIDC(ctx, state, started.BrowserToken.CookieValue(), "pending")
	if err != nil || completion.PendingIdentity == nil || completion.CurrentUser != nil {
		t.Fatalf("first external identity must remain pending: %#v, %v", completion, err)
	}
	firstIdentity := completion.PendingIdentity.ID
	if _, err := module.CompleteOIDC(ctx, state, started.BrowserToken.CookieValue(), "pending"); !errors.Is(err, identity.ErrOIDCTransaction) {
		t.Fatalf("consumed state was replayed: %v", err)
	}
	pendingCaller, err := module.ResolveSession(ctx, completion.Token.CookieValue())
	if err != nil || pendingCaller.UserID().String() != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("pending session became a user: %v", err)
	}
	principal, err := module.CurrentPrincipal(ctx, pendingCaller)
	if err != nil || principal.Kind != "pending" || principal.ExternalIdentity == nil {
		t.Fatalf("pending principal projection: %#v, %v", principal, err)
	}

	admin := resolveFixture(t, module, loginFixture(t, module))
	rejected, err := module.RejectAdmission(ctx, admin, firstIdentity)
	if err != nil || rejected.Status != "rejected" {
		t.Fatalf("reject admission: %#v, %v", rejected, err)
	}
	if _, err := module.ResolveSession(ctx, completion.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("rejection retained pending session: %v", err)
	}
	rejectedStart, rejectedState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	if _, err := module.CompleteOIDC(ctx, rejectedState, rejectedStart.BrowserToken.CookieValue(), "pending"); !errors.Is(err, identity.ErrAdmissionRejected) {
		t.Fatalf("new login erased rejection: %v", err)
	}
	if _, err := module.ReopenAdmission(ctx, admin, firstIdentity); err != nil {
		t.Fatalf("reopen admission: %v", err)
	}
	reopenedStart, reopenedState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	reopened, err := module.CompleteOIDC(ctx, reopenedState, reopenedStart.BrowserToken.CookieValue(), "pending")
	if err != nil || reopened.PendingIdentity == nil || reopened.PendingIdentity.ID != firstIdentity {
		t.Fatalf("reopen created another external identity: %#v, %v", reopened, err)
	}
	admitted, err := module.ApproveAdmission(ctx, admin, firstIdentity)
	if err != nil {
		t.Fatalf("approve admission: %v", err)
	}
	if _, err := module.ResolveSession(ctx, reopened.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("approval retained restricted session: %v", err)
	}
	replayed, err := module.ApproveAdmission(ctx, admin, firstIdentity)
	if err != nil || replayed.ID != admitted.ID {
		t.Fatalf("approval was not naturally idempotent: %#v, %v", replayed, err)
	}
	linkedStart, linkedState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	linked, err := module.CompleteOIDC(ctx, linkedState, linkedStart.BrowserToken.CookieValue(), "pending")
	if err != nil || linked.CurrentUser == nil || linked.CurrentUser.User.ID != admitted.ID {
		t.Fatalf("linked identity did not login as admitted user: %#v, %v", linked, err)
	}
	linkedCaller, err := module.ResolveSession(ctx, linked.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve linked OIDC session: %v", err)
	}
	if err := module.ChangePassword(ctx, identity.ChangePasswordCommand{Caller: linkedCaller,
		LoginName: "oidc-local", NewPassword: fixturePassword + "-oidc", SourceIP: "127.0.0.3"}); !errors.Is(err, identity.ErrRecentAuthenticationRequired) {
		t.Fatalf("ordinary OIDC login without auth_time established a local credential: %v", err)
	}
	reauthStart, reauthState := startOIDCFixture(t, module, identity.OIDCStartCommand{
		ProviderID: "fixture", Mode: "reauth", Caller: linkedCaller,
	})
	reauthenticated, err := module.CompleteOIDC(ctx, reauthState, reauthStart.BrowserToken.CookieValue(), "reauth-one")
	if err != nil {
		t.Fatalf("reauthenticate admitted OIDC-only user: %v", err)
	}
	recentCaller, err := module.ResolveSession(ctx, reauthenticated.Token.CookieValue())
	if err != nil {
		t.Fatalf("resolve recent OIDC session: %v", err)
	}
	if err := module.ChangePassword(ctx, identity.ChangePasswordCommand{Caller: recentCaller,
		LoginName: "oidc-local", NewPassword: fixturePassword + "-oidc", SourceIP: "127.0.0.3"}); err != nil {
		t.Fatalf("recent OIDC proof could not establish first local credential: %v", err)
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "oidc-local",
		Password: fixturePassword + "-oidc", SourceIP: "127.0.0.3"}); err != nil {
		t.Fatalf("first local credential cannot login: %v", err)
	}

	secondStart, secondState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	second, err := module.CompleteOIDC(ctx, secondState, secondStart.BrowserToken.CookieValue(), "same-email")
	if err != nil || second.PendingIdentity == nil || second.PendingIdentity.ID == firstIdentity {
		t.Fatalf("same email was merged instead of creating a distinct pending subject: %#v, %v", second, err)
	}
}

func TestAdmissionStatusViewsAndDetailFollowAuthoritativeState(t *testing.T) {
	t.Parallel()
	module, _ := newOIDCIdentity(t)
	ctx := context.Background()
	admin := resolveFixture(t, module, loginFixture(t, module))
	firstStart, firstState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	first, err := module.CompleteOIDC(ctx, firstState, firstStart.BrowserToken.CookieValue(), "pending")
	if err != nil {
		t.Fatal(err)
	}
	secondStart, secondState := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	if _, err := module.CompleteOIDC(ctx, secondState, secondStart.BrowserToken.CookieValue(), "same-email"); err != nil {
		t.Fatal(err)
	}
	id := first.PendingIdentity.ID
	pending, err := module.ListAdmissionsByStatus(ctx, admin, 1, "", "pending")
	if err != nil || len(pending.Items) != 1 || pending.NextCursor == nil {
		t.Fatalf("pending page: %+v, %v", pending, err)
	}
	if _, err := module.ListAdmissionsByStatus(ctx, admin, 1, *pending.NextCursor, "rejected"); !errors.Is(err, identity.ErrInvalidCursor) {
		t.Fatalf("cross-status cursor accepted: %v", err)
	}
	item, err := module.GetAdmission(ctx, admin, id)
	if err != nil || item.Status != "pending" {
		t.Fatalf("pending detail: %+v, %v", item, err)
	}
	if _, err := module.RejectAdmission(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	rejected, err := module.ListAdmissionsByStatus(ctx, admin, 20, "", "rejected")
	if err != nil || len(rejected.Items) != 1 || rejected.Items[0].ID != id {
		t.Fatalf("rejected view: %+v, %v", rejected, err)
	}
	if _, err := module.ReopenAdmission(ctx, admin, id); err != nil {
		t.Fatal(err)
	}
	user, err := module.ApproveAdmission(ctx, admin, id)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := module.ListAdmissionsByStatus(ctx, admin, 20, "", "linked")
	if err != nil || len(linked.Items) != 1 || linked.Items[0].UserID == nil || *linked.Items[0].UserID != user.ID {
		t.Fatalf("linked view: %+v, %v", linked, err)
	}
	item, err = module.GetAdmission(ctx, admin, id)
	if err != nil || item.Status != "linked" || item.UserID == nil || *item.UserID != user.ID {
		t.Fatalf("linked detail: %+v, %v", item, err)
	}
	defaultPage, err := module.ListAdmissions(ctx, admin, 20, "")
	if err != nil || len(defaultPage.Items) != 1 || defaultPage.Items[0].ID == id {
		t.Fatalf("default view leaked linked identity: %+v, %v", defaultPage, err)
	}
}

func TestOIDCNonceRecentProofBindingAndFirstLocalCredential(t *testing.T) {
	t.Parallel()
	module, _ := newOIDCIdentity(t)
	ctx := context.Background()
	started, state := startOIDCFixture(t, module, identity.OIDCStartCommand{ProviderID: "fixture", Mode: "login"})
	if _, err := module.CompleteOIDC(ctx, state, started.BrowserToken.CookieValue(), "wrong-nonce"); !errors.Is(err, identity.ErrOIDCProof) {
		t.Fatalf("wrong nonce was accepted: %v", err)
	}
	if _, err := module.CompleteOIDC(ctx, state, started.BrowserToken.CookieValue(), "pending"); !errors.Is(err, identity.ErrOIDCTransaction) {
		t.Fatalf("failed proof did not consume one-time state: %v", err)
	}

	admin := resolveFixture(t, module, loginFixture(t, module))
	bindStart, bindState := startOIDCFixture(t, module, identity.OIDCStartCommand{
		ProviderID: "fixture", Mode: "bind", Caller: admin,
	})
	bound, err := module.CompleteOIDC(ctx, bindState, bindStart.BrowserToken.CookieValue(), "bound")
	if err != nil || !bound.RequiresRelogin {
		t.Fatalf("bilateral binding did not revoke original proof: %#v, %v", bound, err)
	}
	if _, err := module.CurrentUser(ctx, admin); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("binding retained original Caller: %v", err)
	}
	admin = resolveFixture(t, module, loginFixture(t, module))
	reauthStart, reauthState := startOIDCFixture(t, module, identity.OIDCStartCommand{
		ProviderID: "fixture", Mode: "reauth", Caller: admin,
	})
	reauthenticated, err := module.CompleteOIDC(ctx, reauthState, reauthStart.BrowserToken.CookieValue(), "reauth")
	if err != nil || reauthenticated.CurrentUser == nil || reauthenticated.Token.CookieValue() == "" {
		t.Fatalf("OIDC reauthentication did not rotate the session: %#v, %v", reauthenticated, err)
	}
	if _, err := module.CurrentUser(ctx, admin); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("OIDC reauthentication retained original Caller: %v", err)
	}
}

func startOIDCFixture(t *testing.T, module *identity.Module, command identity.OIDCStartCommand) (identity.OIDCStart, string) {
	t.Helper()
	started, err := module.StartOIDC(context.Background(), command)
	if err != nil {
		t.Fatalf("start OIDC fixture: %v", err)
	}
	location, err := url.Parse(started.AuthorizationURL)
	if err != nil || location.Query().Get("state") == "" {
		t.Fatal("OIDC fixture did not receive state")
	}
	return started, location.Query().Get("state")
}
