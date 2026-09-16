package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
)

const externalIdentitySelect = `SELECT id,provider_id,subject,status,user_id,display_name,email,email_verified,created_at,updated_at FROM external_identities`

func (m *Module) completeOIDCLogin(ctx context.Context, provider OIDCProvider, claims OIDCClaims) (OIDCCompletion, error) {
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return OIDCCompletion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := m.loadOIDCRuntime(ctx, tx, provider.ID); err != nil {
		return OIDCCompletion{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return OIDCCompletion{}, err
	}
	identityRecord, err := lockExternalIdentity(ctx, tx, provider.ID, claims.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		identityRecord = externalIdentityFromClaims(provider.ID, claims, "pending", now)
		if _, err := tx.ExecContext(ctx, `INSERT INTO external_identities
			(id,provider_id,subject,status,display_name,email,email_verified,created_at,updated_at)
			VALUES($1,$2,$3,'pending',$4,$5,$6,$7,$7)`, identityRecord.ID, provider.ID, claims.Subject,
			identityRecord.DisplayName, identityRecord.Email, identityRecord.EmailVerified, now); err != nil {
			return OIDCCompletion{}, dependencyError(err)
		}
	} else if err != nil {
		return OIDCCompletion{}, err
	} else {
		if identityRecord.Status == "rejected" {
			return OIDCCompletion{}, ErrAdmissionRejected
		}
		if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET display_name=$2,email=$3,email_verified=$4,updated_at=$5 WHERE id=$1`,
			identityRecord.ID, claims.DisplayName, nullableEmail(claims.Email), claims.EmailVerified, now); err != nil {
			return OIDCCompletion{}, dependencyError(err)
		}
		identityRecord.DisplayName, identityRecord.Email, identityRecord.EmailVerified, identityRecord.UpdatedAt = claims.DisplayName, nullableEmail(claims.Email), claims.EmailVerified, now
	}
	token, expiresAt, sessionID, err := m.insertOIDCSession(ctx, tx, identityRecord, claims.AuthTime, now, now.Add(sessionAbsoluteLifetime))
	if err != nil {
		return OIDCCompletion{}, err
	}
	completion := OIDCCompletion{Mode: "login", Token: token, ExpiresAt: expiresAt}
	if identityRecord.Status == "pending" {
		completion.PendingIdentity = &identityRecord
	} else {
		current, err := currentUserForExternal(ctx, tx, identityRecord)
		if err != nil {
			return OIDCCompletion{}, err
		}
		completion.CurrentUser = &current
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: externalActor(identityRecord.ID), Action: "auth.login",
		TargetType: "session", TargetID: sessionID,
		Summary: map[string]any{"method": "oidc", "providerId": provider.ID, "admission": identityRecord.Status}, CreatedAt: now}); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	return completion, nil
}

func (m *Module) completeOIDCBind(ctx context.Context, transaction oidcTransaction, provider OIDCProvider, claims OIDCClaims) (OIDCCompletion, error) {
	if !transaction.OriginalUserID.Valid || !transaction.OriginalSessionID.Valid {
		return OIDCCompletion{}, ErrOIDCTransaction
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return OIDCCompletion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadOriginalSession(ctx, tx, transaction.OriginalUserID.UUID, transaction.OriginalSessionID.UUID)
	if err != nil {
		return OIDCCompletion{}, err
	}
	if current.MustChangePassword {
		return OIDCCompletion{}, ErrPasswordChangeRequired
	}
	if err := requireRecentAuthentication(ctx, tx, current); err != nil {
		return OIDCCompletion{}, err
	}
	if err := recentOIDCClaim(ctx, tx, claims.AuthTime); err != nil {
		return OIDCCompletion{}, err
	}
	if _, _, err := m.loadOIDCRuntime(ctx, tx, provider.ID); err != nil {
		return OIDCCompletion{}, err
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return OIDCCompletion{}, err
	}
	identityRecord, err := lockExternalIdentity(ctx, tx, provider.ID, claims.Subject)
	if errors.Is(err, sql.ErrNoRows) {
		identityRecord = externalIdentityFromClaims(provider.ID, claims, "linked", now)
		identityRecord.UserID = &current.ID
		_, err = tx.ExecContext(ctx, `INSERT INTO external_identities
			(id,provider_id,subject,status,user_id,display_name,email,email_verified,decision_by,decision_at,created_at,updated_at)
			VALUES($1,$2,$3,'linked',$4,$5,$6,$7,$8,$9,$9,$9)`, identityRecord.ID, provider.ID, claims.Subject,
			current.ID, claims.DisplayName, nullableEmail(claims.Email), claims.EmailVerified, current.ID.String(), now)
	} else if err == nil {
		if identityRecord.Status == "rejected" {
			return OIDCCompletion{}, ErrAdmissionRejected
		}
		if identityRecord.Status == "linked" && (identityRecord.UserID == nil || *identityRecord.UserID != current.ID) {
			return OIDCCompletion{}, ErrIdentityAlreadyBound
		}
		_, err = tx.ExecContext(ctx, `UPDATE external_identities SET status='linked',user_id=$2,display_name=$3,email=$4,
			email_verified=$5,decision_by=$6,decision_at=$7,updated_at=$7 WHERE id=$1`, identityRecord.ID,
			current.ID, claims.DisplayName, nullableEmail(claims.Email), claims.EmailVerified, current.ID.String(), now)
	} else {
		return OIDCCompletion{}, err
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return OIDCCompletion{}, ErrIdentityAlreadyBound
		}
		return OIDCCompletion{}, fmt.Errorf("persist external identity binding: %w", dependencyError(err))
	}
	if err := advanceAndRevoke(ctx, tx, current.ID, now); err != nil {
		return OIDCCompletion{}, fmt.Errorf("revoke sessions after external identity binding: %w", err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(), Action: "user.external_identity.bind",
		TargetType: "external_identity", TargetID: identityRecord.ID,
		Summary: map[string]any{"providerId": provider.ID}, CreatedAt: now}); err != nil {
		return OIDCCompletion{}, fmt.Errorf("audit external identity binding: %w", dependencyError(err))
	}
	if err := tx.Commit(); err != nil {
		return OIDCCompletion{}, fmt.Errorf("commit external identity binding: %w", dependencyError(err))
	}
	return OIDCCompletion{Mode: "bind", RequiresRelogin: true}, nil
}

func (m *Module) completeOIDCReauthentication(ctx context.Context, transaction oidcTransaction, provider OIDCProvider, claims OIDCClaims) (OIDCCompletion, error) {
	if !transaction.OriginalUserID.Valid || !transaction.OriginalSessionID.Valid {
		return OIDCCompletion{}, ErrOIDCTransaction
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return OIDCCompletion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadOriginalSession(ctx, tx, transaction.OriginalUserID.UUID, transaction.OriginalSessionID.UUID)
	if err != nil {
		return OIDCCompletion{}, err
	}
	if current.MustChangePassword {
		return OIDCCompletion{}, ErrPasswordChangeRequired
	}
	if err := recentOIDCClaim(ctx, tx, claims.AuthTime); err != nil {
		return OIDCCompletion{}, err
	}
	identityRecord, err := lockExternalIdentity(ctx, tx, provider.ID, claims.Subject)
	if err != nil || identityRecord.Status != "linked" || identityRecord.UserID == nil || *identityRecord.UserID != current.ID {
		return OIDCCompletion{}, ErrOIDCProof
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return OIDCCompletion{}, err
	}
	if !current.ExpiresAt.After(now) {
		return OIDCCompletion{}, ErrUnauthenticated
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at=$1 WHERE id=$2`, now, current.SessionID); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	token, expiresAt, _, err := m.insertOIDCSession(ctx, tx, identityRecord, claims.AuthTime, now, current.ExpiresAt)
	if err != nil {
		return OIDCCompletion{}, err
	}
	public := current.public()
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(), Action: "auth.reauth",
		TargetType: "external_identity", TargetID: identityRecord.ID, Summary: map[string]any{"method": "oidc", "providerId": provider.ID}, CreatedAt: now}); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return OIDCCompletion{}, dependencyError(err)
	}
	return OIDCCompletion{Mode: "reauth", CurrentUser: &public, Token: token, ExpiresAt: expiresAt}, nil
}

