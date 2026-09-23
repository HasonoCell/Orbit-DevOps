package access

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
)

type UpdateHostCommand struct {
	ProjectID, HostID uuid.UUID
	Caller            identity.Caller
	IdempotencyKey    string
	Input             HostInput
}

type UpdateRouteCommand struct {
	ProjectID, HostID, RouteID uuid.UUID
	Caller                     identity.Caller
	IdempotencyKey             string
	PathPrefix                 string
	DeploymentTargetID         uuid.UUID
}

type DeleteCommand struct {
	ProjectID, HostID, RouteID uuid.UUID
	Caller                     identity.Caller
	IdempotencyKey             string
}

// UpdateHost 只允许修改 TLS 配置；域名迁移需新建 Host，不能绕过域名归属清理。
func (m *Module) UpdateHost(ctx context.Context, command UpdateHostCommand) (Host, error) {
	input, err := m.validateHostInput(command.Input)
	if err != nil {
		return Host{}, err
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID, HostID uuid.UUID
		Input             HostInput
	}{command.ProjectID, command.HostID, input})
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
	var host Host
	if err := tx.GetContext(ctx, &host, `SELECT `+hostColumns+` FROM access_hosts WHERE id=$1 AND project_id=$2`, command.HostID, command.ProjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Host{}, ErrHostNotFound
		}
		return Host{}, err
	}
	if host.Lifecycle != "active" || input.Hostname != host.Hostname {
		return Host{}, ErrConflict
	}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_host.update", Key: command.IdempotencyKey}
	now := time.Now().UTC()
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
		if err := tx.GetContext(ctx, &valid, `SELECT EXISTS (SELECT 1 FROM access_secret_bindings WHERE id=$1 AND project_id=$2
			AND cluster_ref=$3 AND namespace=$4 AND hostname=$5 AND state='active')`, *input.SecretBindingID,
			command.ProjectID, m.config.ClusterRef, m.config.Namespace, host.Hostname); err != nil {
			return Host{}, err
		}
		if !valid {
			return Host{}, ErrInvalidHost
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE access_hosts SET tls_mode=$3,issuer_policy_key=$4,secret_binding_id=$5,updated_at=$6
		WHERE id=$1 AND project_id=$2`, host.ID, command.ProjectID, input.TLSMode, input.IssuerPolicyKey, input.SecretBindingID, now)
	if err != nil {
		return Host{}, err
	}
	host.TLSMode, host.IssuerPolicyKey, host.SecretBindingID, host.UpdatedAt = input.TLSMode, input.IssuerPolicyKey, input.SecretBindingID, now
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_host.update",
		TargetType: "access_host", TargetID: host.ID, Summary: map[string]string{"tlsMode": host.TLSMode}, CreatedAt: now}); err != nil {
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

// DeleteHost 先保留 tombstone 与域名唯一声明；Worker 完成 K8s 清理才物理删除。
func (m *Module) DeleteHost(ctx context.Context, command DeleteCommand) (Host, error) {
	hash, err := idempotency.Fingerprint(struct{ ProjectID, HostID uuid.UUID }{command.ProjectID, command.HostID})
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
	var host Host
	if err := tx.GetContext(ctx, &host, `SELECT `+hostColumns+` FROM access_hosts WHERE id=$1 AND project_id=$2`, command.HostID, command.ProjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Host{}, ErrHostNotFound
		}
		return Host{}, err
	}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_host.delete", Key: command.IdempotencyKey}
	now := time.Now().UTC()
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
	if host.Lifecycle == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE access_hosts SET lifecycle='deleting',updated_at=$2 WHERE id=$1`, host.ID, now); err != nil {
			return Host{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE access_routes SET lifecycle='deleting',updated_at=$2 WHERE host_id=$1 AND lifecycle='active'`, host.ID, now); err != nil {
			return Host{}, err
		}
		host.Lifecycle, host.UpdatedAt = "deleting", now
		if err := m.changeSync(ctx, tx, command.ProjectID, now); err != nil {
			return Host{}, err
		}
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_host.delete",
		TargetType: "access_host", TargetID: host.ID, Summary: map[string]string{"hostname": host.Hostname}, CreatedAt: now}); err != nil {
		return Host{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, host); err != nil {
		return Host{}, err
	}
	return host, tx.Commit()
}

// UpdateRoute 在同一 Gateway 集合锁下更新路径和目标，确保目标归属与端口来源不变。
func (m *Module) UpdateRoute(ctx context.Context, command UpdateRouteCommand) (Route, error) {
	path, err := NormalizePath(command.PathPrefix)
	if err != nil || command.DeploymentTargetID == uuid.Nil {
		return Route{}, ErrInvalidRoute
	}
	hash, err := idempotency.Fingerprint(struct {
		ProjectID, HostID, RouteID, TargetID uuid.UUID
		Path                                 string
	}{
		command.ProjectID, command.HostID, command.RouteID, command.DeploymentTargetID, path})
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
	var route Route
	if err := tx.GetContext(ctx, &route, `SELECT `+routeColumns+` FROM access_routes WHERE id=$1 AND host_id=$2 AND lifecycle='active'`, command.RouteID, command.HostID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Route{}, ErrRouteNotFound
		}
		return Route{}, err
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
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_route.update", Key: command.IdempotencyKey}
	now := time.Now().UTC()
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
	_, err = tx.ExecContext(ctx, `UPDATE access_routes SET deployment_target_id=$2,path_prefix=$3,updated_at=$4 WHERE id=$1`, route.ID, command.DeploymentTargetID, path, now)
	if err != nil {
		return Route{}, conflict(err)
	}
	route.DeploymentTargetID, route.PathPrefix, route.UpdatedAt = command.DeploymentTargetID, path, now
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_route.update",
		TargetType: "access_route", TargetID: route.ID, Summary: map[string]string{"hostId": route.HostID.String(), "pathPrefix": path}, CreatedAt: now}); err != nil {
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

func (m *Module) DeleteRoute(ctx context.Context, command DeleteCommand) (Route, error) {
	hash, err := idempotency.Fingerprint(struct{ ProjectID, HostID, RouteID uuid.UUID }{command.ProjectID, command.HostID, command.RouteID})
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
	var route Route
	if err := tx.GetContext(ctx, &route, `SELECT `+routeColumns+` FROM access_routes WHERE id=$1 AND host_id=$2
		AND EXISTS (SELECT 1 FROM access_hosts WHERE id=$2 AND project_id=$3)`, command.RouteID, command.HostID, command.ProjectID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Route{}, ErrRouteNotFound
		}
		return Route{}, err
	}
	scope := idempotency.Scope{ActorID: command.Caller.ActorID(), CommandType: "access_route.delete", Key: command.IdempotencyKey}
	now := time.Now().UTC()
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
	if route.Lifecycle == "active" {
		if _, err := tx.ExecContext(ctx, `UPDATE access_routes SET lifecycle='deleting',updated_at=$2 WHERE id=$1`, route.ID, now); err != nil {
			return Route{}, err
		}
		route.Lifecycle, route.UpdatedAt = "deleting", now
		if err := m.changeSync(ctx, tx, command.ProjectID, now); err != nil {
			return Route{}, err
		}
	}
	if err := audit.Append(ctx, tx, audit.Entry{ActorID: command.Caller.ActorID(), Action: "access_route.delete",
		TargetType: "access_route", TargetID: route.ID, Summary: map[string]string{"hostId": route.HostID.String()}, CreatedAt: now}); err != nil {
		return Route{}, err
	}
	if err := idempotency.StoreResponse(ctx, tx, scope, route); err != nil {
		return Route{}, err
	}
	return route, tx.Commit()
}
