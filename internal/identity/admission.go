package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
)

var ErrAdmissionState = errors.New("external identity admission state conflict")

type AdmissionPage struct {
	Items      []ExternalIdentity
	NextCursor *string
}

func (m *Module) ListAdmissions(ctx context.Context, caller Caller, limit int, cursorValue string) (AdmissionPage, error) {
	return m.ListAdmissionsByStatus(ctx, caller, limit, cursorValue, "")
}

// ListAdmissionsByStatus 的游标绑定筛选条件；默认仍保留原来的待处理/已拒绝视图。
func (m *Module) ListAdmissionsByStatus(ctx context.Context, caller Caller, limit int, cursorValue, status string) (AdmissionPage, error) {
	if limit < 1 || limit > 100 {
		return AdmissionPage{}, ErrInvalidCursor
	}
	if status != "" && status != "pending" && status != "rejected" && status != "linked" {
		return AdmissionPage{}, ErrInvalidCursor
	}
	scope := "admissions"
	if status != "" {
		scope += ":" + status
	}
	var cursor *identityCursor
	if cursorValue != "" {
		decoded, err := decodeIdentityCursor(cursorValue, scope, uuid.Nil)
		if err != nil {
			return AdmissionPage{}, err
		}
		cursor = &decoded
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return AdmissionPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := requireAdministrator(ctx, tx, caller, false); err != nil {
		return AdmissionPage{}, err
	}
	items := make([]ExternalIdentity, 0, limit+1)
	query := externalIdentitySelect + ` WHERE status IN ('pending','rejected')`
	args := make([]any, 0, 4)
	if status != "" {
		query = externalIdentitySelect + ` WHERE status=$1`
		args = append(args, status)
	}
	if cursor != nil {
		query += fmt.Sprintf(` AND (created_at,id)<($%d,$%d)`, len(args)+1, len(args)+2)
		args = append(args, cursor.CreatedAt, cursor.ID)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)
	if err := tx.SelectContext(ctx, &items, query, args...); err != nil {
		return AdmissionPage{}, dependencyError(err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	if err := tx.Commit(); err != nil {
		return AdmissionPage{}, dependencyError(err)
	}
	page := AdmissionPage{Items: items}
	if hasMore {
		last := items[len(items)-1]
		value := encodeIdentityCursor(identityCursor{Scope: scope, CreatedAt: last.CreatedAt, ID: last.ID})
		page.NextCursor = &value
	}
	return page, nil
}

// GetAdmission 是管理员详情投影，不要求近期认证；写命令仍单独重验近期证明。
func (m *Module) GetAdmission(ctx context.Context, caller Caller, identityID uuid.UUID) (ExternalIdentity, error) {
	if identityID == uuid.Nil {
		return ExternalIdentity{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, false)
	if err != nil {
		return ExternalIdentity{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := requireAdministrator(ctx, tx, caller, false); err != nil {
		return ExternalIdentity{}, err
	}
	var item ExternalIdentity
	if err := tx.GetContext(ctx, &item, externalIdentitySelect+` WHERE id=$1`, identityID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalIdentity{}, ErrUserNotFound
		}
		return ExternalIdentity{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return ExternalIdentity{}, dependencyError(err)
	}
	return item, nil
}

func (m *Module) ApproveAdmission(ctx context.Context, caller Caller, identityID uuid.UUID) (User, error) {
	if identityID == uuid.Nil {
		return User{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	admin, err := requireAdministrator(ctx, tx, caller, true)
	if err != nil {
		return User{}, err
	}
	identityRecord, err := externalIdentityByIDForUpdate(ctx, tx, identityID)
	if err != nil {
		return User{}, err
	}
	if identityRecord.Status == "linked" && identityRecord.UserID != nil {
		user, err := userForSecurityChange(ctx, tx, *identityRecord.UserID)
		if err != nil {
			return User{}, err
		}
		if err := tx.Commit(); err != nil {
			return User{}, dependencyError(err)
		}
		return user, nil
	}
	if identityRecord.Status != "pending" {
		return User{}, ErrAdmissionState
	}
	var providerEnabled bool
	if err := tx.GetContext(ctx, &providerEnabled, `SELECT enabled FROM auth_providers WHERE id=$1`, identityRecord.ProviderID); err != nil {
		return User{}, dependencyError(err)
	}
	if !providerEnabled {
		return User{}, ErrOIDCUnavailable
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return User{}, err
	}
	user := User{ID: uuid.New(), DisplayName: identityRecord.DisplayName, Status: "active", PlatformRole: RoleUser, CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users
		(id,display_name,status,platform_role,admitted_by,admitted_at,created_at,updated_at)
		VALUES($1,$2,'active','user',$3,$4,$4,$4)`, user.ID, user.DisplayName, admin.ID.String(), now); err != nil {
		return User{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET status='linked',user_id=$2,
		decision_by=$3,decision_at=$4,updated_at=$4 WHERE id=$1`, identityID, user.ID, admin.ID.String(), now); err != nil {
		return User{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at=$2 WHERE external_identity_id=$1 AND revoked_at IS NULL`, identityID, now); err != nil {
		return User{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: admin.ID.String(), Action: "auth.admission.approve",
		TargetType: "external_identity", TargetID: identityID,
		Summary: map[string]any{"providerId": identityRecord.ProviderID, "userId": user.ID.String()}, CreatedAt: now}); err != nil {
		return User{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, dependencyError(err)
	}
	return user, nil
}

func (m *Module) RejectAdmission(ctx context.Context, caller Caller, identityID uuid.UUID) (ExternalIdentity, error) {
	return m.changeAdmission(ctx, caller, identityID, "pending", "rejected", "auth.admission.reject")
}

func (m *Module) ReopenAdmission(ctx context.Context, caller Caller, identityID uuid.UUID) (ExternalIdentity, error) {
	return m.changeAdmission(ctx, caller, identityID, "rejected", "pending", "auth.admission.reopen")
}

func (m *Module) changeAdmission(ctx context.Context, caller Caller, identityID uuid.UUID, from, to, action string) (ExternalIdentity, error) {
	if identityID == uuid.Nil {
		return ExternalIdentity{}, ErrInvalidCommand
	}
	tx, _, err := m.beginControlled(ctx, true)
	if err != nil {
		return ExternalIdentity{}, err
	}
	defer func() { _ = tx.Rollback() }()
	admin, err := requireAdministrator(ctx, tx, caller, true)
	if err != nil {
		return ExternalIdentity{}, err
	}
	identityRecord, err := externalIdentityByIDForUpdate(ctx, tx, identityID)
	if err != nil {
		return ExternalIdentity{}, err
	}
	if identityRecord.Status == to {
		if err := tx.Commit(); err != nil {
			return ExternalIdentity{}, dependencyError(err)
		}
		return identityRecord, nil
	}
	if identityRecord.Status != from {
		return ExternalIdentity{}, ErrAdmissionState
	}
	now, err := databaseNow(ctx, tx)
	if err != nil {
		return ExternalIdentity{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE external_identities SET status=$2,user_id=NULL,
		decision_by=$3,decision_at=$4,updated_at=$4 WHERE id=$1`, identityID, to, admin.ID.String(), now); err != nil {
		return ExternalIdentity{}, dependencyError(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at=$2 WHERE external_identity_id=$1 AND revoked_at IS NULL`, identityID, now); err != nil {
		return ExternalIdentity{}, dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: admin.ID.String(), Action: action,
		TargetType: "external_identity", TargetID: identityID,
		Summary: map[string]any{"providerId": identityRecord.ProviderID, "status": to}, CreatedAt: now}); err != nil {
		return ExternalIdentity{}, dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return ExternalIdentity{}, dependencyError(err)
	}
	identityRecord.Status, identityRecord.UpdatedAt = to, now
	return identityRecord, nil
}

func externalIdentityByIDForUpdate(ctx context.Context, tx interface {
	GetContext(context.Context, any, string, ...any) error
}, identityID uuid.UUID) (ExternalIdentity, error) {
	var identityRecord ExternalIdentity
	if err := tx.GetContext(ctx, &identityRecord, externalIdentitySelect+` WHERE id=$1 FOR UPDATE`, identityID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExternalIdentity{}, ErrUserNotFound
		}
		return ExternalIdentity{}, dependencyError(err)
	}
	return identityRecord, nil
}