func (m *Module) insertOIDCSession(ctx context.Context, tx *sqlx.Tx, identityRecord ExternalIdentity,
	primaryAuthenticatedAt *time.Time, now, expiresAt time.Time) (SessionToken, time.Time, uuid.UUID, error) {
	token, err := randomSessionToken()
	if err != nil {
		return SessionToken{}, time.Time{}, uuid.Nil, err
	}
	hash := sha256.Sum256([]byte(token.CookieValue()))
	sessionID := uuid.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions
		(id,token_hash,user_id,external_identity_id,provider_id,auth_version,auth_method,primary_authenticated_at,
		 created_at,last_seen_at,expires_at) VALUES($1,$2,NULL,$3,$4,NULL,'oidc',$5,$6,$6,$7)`,
		sessionID, hash[:], identityRecord.ID, identityRecord.ProviderID, primaryAuthenticatedAt, now, expiresAt); err != nil {
		return SessionToken{}, time.Time{}, uuid.Nil, dependencyError(err)
	}
	return token, expiresAt, sessionID, nil
}

func currentUserForExternal(ctx context.Context, tx *sqlx.Tx, identityRecord ExternalIdentity) (CurrentUser, error) {
	if identityRecord.UserID == nil {
		return CurrentUser{}, ErrAdmissionPending
	}
	var current currentRecord
	if err := tx.GetContext(ctx, &current, `SELECT u.id,u.display_name,u.status,u.platform_role,u.created_at,
		COALESCE(lc.must_change_password,false) must_change_password FROM users u
		LEFT JOIN local_credentials lc ON lc.user_id=u.id WHERE u.id=$1 AND u.status='active'`, *identityRecord.UserID); err != nil {
		return CurrentUser{}, authenticationQueryError(err)
	}
	return current.public(), nil
}

func loadOriginalSession(ctx context.Context, tx *sqlx.Tx, userID, sessionID uuid.UUID) (currentRecord, error) {
	var current currentRecord
	if err := tx.GetContext(ctx, &current, `SELECT u.id,u.display_name,u.status,u.platform_role,u.created_at,
		COALESCE(lc.must_change_password,false) must_change_password,s.id session_id,u.auth_version,
		s.primary_authenticated_at,s.expires_at
		FROM auth_sessions s LEFT JOIN external_identities ei ON ei.id=s.external_identity_id
		LEFT JOIN auth_providers ap ON ap.id=ei.provider_id
		JOIN users u ON u.id=COALESCE(s.user_id,ei.user_id) LEFT JOIN local_credentials lc ON lc.user_id=u.id
		WHERE s.id=$1 AND u.id=$2 AND u.status='active' AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND s.last_seen_at>clock_timestamp()-interval '30 minutes'
		AND ((s.auth_method='password' AND s.auth_version=u.auth_version)
		 OR (s.auth_method='oidc' AND ei.status='linked' AND ap.enabled))`, sessionID, userID); err != nil {
		return currentRecord{}, authenticationQueryError(err)
	}
	return current, nil
}

func lockExternalIdentity(ctx context.Context, tx *sqlx.Tx, providerID, subject string) (ExternalIdentity, error) {
	var identityRecord ExternalIdentity
	err := tx.GetContext(ctx, &identityRecord, externalIdentitySelect+` WHERE provider_id=$1 AND subject=$2 FOR UPDATE`, providerID, subject)
	return identityRecord, err
}

func externalIdentityFromClaims(providerID string, claims OIDCClaims, status string, now time.Time) ExternalIdentity {
	return ExternalIdentity{ID: uuid.New(), ProviderID: providerID, Subject: claims.Subject, Status: status,
		DisplayName: claims.DisplayName, Email: nullableEmail(claims.Email), EmailVerified: claims.EmailVerified,
		CreatedAt: now, UpdatedAt: now}
}

func nullableEmail(email string) *string {
	if email == "" {
		return nil
	}
	return &email
}
