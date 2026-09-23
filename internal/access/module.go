// Package access 管理 Project 的访问域名、路径路由和入口期望修订，不把 Kubernetes 状态当作数据库事实。
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"golang.org/x/net/idna"
)

var (
	ErrInvalidHost   = errors.New("invalid access host")
	ErrInvalidRoute  = errors.New("invalid access route")
	ErrHostNotFound  = errors.New("access host not found")
	ErrRouteNotFound = errors.New("access route not found")
	ErrConflict      = errors.New("access resource conflict")
)

type Config struct {
	ClusterRef     string
	Namespace      string
	IssuerPolicies map[string]IssuerPolicy
}

type IssuerPolicy struct{ Kind, Name string }

type Module struct {
	db         *sqlx.DB
	authorizer *projectauth.Module
	config     Config
}

type Host struct {
	ID              uuid.UUID  `db:"id" json:"id"`
	ProjectID       uuid.UUID  `db:"project_id" json:"projectId"`
	ClusterRef      string     `db:"cluster_ref" json:"clusterRef"`
	Namespace       string     `db:"namespace" json:"namespace"`
	Hostname        string     `db:"hostname" json:"hostname"`
	TLSMode         string     `db:"tls_mode" json:"tlsMode"`
	IssuerPolicyKey *string    `db:"issuer_policy_key" json:"issuerPolicyKey,omitempty"`
	SecretBindingID *uuid.UUID `db:"secret_binding_id" json:"secretBindingId,omitempty"`
	Lifecycle       string     `db:"lifecycle" json:"lifecycle"`
	CreatedAt       time.Time  `db:"created_at" json:"createdAt"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updatedAt"`
}

type Route struct {
	ID                 uuid.UUID `db:"id" json:"id"`
	HostID             uuid.UUID `db:"host_id" json:"hostId"`
	DeploymentTargetID uuid.UUID `db:"deployment_target_id" json:"deploymentTargetId"`
	PathPrefix         string    `db:"path_prefix" json:"pathPrefix"`
	Lifecycle          string    `db:"lifecycle" json:"lifecycle"`
	CreatedAt          time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt          time.Time `db:"updated_at" json:"updatedAt"`
}

type HostInput struct {
	Hostname        string
	TLSMode         string
	IssuerPolicyKey *string
	SecretBindingID *uuid.UUID
}

type HostCommand struct {
	ProjectID      uuid.UUID
	Caller         identity.Caller
	IdempotencyKey string
	Input          HostInput
}

type RouteCommand struct {
	ProjectID          uuid.UUID
	HostID             uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
	PathPrefix         string
	DeploymentTargetID uuid.UUID
}

func New(db *sqlx.DB, authorizer *projectauth.Module, config Config) (*Module, error) {
	if db == nil || authorizer == nil || config.ClusterRef == "" || config.Namespace == "" {
		return nil, errors.New("invalid access module configuration")
	}
	return &Module{db: db, authorizer: authorizer, config: config}, nil
}

// NormalizeHostname 把同一 DNS 名的大小写与尾点统一，拒绝通配、IP 和非完整域名。
func NormalizeHostname(value string) (string, error) {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if value == "" || strings.ContainsAny(value, ":/* ") || len(value) > 253 {
		return "", ErrInvalidHost
	}
	ascii, err := idna.Lookup.ToASCII(value)
	if err != nil {
		return "", ErrInvalidHost
	}
	ascii = strings.ToLower(ascii)
	parts := strings.Split(ascii, ".")
	if len(parts) < 2 || len(parts[len(parts)-1]) < 2 {
		return "", ErrInvalidHost
	}
	for _, part := range parts {
		if len(part) < 1 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return "", ErrInvalidHost
		}
		for _, char := range part {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", ErrInvalidHost
			}
		}
	}
	return ascii, nil
}

// NormalizePath 与 Gateway API 的 PathPrefix 语义保持一致，不隐式合并两个不同路径。
func NormalizePath(value string) (string, error) {
	if len(value) < 1 || len(value) > 1024 || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#\\") || strings.Contains(value, "//") {
		return "", ErrInvalidRoute
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return "", ErrInvalidRoute
		}
	}
	if value != "/" {
		value = strings.TrimSuffix(value, "/")
	}
	return value, nil
}

func (m *Module) validateHostInput(input HostInput) (HostInput, error) {
	hostname, err := NormalizeHostname(input.Hostname)
	if err != nil {
		return HostInput{}, err
	}
	input.Hostname = hostname
	switch input.TLSMode {
	case "http_only":
		if input.IssuerPolicyKey != nil || input.SecretBindingID != nil {
			return HostInput{}, ErrInvalidHost
		}
	case "managed":
		if input.IssuerPolicyKey == nil || input.SecretBindingID != nil {
			return HostInput{}, ErrInvalidHost
		}
		if _, ok := m.config.IssuerPolicies[*input.IssuerPolicyKey]; !ok {
			return HostInput{}, ErrInvalidHost
		}
	case "existing_secret":
		if input.IssuerPolicyKey != nil || input.SecretBindingID == nil {
			return HostInput{}, ErrInvalidHost
		}
	default:
		return HostInput{}, ErrInvalidHost
	}
	return input, nil
}

