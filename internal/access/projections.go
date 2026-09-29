package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var ErrTargetNotFound = errors.New("access target not found")

// EligibleTarget 是创建 Route 时的受限候选，不推断 Service 已存在或已由控制器接纳。
type EligibleTarget struct {
	ID              uuid.UUID `db:"id" json:"id"`
	ApplicationID   uuid.UUID `db:"application_id" json:"applicationId"`
	ApplicationName string    `db:"application_name" json:"applicationName"`
	Stage           string    `db:"stage" json:"stage"`
	ClusterRef      string    `db:"cluster_ref" json:"clusterRef"`
	Namespace       string    `db:"namespace" json:"namespace"`
}

// TargetRoute 按一个 Target 投影其入口关系，避免客户端遍历整个项目的 Host 和 Route。
type TargetRoute struct {
	RouteID        uuid.UUID `db:"route_id" json:"routeId"`
	HostID         uuid.UUID `db:"host_id" json:"hostId"`
	Hostname       string    `db:"hostname" json:"hostname"`
	PathPrefix     string    `db:"path_prefix" json:"pathPrefix"`
	RouteLifecycle string    `db:"route_lifecycle" json:"routeLifecycle"`
	HostLifecycle  string    `db:"host_lifecycle" json:"hostLifecycle"`
}

// ListEligibleTargets 在读事务内同时检查项目可见性与 Host 归属，结果只包含同网络边界的目标。
func (m *Module) ListEligibleTargets(ctx context.Context, projectID, hostID uuid.UUID, caller identity.Caller, page Page) ([]EligibleTarget, error) {
	page, err := page.normalized()
	if err != nil {
		return nil, err
	}
	items := make([]EligibleTarget, 0)
	err = m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		var host Host
		if err := tx.GetContext(ctx, &host, `SELECT `+hostColumns+` FROM access_hosts WHERE id=$1 AND project_id=$2`, hostID, projectID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrHostNotFound
			}
			return fmt.Errorf("load access host for targets: %w", err)
		}
		return tx.SelectContext(ctx, &items, `SELECT t.id,t.application_id,a.name AS application_name,t.stage,t.cluster_ref,t.namespace
			FROM deployment_targets t JOIN applications a ON a.id=t.application_id
			WHERE a.project_id=$1 AND t.cluster_ref=$2 AND t.namespace=$3
			ORDER BY a.name,t.stage,t.id LIMIT $4 OFFSET $5`, projectID, host.ClusterRef, host.Namespace, page.Limit, page.Offset)
	})
	return items, err
}

// ListTargetRoutes 以目标所属项目授权后才读取入口关系，包括清理中的关系供 UI 继续观察。
func (m *Module) ListTargetRoutes(ctx context.Context, targetID uuid.UUID, caller identity.Caller, page Page) ([]TargetRoute, error) {
	page, err := page.normalized()
	if err != nil {
		return nil, err
	}
	items := make([]TargetRoute, 0)
	err = m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		var projectID uuid.UUID
		if err := tx.GetContext(ctx, &projectID, `SELECT a.project_id FROM deployment_targets t JOIN applications a ON a.id=t.application_id WHERE t.id=$1`, targetID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTargetNotFound
			}
			return fmt.Errorf("load access target project: %w", err)
		}
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionRead); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &items, `SELECT r.id AS route_id,h.id AS host_id,h.hostname,r.path_prefix,
			r.lifecycle AS route_lifecycle,h.lifecycle AS host_lifecycle
			FROM access_routes r JOIN access_hosts h ON h.id=r.host_id
			WHERE r.deployment_target_id=$1 AND h.project_id=$2
			ORDER BY h.hostname,r.path_prefix,r.id LIMIT $3 OFFSET $4`, targetID, projectID, page.Limit, page.Offset)
	})
	if errors.Is(err, projectauth.ErrNotMember) {
		return nil, ErrTargetNotFound
	}
	return items, err
}
