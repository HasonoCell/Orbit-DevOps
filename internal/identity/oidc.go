package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var (
	ErrOIDCTransaction      = errors.New("OIDC transaction invalid")
	ErrAdmissionPending     = errors.New("external identity admission pending")
	ErrAdmissionRejected    = errors.New("external identity admission rejected")
	ErrIdentityAlreadyBound = errors.New("external identity already bound")
	ErrLastLoginMethod      = errors.New("user must retain a login method")
)

const oidcTransactionLifetime = 10 * time.Minute

type OIDCProvider struct {
	ID                    string    `db:"id" json:"id"`
	DisplayName           string    `db:"display_name" json:"displayName"`
	Issuer                string    `db:"issuer" json:"-"`
	AllowInsecureLoopback bool      `db:"allow_insecure_loopback" json:"-"`
	ClientID              string    `db:"client_id" json:"-"`
	SecretRef             string    `db:"client_secret_ref" json:"-"`
	Enabled               bool      `db:"enabled" json:"enabled"`
	CreatedAt             time.Time `db:"created_at" json:"createdAt"`
}

type ExternalIdentity struct {
	ID            uuid.UUID  `db:"id" json:"id"`
	ProviderID    string     `db:"provider_id" json:"providerId"`
	Subject       string     `db:"subject" json:"-"`
	Status        string     `db:"status" json:"status"`
	UserID        *uuid.UUID `db:"user_id" json:"userId,omitempty"`
	DisplayName   string     `db:"display_name" json:"displayName"`
	Email         *string    `db:"email" json:"email,omitempty"`
	EmailVerified bool       `db:"email_verified" json:"emailVerified"`
	CreatedAt     time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updatedAt"`
}

type OIDCStartCommand struct {
	ProviderID string
	Mode       string
	Caller     Caller
}

type OIDCStart struct {
	AuthorizationURL string
	BrowserToken     SessionToken
	ExpiresAt        time.Time
}

type OIDCCompletion struct {
	Mode            string
	CurrentUser     *CurrentUser
	PendingIdentity *ExternalIdentity
	Token           SessionToken
	ExpiresAt       time.Time
	RequiresRelogin bool
}

type oidcTransaction struct {
	ID                uuid.UUID     `db:"id"`
	ProviderID        string        `db:"provider_id"`
	Mode              string        `db:"mode"`
	OriginalUserID    uuid.NullUUID `db:"original_user_id"`
	OriginalSessionID uuid.NullUUID `db:"original_session_id"`
	NonceHash         []byte        `db:"nonce_hash"`
	VerifierCipher    []byte        `db:"verifier_ciphertext"`
	VerifierNonce     []byte        `db:"verifier_nonce"`
}

// ListOIDCProviders 只暴露登录入口摘要；Secret ref、issuer 与 client ID 不进入 HTTP DTO。
func (m *Module) ListOIDCProviders(ctx context.Context) ([]OIDCProvider, error) {
	providers := make([]OIDCProvider, 0, 1)
	if err := m.db.SelectContext(ctx, &providers, `SELECT id,display_name,issuer,allow_insecure_loopback,client_id,client_secret_ref,enabled,created_at
		FROM auth_providers WHERE enabled ORDER BY id`); err != nil {
		return nil, dependencyError(err)
	}
	return providers, nil
}

func (m *Module) OIDCProviderAvailable(provider OIDCProvider) bool {
	_, hasSecret := m.oidcSecrets[provider.SecretRef]
	return provider.Enabled && m.oidcAdapter != nil && m.oidcCipher != nil && hasSecret && m.oidcRedirectURL != ""
}