// lockSync 使同一 Project Gateway 的所有期望写入串行，避免 listener 集合丢失更新。
func (m *Module) lockSync(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID) error {
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_gateway_sync
		(project_id,cluster_ref,namespace,desired_revision,applied_revision,state,next_attempt_at,updated_at)
		VALUES ($1,$2,$3,0,0,'applied',$4,$4) ON CONFLICT DO NOTHING`, projectID, m.config.ClusterRef, m.config.Namespace, now); err != nil {
		return err
	}
	var revision int64
	return tx.GetContext(ctx, &revision, `SELECT desired_revision FROM project_gateway_sync
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3 FOR UPDATE`, projectID, m.config.ClusterRef, m.config.Namespace)
}

// changeSync 与业务写入、审计同事务生成可靠唤醒；Redis 不承载权威配置。
func (m *Module) changeSync(ctx context.Context, tx *sqlx.Tx, projectID uuid.UUID, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE project_gateway_sync SET desired_revision=desired_revision+1,
		state='pending',next_attempt_at=$4,last_error_code=NULL,updated_at=$4
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`, projectID, m.config.ClusterRef, m.config.Namespace, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO internal_event_outbox
		(id,topic,aggregate_id,state,available_at,next_dispatch_at,created_at,updated_at)
		VALUES ($1,'project_gateway.reconcile.v1',$2,'pending',$3,$3,$3,$3)`, uuid.New(), projectID, now)
	return err
}

func conflict(err error) error {
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) && postgres.Code == "23505" {
		return ErrConflict
	}
	return err
}

const hostColumns = `id,project_id,cluster_ref,namespace,hostname,tls_mode,issuer_policy_key,
	secret_binding_id,lifecycle,created_at,updated_at`
const routeColumns = `id,host_id,deployment_target_id,path_prefix,lifecycle,created_at,updated_at`

// CreateHost 将域名声明、幂等、审计和调和意图原子接纳；不在请求事务内访问集群。
func (m *Module) CreateHost(ctx context.Context, command HostCommand) (Host, error) {
	input, err := m.validateHostInput(command.Input)
	if err != nil {
		return Host{}, err
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID uuid.UUID
		Input     HostInput
	}{command.ProjectID, input})
	if err != nil {
		return Host{}, err
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Host{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return Host{}, err
	}
	if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, command.ProjectID, command.Caller, projectauth.PermissionManageAccessHosts); err != nil {
		return Host{}, err
	}
	if err := m.lockSync(ctx, tx, command.ProjectID); err != nil {
		return Host{}, err
	}
	now := time.Now().UTC()
	host := Host{ID: uuid.New(), ProjectID: command.ProjectID, ClusterRef: m.config.ClusterRef,
		Namespace: m.config.Namespace, Hostname: input.Hostname, TLSMode: input.TLSMode,
		IssuerPolicyKey: input.IssuerPolicyKey, SecretBindingID: input.SecretBindingID,
		Lifecycle: "active", CreatedAt: now, UpdatedAt: now}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_host.create", Key: command.IdempotencyKey}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, hash, host.ID, now)
	if err != nil {
		return Host{}, err
	}
	if !isNew {
		if err := idempotency.LoadResponse(ctx, tx, scope, &host); err != nil {
			return Host{}, err
		}
		return host, tx.Commit()
	}
	if input.SecretBindingID != nil {
		var valid bool
		if err := tx.GetContext(ctx, &valid, `SELECT EXISTS (SELECT 1 FROM access_secret_bindings
			WHERE id=$1 AND project_id=$2 AND cluster_ref=$3 AND namespace=$4 AND hostname=$5 AND state='active')`,
			*input.SecretBindingID, command.ProjectID, m.config.ClusterRef, m.config.Namespace, input.Hostname); err != nil {
			return Host{}, err
		}
		if !valid {
			return Host{}, ErrInvalidHost
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_hosts
		(id,project_id,cluster_ref,namespace,hostname,tls_mode,issuer_policy_key,secret_binding_id,lifecycle,created_by,created_at,updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active',$9,$10,$10)`, host.ID, host.ProjectID, host.ClusterRef,
		host.Namespace, host.Hostname, host.TLSMode, host.IssuerPolicyKey, host.SecretBindingID, command.Caller.UserID(), now)
	if err != nil {
		return Host{}, conflict(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_host.create",
		TargetType: "access_host", TargetID: host.ID, Summary: map[string]string{"projectId": command.ProjectID.String(), "hostname": host.Hostname}, CreatedAt: now}); err != nil {
		return Host{}, err
	}
	if err := m.changeSync(ctx, tx, command.ProjectID, now); err != nil {
		return Host{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, host); err != nil {
		return Host{}, err
	}
	return host, tx.Commit()
}

// GetHost 先判断当前身份与项目可见性，再读取 Host，避免跨项目探测。
func (m *Module) GetHost(ctx context.Context, projectID, hostID uuid.UUID, caller identity.Caller) (Host, error) {
	var host Host
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		err := tx.GetContext(ctx, &host, `SELECT `+hostColumns+` FROM access_hosts WHERE id=$1 AND project_id=$2`, hostID, projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrHostNotFound
		}
		return err
	})
	return host, err
}

