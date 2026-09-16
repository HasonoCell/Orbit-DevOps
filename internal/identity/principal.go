package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
)

type CurrentPrincipal struct {
	Kind             string
	User             *CurrentUser
	ExternalIdentity *ExternalIdentity
}

type SessionSummary struct {
	ID         uuid.UUID
	Method     string
	ProviderID *string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	Current    bool
}

type SessionPage struct {
	Items      []SessionSummary
	NextCursor *string
}

type ExternalIdentityPage struct {
	Items      []ExternalIdentity
	NextCursor *string
}

// CurrentPrincipal 是正式 User 与待准入主体共享的最小本人投影；pending 永远不能进入业务授权。
func (m *Module) CurrentPrincipal(ctx context.Context, caller Caller) (CurrentPrincipal, error) {
	if caller.userID != uuid.Nil {
		current, err := m.CurrentUser(ctx, caller)
		if err != nil {
			return CurrentPrincipal{}, err
		}
		return CurrentPrincipal{Kind: "user", User: &current}, nil
	}
	if caller.externalIdentityID == uuid.Nil || caller.sessionID == uuid.Nil {
		return CurrentPrincipal{}, ErrUnauthenticated
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return CurrentPrincipal{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var identityRecord ExternalIdentity
	if err := tx.GetContext(ctx, &identityRecord, `SELECT external_identities.id,external_identities.provider_id,
		external_identities.subject,external_identities.status,external_identities.user_id,external_identities.display_name,
		external_identities.email,external_identities.email_verified,external_identities.created_at,external_identities.updated_at
		FROM external_identities JOIN auth_sessions s ON s.external_identity_id=external_identities.id
		JOIN auth_providers ap ON ap.id=external_identities.provider_id
		WHERE external_identities.id=$1 AND s.id=$2 AND external_identities.status='pending' AND ap.enabled
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND s.last_seen_at>clock_timestamp()-interval '30 minutes'`, caller.externalIdentityID, caller.sessionID); err != nil {
		return CurrentPrincipal{}, authenticationQueryError(err)
	}
	if err := tx.Commit(); err != nil {
		return CurrentPrincipal{}, dependencyError(err)
	}
	return CurrentPrincipal{Kind: "pending", ExternalIdentity: &identityRecord}, nil
}

func (m *Module) ListSessions(ctx context.Context, caller Caller, limit int, cursorValue string) (SessionPage, error) {
	if limit < 1 || limit > 100 {
		return SessionPage{}, ErrInvalidCursor
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return SessionPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return SessionPage{}, err
	}
	if current.MustChangePassword {
		return SessionPage{}, ErrPasswordChangeRequired
	}
	var cursor *identityCursor
	if cursorValue != "" {
		decoded, err := decodeIdentityCursor(cursorValue, "sessions", current.ID)
		if err != nil {
			return SessionPage{}, err
		}
		cursor = &decoded
	}
	type row struct {
		ID         uuid.UUID      `db:"id"`
		Method     string         `db:"auth_method"`
		ProviderID sql.NullString `db:"provider_id"`
		CreatedAt  time.Time      `db:"created_at"`
		LastSeenAt time.Time      `db:"last_seen_at"`
		ExpiresAt  time.Time      `db:"expires_at"`
	}
	rows := make([]row, 0, limit+1)
	query := `SELECT s.id,s.auth_method,s.provider_id,s.created_at,s.last_seen_at,s.expires_at
		FROM auth_sessions s LEFT JOIN external_identities ei ON ei.id=s.external_identity_id
		WHERE (s.user_id=$1 OR ei.user_id=$1) AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND s.last_seen_at>clock_timestamp()-interval '30 minutes'`
	args := []any{current.ID, limit + 1}
	if cursor == nil {
		query += ` ORDER BY s.created_at DESC,s.id DESC LIMIT $2`
	} else {
		query += ` AND (s.created_at,s.id)<($2,$3) ORDER BY s.created_at DESC,s.id DESC LIMIT $4`
		args = []any{current.ID, cursor.CreatedAt, cursor.ID, limit + 1}
	}
	if err := tx.SelectContext(ctx, &rows, query, args...); err != nil {
		return SessionPage{}, dependencyError(err)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	result := make([]SessionSummary, 0, len(rows))
	for _, item := range rows {
		var providerID *string
		if item.ProviderID.Valid {
			value := item.ProviderID.String
			providerID = &value
		}
		result = append(result, SessionSummary{ID: item.ID, Method: item.Method, ProviderID: providerID,
			CreatedAt: item.CreatedAt, LastSeenAt: item.LastSeenAt, ExpiresAt: item.ExpiresAt, Current: item.ID == caller.sessionID})
	}
	if err := tx.Commit(); err != nil {
		return SessionPage{}, dependencyError(err)
	}
	page := SessionPage{Items: result}
	if hasMore {
		last := rows[len(rows)-1]
		value := encodeIdentityCursor(identityCursor{Scope: "sessions", SubjectID: current.ID, CreatedAt: last.CreatedAt, ID: last.ID})
		page.NextCursor = &value
	}
	return page, nil
}

func (m *Module) ListExternalIdentities(ctx context.Context, caller Caller, limit int, cursorValue string) (ExternalIdentityPage, error) {
	if limit < 1 || limit > 100 {
		return ExternalIdentityPage{}, ErrInvalidCursor
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return ExternalIdentityPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return ExternalIdentityPage{}, err
	}
	if current.MustChangePassword {
		return ExternalIdentityPage{}, ErrPasswordChangeRequired
	}
	var cursor *identityCursor
	if cursorValue != "" {
		decoded, err := decodeIdentityCursor(cursorValue, "external-identities", current.ID)
		if err != nil {
			return ExternalIdentityPage{}, err
		}
		cursor = &decoded
	}
	items := make([]ExternalIdentity, 0, limit+1)
	query := externalIdentitySelect + ` WHERE user_id=$1 AND status='linked'`
	args := []any{current.ID, limit + 1}
	if cursor == nil {
		query += ` ORDER BY created_at DESC,id DESC LIMIT $2`
	} else {
		query += ` AND (created_at,id)<($2,$3) ORDER BY created_at DESC,id DESC LIMIT $4`
		args = []any{current.ID, cursor.CreatedAt, cursor.ID, limit + 1}
	}
	if err := tx.SelectContext(ctx, &items, query, args...); err != nil {
		return ExternalIdentityPage{}, dependencyError(err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if err := tx.Commit(); err != nil {
		return ExternalIdentityPage{}, dependencyError(err)
	}
	page := ExternalIdentityPage{Items: items}
	if hasMore {
		last := items[len(items)-1]
		value := encodeIdentityCursor(identityCursor{Scope: "external-identities", SubjectID: current.ID, CreatedAt: last.CreatedAt, ID: last.ID})
		page.NextCursor = &value
	}
	return page, nil
}

func (m *Module) UnbindExternalIdentity(ctx context.Context, caller Caller, identityID uuid.UUID) error {
	if identityID == uuid.Nil {
		return ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := loadCaller(ctx, tx, caller)
	if err != nil {
		return err
	}
	if current.MustChangePassword {
		return ErrPasswordChangeRequired
	}
	if err := requireRecentAuthentication(ctx, tx, current); err != nil {
		return err
	}
	var identityRecord ExternalIdentity
	if err := tx.GetContext(ctx, &identityRecord, externalIdentitySelect+` WHERE id=$1 AND user_id=$2 AND status='linked' FOR UPDATE`, identityID, current.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		return dependencyError(err)
	}
	var hasOther bool
	if err := tx.GetContext(ctx, &hasOther, `SELECT
		EXISTS(SELECT 1 FROM local_credentials WHERE user_id=$1)
		OR EXISTS(SELECT 1 FROM external_identities ei JOIN auth_providers ap ON ap.id=ei.provider_id
		 WHERE ei.user_id=$1 AND ei.status='linked' AND ei.id<>$2 AND ap.enabled)`, current.ID, identityID); err != nil {
		return dependencyError(err)
	}
	if !hasOther {
		return ErrLastLoginMethod
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET status='rejected',user_id=NULL,
		decision_by=$2,decision_at=$3,updated_at=$3 WHERE id=$1`, identityID, current.ID.String(), now); err != nil {
		return dependencyError(err)
	}
	if err := advanceAndRevoke(ctx, tx, current.ID, now); err != nil {
		return err
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: current.ID.String(), Action: "user.external_identity.unbind",
		TargetType: "external_identity", TargetID: identityID,
		Summary: map[string]any{"providerId": identityRecord.ProviderID}, CreatedAt: now}); err != nil {
		return dependencyError(err)
	}
	return dependencyErrorOrNil(tx.Commit())
}
