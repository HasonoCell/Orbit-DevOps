package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestCoreOSOIDCAdapterControlledIssuer 使用真实 discovery/JWKS/RS256 Token 验证受信任 issuer 边界。
func TestCoreOSOIDCAdapterControlledIssuer(t *testing.T) {
	t.Parallel()
	issuer := newControlledIssuer(t)
	runtime := OIDCProviderRuntime{ID: "fixture", Issuer: issuer.server.URL, ClientID: "orbit-client",
		ClientSecret: "fixture-secret", RedirectURL: "https://orbit.example.test/api/v1/auth/oidc/callback"}
	adapter := &CoreOSOIDCAdapter{client: issuer.server.Client()}

	claims, err := adapter.Exchange(context.Background(), runtime, "valid", issuer.verifier)
	if err != nil {
		t.Fatalf("verify controlled issuer token: %v", err)
	}
	if claims.Subject != "fixture-subject" || claims.Nonce != "fixture-nonce" || claims.AuthTime == nil {
		t.Fatalf("unexpected verified claims: %#v", claims)
	}
	for _, code := range []string{"wrong-issuer", "wrong-audience", "wrong-azp", "missing-azp", "expired"} {
		if _, err := adapter.Exchange(context.Background(), runtime, code, issuer.verifier); !errors.Is(err, ErrOIDCProof) {
			t.Fatalf("%s token was accepted: %v", code, err)
		}
	}
	if _, err := adapter.Exchange(context.Background(), runtime, "valid", "wrong-verifier"); !errors.Is(err, ErrOIDCProof) {
		t.Fatalf("wrong PKCE verifier was accepted: %v", err)
	}
}

func TestCoreOSOIDCAuthorizationURLCarriesIndependentProofs(t *testing.T) {
	t.Parallel()
	issuer := newControlledIssuer(t)
	runtime := OIDCProviderRuntime{ID: "fixture", Issuer: issuer.server.URL, ClientID: "orbit-client",
		ClientSecret: "fixture-secret", RedirectURL: "https://orbit.example.test/api/v1/auth/oidc/callback"}
	adapter := &CoreOSOIDCAdapter{client: issuer.server.Client()}
	location, err := adapter.AuthorizationURL(context.Background(), runtime, "fixture-state", "fixture-nonce", issuer.verifier, true)
	if err != nil {
		t.Fatalf("build authorization URL: %v", err)
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal("parse authorization URL")
	}
	query := parsed.Query()
	challenge := sha256.Sum256([]byte(issuer.verifier))
	if query.Get("state") != "fixture-state" || query.Get("nonce") != "fixture-nonce" ||
		query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) ||
		query.Get("max_age") != "300" || query.Get("prompt") != "login" {
		t.Fatalf("authorization URL omitted independent proofs: %s", location)
	}
}

type controlledIssuer struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	verifier string
}

func newControlledIssuer(t *testing.T) *controlledIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal("generate issuer key")
	}
	fixture := &controlledIssuer{key: key, verifier: strings.Repeat("v", 43)}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			writeIssuerJSON(writer, map[string]any{"issuer": fixture.server.URL,
				"authorization_endpoint": fixture.server.URL + "/authorize", "token_endpoint": fixture.server.URL + "/token",
				"jwks_uri": fixture.server.URL + "/jwks", "response_types_supported": []string{"code"},
				"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			writeIssuerJSON(writer, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "fixture-key",
				"use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": rsaExponent(key.PublicKey.E)}}})
		case "/token":
			if err := request.ParseForm(); err != nil || request.Form.Get("code_verifier") != fixture.verifier {
				http.Error(writer, "invalid grant", http.StatusBadRequest)
				return
			}
			claims := fixture.claims(request.Form.Get("code"))
			writeIssuerJSON(writer, map[string]any{"access_token": "fixture-access-token", "token_type": "Bearer",
				"expires_in": 300, "id_token": fixture.sign(t, claims)})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *controlledIssuer) claims(code string) map[string]any {
	now := time.Now().UTC()
	issuer, audience, authorizedParty, expiry := fixture.server.URL, any("orbit-client"), "orbit-client", now.Add(5*time.Minute)
	switch code {
	case "wrong-issuer":
		issuer = "https://different-issuer.example.test"
	case "wrong-audience":
		audience = "another-client"
	case "wrong-azp":
		audience, authorizedParty = []string{"orbit-client", "another-client"}, "another-client"
	case "missing-azp":
		audience, authorizedParty = []string{"orbit-client", "another-client"}, ""
	case "expired":
		expiry = now.Add(-time.Minute)
	}
	claims := map[string]any{"iss": issuer, "sub": "fixture-subject", "aud": audience, "exp": expiry.Unix(),
		"iat": now.Unix(), "nonce": "fixture-nonce", "auth_time": now.Unix(), "name": "Fixture User",
		"email": "fixture@example.test", "email_verified": true}
	if authorizedParty != "" {
		claims["azp"] = authorizedParty
	}
	return claims
}

func (fixture *controlledIssuer) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "fixture-key", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := fixture.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		t.Fatal("sign controlled ID token")
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func rsaExponent(value int) string {
	return base64.RawURLEncoding.EncodeToString(big.NewInt(int64(value)).Bytes())
}

func writeIssuerJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
