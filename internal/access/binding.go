package access

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	ErrInvalidSecret   = errors.New("invalid existing TLS Secret binding")
	ErrBindingNotFound = errors.New("TLS Secret binding not found")
)

type SecretBinding struct {
	ID         uuid.UUID `db:"id" json:"id"`
	ProjectID  uuid.UUID `db:"project_id" json:"projectId"`
	ClusterRef string    `db:"cluster_ref" json:"clusterRef"`
	Namespace  string    `db:"namespace" json:"namespace"`
	Hostname   string    `db:"hostname" json:"hostname"`
	SecretName string    `db:"secret_name" json:"secretName"`
	State      string    `db:"state" json:"state"`
	CreatedAt  time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt  time.Time `db:"updated_at" json:"updatedAt"`
}

type RegisterSecretCommand struct {
	ProjectID      uuid.UUID
	Hostname       string
	SecretName     string
	Caller         identity.Caller
	IdempotencyKey string
}

type RevokeSecretCommand struct {
	BindingID      uuid.UUID
	Caller         identity.Caller
	IdempotencyKey string
}

const bindingColumns = `id,project_id,cluster_ref,namespace,hostname,secret_name,state,created_at,updated_at`

func (m *Module) requireAdmin(ctx context.Context, tx *sqlx.Tx, caller identity.Caller) error {
	if m.identities == nil {
		return identity.ErrUnavailable
	}
	return m.identities.RequirePlatformAdminInTx(ctx, tx, caller)
}

// RegisterSecret 先鉴权再出站检查 Secret，最终仍在写事务内重新核验管理员身份。
func (m *Module) RegisterSecret(ctx context.Context, command RegisterSecretCommand) (SecretBinding, error) {
	hostname, err := NormalizeHostname(command.Hostname)
	if err != nil || command.ProjectID == uuid.Nil || len(validation.IsDNS1123Subdomain(command.SecretName)) > 0 {
		return SecretBinding{}, ErrInvalidSecret
	}
	if m.secretVerifier == nil {
		return SecretBinding{}, identity.ErrUnavailable
	}
	preflight, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return SecretBinding{}, err
	}
	if err := m.requireAdmin(ctx, preflight, command.Caller); err != nil {
		_ = preflight.Rollback()
		return SecretBinding{}, err
	}
	if err := preflight.Commit(); err != nil {
		return SecretBinding{}, err
	}
	if err := m.secretVerifier.VerifyTLSSecret(ctx, m.config.Namespace, command.SecretName, hostname); err != nil {
		return SecretBinding{}, ErrInvalidSecret
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID            uuid.UUID
		Hostname, SecretName string
	}{command.ProjectID, hostname, command.SecretName})
	if err != nil {
		return SecretBinding{}, err
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return SecretBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.requireAdmin(ctx, tx, command.Caller); err != nil {
		return SecretBinding{}, err
	}
	var projectExists bool
	if err := tx.GetContext(ctx, &projectExists, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=$1)`, command.ProjectID); err != nil {
		return SecretBinding{}, err
	}
	if !projectExists {
		return SecretBinding{}, ErrBindingNotFound
	}
	now := time.Now().UTC()
	binding := SecretBinding{ID: uuid.New(), ProjectID: command.ProjectID, ClusterRef: m.config.ClusterRef,
		Namespace: m.config.Namespace, Hostname: hostname, SecretName: command.SecretName,
		State: "active", CreatedAt: now, UpdatedAt: now}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_secret_binding.register", Key: command.IdempotencyKey}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, hash, binding.ID, now)
	if err != nil {
		return SecretBinding{}, err
	}
	if !isNew {
		if err := idempotency.LoadResponse(ctx, tx, scope, &binding); err != nil {
			return SecretBinding{}, err
		}
		return binding, tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_secret_bindings
		(id,project_id,cluster_ref,namespace,hostname,secret_name,state,created_by,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,'active',$7,$8,$8)`, binding.ID, binding.ProjectID,
		binding.ClusterRef, binding.Namespace, binding.Hostname, binding.SecretName, command.Caller.UserID(), now)
	if err != nil {
		return SecretBinding{}, conflict(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_secret_binding.register",
		TargetType: "access_secret_binding", TargetID: binding.ID, Summary: map[string]string{
			"projectId": binding.ProjectID.String(), "hostname": binding.Hostname, "secretName": binding.SecretName}, CreatedAt: now}); err != nil {
		return SecretBinding{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, binding); err != nil {
		return SecretBinding{}, err
	}
	return binding, tx.Commit()
}

func (m *Module) ListSecretBindings(ctx context.Context, caller identity.Caller) ([]SecretBinding, error) {
	result := make([]SecretBinding, 0)
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.requireAdmin(ctx, tx, caller); err != nil {
		return nil, err
	}
	if err := tx.SelectContext(ctx, &result, `SELECT `+bindingColumns+` FROM access_secret_bindings
		ORDER BY created_at,id LIMIT 100`); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

// RevokeSecret 只撤销授权。若 Host 正在引用它，唤醒调和移除 listener；绝不删除外部 Secret。
func (m *Module) RevokeSecret(ctx context.Context, command RevokeSecretCommand) (SecretBinding, error) {
	hash, err := idempotency.Fingerprint(struct{ BindingID uuid.UUID }{command.BindingID})
	if err != nil {
		return SecretBinding{}, err
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return SecretBinding{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.requireAdmin(ctx, tx, command.Caller); err != nil {
		return SecretBinding{}, err
	}
	var binding SecretBinding
	if err := tx.GetContext(ctx, &binding, `SELECT `+bindingColumns+` FROM access_secret_bindings WHERE id=$1`, command.BindingID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SecretBinding{}, ErrBindingNotFound
		}
		return SecretBinding{}, err
	}
	if err := m.lockSync(ctx, tx, binding.ProjectID); err != nil {
		return SecretBinding{}, err
	}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_secret_binding.revoke", Key: command.IdempotencyKey}
	now := time.Now().UTC()
	_, isNew, err := idempotency.Claim(ctx, tx, scope, hash, binding.ID, now)
	if err != nil {
		return SecretBinding{}, err
	}
	if !isNew {
		if err := idempotency.LoadResponse(ctx, tx, scope, &binding); err != nil {
			return SecretBinding{}, err
		}
		return binding, tx.Commit()
	}
	if binding.State == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE access_secret_bindings SET state='revoked',updated_at=$2 WHERE id=$1`, binding.ID, now); err != nil {
			return SecretBinding{}, err
		}
		binding.State, binding.UpdatedAt = "revoked", now
		var used bool
		if err := tx.GetContext(ctx, &used, `SELECT EXISTS(SELECT 1 FROM access_hosts WHERE secret_binding_id=$1 AND lifecycle='active')`, binding.ID); err != nil {
			return SecretBinding{}, err
		}
		if used {
			if err := m.changeSync(ctx, tx, binding.ProjectID, now); err != nil {
				return SecretBinding{}, err
			}
		}
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_secret_binding.revoke",
		TargetType: "access_secret_binding", TargetID: binding.ID, Summary: map[string]string{
			"projectId": binding.ProjectID.String(), "hostname": binding.Hostname}, CreatedAt: now}); err != nil {
		return SecretBinding{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, binding); err != nil {
		return SecretBinding{}, err
	}
	return binding, tx.Commit()
}