func (m *Module) ListHosts(ctx context.Context, projectID uuid.UUID, caller identity.Caller) ([]Host, error) {
	hosts := make([]Host, 0)
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &hosts, `SELECT `+hostColumns+` FROM access_hosts WHERE project_id=$1 ORDER BY created_at,id LIMIT 100`, projectID)
	})
	return hosts, err
}

// CreateRoute 保证后端 Target 和 Host 同项目、同受控集群，端口不由客户端提供。
func (m *Module) CreateRoute(ctx context.Context, command RouteCommand) (Route, error) {
	path, err := NormalizePath(command.PathPrefix)
	if err != nil || command.DeploymentTargetID == uuid.Nil {
		return Route{}, ErrInvalidRoute
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID, HostID, TargetID uuid.UUID
		Path                        string
	}{
		command.ProjectID, command.HostID, command.DeploymentTargetID, path})
	if err != nil {
		return Route{}, err
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Route{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.authorizer.AuthorizeUserInTransaction(ctx, tx, command.Caller); err != nil {
		return Route{}, err
	}
	if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, command.ProjectID, command.Caller, projectauth.PermissionManageAccessRoutes); err != nil {
		return Route{}, err
	}
	if err := m.lockSync(ctx, tx, command.ProjectID); err != nil {
		return Route{}, err
	}
	var host Host
	if err := tx.GetContext(ctx, &host, `SELECT `+hostColumns+` FROM access_hosts WHERE id=$1 AND project_id=$2 AND lifecycle='active'`, command.HostID, command.ProjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Route{}, ErrHostNotFound
		}
		return Route{}, err
	}
	now := time.Now().UTC()
	route := Route{ID: uuid.New(), HostID: command.HostID, DeploymentTargetID: command.DeploymentTargetID,
		PathPrefix: path, Lifecycle: "active", CreatedAt: now, UpdatedAt: now}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_route.create", Key: command.IdempotencyKey}
	_, isNew, err := idempotency.Claim(ctx, tx, scope, hash, route.ID, now)
	if err != nil {
		return Route{}, err
	}
	if !isNew {
		if err := idempotency.LoadResponse(ctx, tx, scope, &route); err != nil {
			return Route{}, err
		}
		return route, tx.Commit()
	}
	var valid bool
	if err := tx.GetContext(ctx, &valid, `SELECT EXISTS (SELECT 1 FROM deployment_targets t JOIN applications a ON a.id=t.application_id
		WHERE t.id=$1 AND a.project_id=$2 AND t.cluster_ref=$3 AND t.namespace=$4)`, command.DeploymentTargetID,
		command.ProjectID, host.ClusterRef, host.Namespace); err != nil {
		return Route{}, err
	}
	if !valid {
		return Route{}, ErrRouteNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO access_routes
		(id,host_id,deployment_target_id,path_prefix,lifecycle,created_by,created_at,updated_at)
		VALUES ($1,$2,$3,$4,'active',$5,$6,$6)`, route.ID, route.HostID, route.DeploymentTargetID,
		route.PathPrefix, command.Caller.UserID(), now)
	if err != nil {
		return Route{}, conflict(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_route.create",
		TargetType: "access_route", TargetID: route.ID, Summary: map[string]string{"hostId": route.HostID.String(), "deploymentTargetId": route.DeploymentTargetID.String(), "pathPrefix": path}, CreatedAt: now}); err != nil {
		return Route{}, err
	}
	if err := m.changeSync(ctx, tx, command.ProjectID, now); err != nil {
		return Route{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, route); err != nil {
		return Route{}, err
	}
	return route, tx.Commit()
}

func (m *Module) GetRoute(ctx context.Context, projectID, hostID, routeID uuid.UUID, caller identity.Caller) (Route, error) {
	var route Route
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		err := tx.GetContext(ctx, &route, `SELECT r.`+strings.ReplaceAll(routeColumns, ",", ",r.")+` FROM access_routes r
			JOIN access_hosts h ON h.id=r.host_id WHERE r.id=$1 AND r.host_id=$2 AND h.project_id=$3`, routeID, hostID, projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRouteNotFound
		}
		return err
	})
	return route, err
}

func (m *Module) ListRoutes(ctx context.Context, projectID, hostID uuid.UUID, caller identity.Caller) ([]Route, error) {
	routes := make([]Route, 0)
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		var exists bool
		if err := tx.GetContext(ctx, &exists, `SELECT EXISTS(SELECT 1 FROM access_hosts WHERE id=$1 AND project_id=$2)`, hostID, projectID); err != nil {
			return err
		}
		if !exists {
			return ErrHostNotFound
		}
		return tx.SelectContext(ctx, &routes, `SELECT `+routeColumns+` FROM access_routes WHERE host_id=$1 ORDER BY path_prefix,id LIMIT 100`, hostID)
	})
	return routes, err
}

func (m *Module) String() string {
	return fmt.Sprintf("access(%s/%s)", m.config.ClusterRef, m.config.Namespace)
}
