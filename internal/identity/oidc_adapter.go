package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var ErrOIDCUnavailable = errors.New("OIDC provider unavailable")
var ErrOIDCProof = errors.New("OIDC proof invalid")

type OIDCProviderRuntime struct {
	ID           string
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type OIDCClaims struct {
	Subject       string
	DisplayName   string
	Email         string
	EmailVerified bool
	Nonce         string
	AuthTime      *time.Time
}

// OIDCAdapter 把外部 discovery/token/JWKS 网络隔离在数据库事务之外。
type OIDCAdapter interface {
	AuthorizationURL(context.Context, OIDCProviderRuntime, string, string, string, bool) (string, error)
	Exchange(context.Context, OIDCProviderRuntime, string, string) (OIDCClaims, error)
}

// CoreOSOIDCAdapter 使用 discovery 与远程 JWKS 完整验证签名、issuer、audience 和期限。
type CoreOSOIDCAdapter struct{ client *http.Client }

func NewCoreOSOIDCAdapter(timeout time.Duration) *CoreOSOIDCAdapter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &CoreOSOIDCAdapter{client: &http.Client{Timeout: timeout}}
}

func (a *CoreOSOIDCAdapter) AuthorizationURL(ctx context.Context, runtime OIDCProviderRuntime,
	state, nonce, verifier string, forceRecent bool) (string, error) {
	provider, config, err := a.provider(ctx, runtime)
	if err != nil {
		return "", err
	}
	_ = provider
	options := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)}
	if forceRecent {
		options = append(options, oauth2.SetAuthURLParam("max_age", "300"), oauth2.SetAuthURLParam("prompt", "login"))
	}
	return config.AuthCodeURL(state, options...), nil
}

func (a *CoreOSOIDCAdapter) Exchange(ctx context.Context, runtime OIDCProviderRuntime, code, verifier string) (OIDCClaims, error) {
	provider, config, err := a.provider(ctx, runtime)
	if err != nil {
		return OIDCClaims{}, err
	}
	token, err := config.Exchange(oidc.ClientContext(ctx, a.client), code, oauth2.VerifierOption(verifier))
	if err != nil {
		return OIDCClaims{}, ErrOIDCProof
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return OIDCClaims{}, ErrOIDCProof
	}
	idToken, err := provider.Verifier(&oidc.Config{ClientID: runtime.ClientID}).Verify(oidc.ClientContext(ctx, a.client), rawIDToken)
	if err != nil || strings.TrimSpace(idToken.Subject) == "" {
		return OIDCClaims{}, ErrOIDCProof
	}
	var raw struct {
		Name            string      `json:"name"`
		PreferredName   string      `json:"preferred_username"`
		Email           string      `json:"email"`
		EmailVerified   bool        `json:"email_verified"`
		AuthTime        json.Number `json:"auth_time"`
		AuthorizedParty string      `json:"azp"`
	}
	if err := idToken.Claims(&raw); err != nil {
		return OIDCClaims{}, ErrOIDCProof
	}
	// go-oidc 已校验 aud 包含 ClientID；多 audience 时仍必须核验 azp，且存在 azp 就不能指向其他客户端。
	if len(idToken.Audience) > 1 && raw.AuthorizedParty == "" ||
		raw.AuthorizedParty != "" && raw.AuthorizedParty != runtime.ClientID {
		return OIDCClaims{}, ErrOIDCProof
	}
	displayName := strings.TrimSpace(raw.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(raw.PreferredName)
	}
	if displayName == "" {
		displayName = "OIDC User"
	}
	claims := OIDCClaims{Subject: idToken.Subject, DisplayName: displayName, Email: strings.TrimSpace(raw.Email),
		EmailVerified: raw.EmailVerified, Nonce: idToken.Nonce}
	if raw.AuthTime != "" {
		seconds, parseErr := raw.AuthTime.Int64()
		if parseErr != nil || seconds <= 0 {
			return OIDCClaims{}, ErrOIDCProof
		}
		value := time.Unix(seconds, 0).UTC()
		claims.AuthTime = &value
	}
	return claims, nil
}

func (a *CoreOSOIDCAdapter) provider(ctx context.Context, runtime OIDCProviderRuntime) (*oidc.Provider, oauth2.Config, error) {
	if a == nil || a.client == nil || runtime.Issuer == "" || runtime.ClientID == "" || runtime.RedirectURL == "" {
		return nil, oauth2.Config{}, ErrOIDCUnavailable
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, a.client), runtime.Issuer)
	if err != nil {
		return nil, oauth2.Config{}, ErrOIDCUnavailable
	}
	return provider, oauth2.Config{ClientID: runtime.ClientID, ClientSecret: runtime.ClientSecret,
		RedirectURL: runtime.RedirectURL, Endpoint: provider.Endpoint(),
		Scopes: []string{oidc.ScopeOpenID, oidc.ScopeProfile, oidc.ScopeEmail}}, nil
}