// StartOIDC 在短事务内固定受信任提供方和一次性证明；授权 URL 发现发生在提交后。
func (m *Module) StartOIDC(ctx context.Context, command OIDCStartCommand) (OIDCStart, error) {
	if command.Mode != "login" && command.Mode != "bind" && command.Mode != "reauth" {
		return OIDCStart{}, ErrInvalidCommand
	}
	if m.oidcAdapter == nil || m.oidcCipher == nil {
		return OIDCStart{}, ErrOIDCUnavailable
	}
	state, err := randomSessionToken()
	if err != nil {
		return OIDCStart{}, err
	}
	nonce, err := randomSessionToken()
	if err != nil {
		return OIDCStart{}, err
	}
	verifier, err := randomSessionToken()
	if err != nil {
		return OIDCStart{}, err
	}
	browser, err := randomSessionToken()
	if err != nil {
		return OIDCStart{}, err
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return OIDCStart{}, err
	}
	defer func() { _ = tx.Rollback() }()
	provider, runtime, err := m.loadOIDCRuntime(ctx, tx, command.ProviderID)
	if err != nil {
		return OIDCStart{}, err
	}
	var originalUser, originalSession any
	forceRecent := false
	if command.Mode != "login" {
		current, err := loadCaller(ctx, tx, command.Caller)
		if err != nil {
			return OIDCStart{}, err
		}
		if current.MustChangePassword {
			return OIDCStart{}, ErrPasswordChangeRequired
		}
		if command.Mode == "bind" {
			if err := requireRecentAuthentication(ctx, tx, current); err != nil {
				return OIDCStart{}, err
			}
		}
		originalUser, originalSession = current.ID, current.SessionID
		forceRecent = true
	}
	id := uuid.New()
	ciphertext, cipherNonce, err := m.encryptVerifier(id, provider.ID, command.Mode, verifier.CookieValue())
	if err != nil {
		return OIDCStart{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return OIDCStart{}, err
	}
	stateHash, browserHash, nonceHash := sha256.Sum256([]byte(state.CookieValue())), sha256.Sum256([]byte(browser.CookieValue())), sha256.Sum256([]byte(nonce.CookieValue()))
	if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_transactions
		(id,state_hash,browser_hash,nonce_hash,provider_id,mode,original_user_id,original_session_id,
		 verifier_ciphertext,verifier_nonce,created_at,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, stateHash[:], browserHash[:], nonceHash[:], provider.ID,
		command.Mode, originalUser, originalSession, ciphertext, cipherNonce, now, now.Add(oidcTransactionLifetime)); err != nil {
		return OIDCStart{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return OIDCStart{}, dependencyError(err)
	}
	url, err := m.oidcAdapter.AuthorizationURL(ctx, runtime, state.CookieValue(), nonce.CookieValue(), verifier.CookieValue(), forceRecent)
	if err != nil {
		return OIDCStart{}, err
	}
	return OIDCStart{AuthorizationURL: url, BrowserToken: browser, ExpiresAt: now.Add(oidcTransactionLifetime)}, nil
}

// CompleteOIDC 先一次性消费 state，再在事务外换码/验签，最后重新读取政策和身份事实。
func (m *Module) CompleteOIDC(ctx context.Context, state, browser, code string) (OIDCCompletion, error) {
	if len(state) != 43 || len(browser) != 43 || code == "" || len(code) > 4096 {
		return OIDCCompletion{}, ErrOIDCTransaction
	}
	stateHash, browserHash := sha256.Sum256([]byte(state)), sha256.Sum256([]byte(browser))
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var transaction oidcTransaction
	if err := tx.GetContext(ctx, &transaction, `UPDATE oidc_transactions SET consumed_at=clock_timestamp()
		WHERE state_hash=$1 AND browser_hash=$2 AND consumed_at IS NULL AND expires_at>clock_timestamp()
		RETURNING id,provider_id,mode,original_user_id,original_session_id,nonce_hash,verifier_ciphertext,verifier_nonce`, stateHash[:], browserHash[:]); err != nil {
		return OIDCCompletion{}, ErrOIDCTransaction
	}
	provider, runtime, err := m.loadOIDCRuntime(ctx, tx, transaction.ProviderID)
	if err != nil {
		return OIDCCompletion{}, err
	}
	verifier, err := m.decryptVerifier(transaction, provider.ID)
	if err != nil {
		return OIDCCompletion{}, err
	}
	if err := tx.Commit(); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	claims, err := m.oidcAdapter.Exchange(ctx, runtime, code, verifier)
	if err != nil {
		return OIDCCompletion{}, err
	}
	if !validOIDCClaims(claims) {
		return OIDCCompletion{}, ErrOIDCProof
	}
	nonceHash := sha256.Sum256([]byte(claims.Nonce))
	if claims.Nonce == "" || subtle.ConstantTimeCompare(nonceHash[:], transaction.NonceHash) != 1 {
		return OIDCCompletion{}, ErrOIDCProof
	}
	switch transaction.Mode {
	case "login":
		return m.completeOIDCLogin(ctx, provider, claims)
	case "bind":
		return m.completeOIDCBind(ctx, transaction, provider, claims)
	case "reauth":
		return m.completeOIDCReauthentication(ctx, transaction, provider, claims)
	default:
		return OIDCCompletion{}, ErrOIDCTransaction
	}
}

func validOIDCClaims(claims OIDCClaims) bool {
	subject, name, email := strings.TrimSpace(claims.Subject), strings.TrimSpace(claims.DisplayName), strings.TrimSpace(claims.Email)
	return subject == claims.Subject && len(subject) >= 1 && len(subject) <= 512 && utf8.ValidString(subject) &&
		name == claims.DisplayName && len(name) >= 1 && len(name) <= 256 && utf8.ValidString(name) &&
		len(email) <= 320 && utf8.ValidString(email) && len(claims.Nonce) <= 512
}

func (m *Module) loadOIDCRuntime(ctx context.Context, queryer sqlx.QueryerContext, providerID string) (OIDCProvider, OIDCProviderRuntime, error) {
	var provider OIDCProvider
	if err := sqlx.GetContext(ctx, queryer, &provider, `SELECT id,display_name,issuer,allow_insecure_loopback,client_id,client_secret_ref,enabled,created_at
		FROM auth_providers WHERE id=$1 AND enabled`, providerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OIDCProvider{}, OIDCProviderRuntime{}, ErrOIDCUnavailable
		}
		return OIDCProvider{}, OIDCProviderRuntime{}, dependencyError(err)
	}
	secret, ok := m.oidcSecrets[provider.SecretRef]
	if !ok || m.oidcAdapter == nil || m.oidcCipher == nil {
		return OIDCProvider{}, OIDCProviderRuntime{}, ErrOIDCUnavailable
	}
	return provider, OIDCProviderRuntime{ID: provider.ID, Issuer: provider.Issuer, ClientID: provider.ClientID,
		ClientSecret: secret, RedirectURL: m.oidcRedirectURL}, nil
}

func (m *Module) encryptVerifier(id uuid.UUID, providerID, mode, verifier string) ([]byte, []byte, error) {
	nonce := make([]byte, m.oidcCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, ErrUnavailable
	}
	aad := []byte(id.String() + "\x00" + providerID + "\x00" + mode)
	return m.oidcCipher.Seal(nil, nonce, []byte(verifier), aad), nonce, nil
}

func (m *Module) decryptVerifier(transaction oidcTransaction, providerID string) (string, error) {
	aad := []byte(transaction.ID.String() + "\x00" + providerID + "\x00" + transaction.Mode)
	plain, err := m.oidcCipher.Open(nil, transaction.VerifierNonce, transaction.VerifierCipher, aad)
	if err != nil {
		return "", ErrOIDCTransaction
	}
	defer clear(plain)
	return string(plain), nil
}

func recentOIDCClaim(ctx context.Context, tx *sqlx.Tx, value *time.Time) error {
	if value == nil {
		return ErrRecentAuthenticationRequired
	}
	var recent bool
	if err := tx.GetContext(ctx, &recent, `SELECT $1<=clock_timestamp() AND $1>clock_timestamp()-interval '5 minutes'`, *value); err != nil {
		return dependencyError(err)
	}
	if !recent {
		return ErrRecentAuthenticationRequired
	}
	return nil
}

func externalActor(id uuid.UUID) string { return "external:" + id.String() }
