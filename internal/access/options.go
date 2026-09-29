package access

import (
	"context"
	"sort"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type IssuerOption struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type HostOptions struct {
	ClusterRef     string         `json:"clusterRef"`
	Namespace      string         `json:"namespace"`
	IssuerPolicies []IssuerOption `json:"issuerPolicies"`
}

type SecretBindingOption struct {
	ID         uuid.UUID `db:"id" json:"id"`
	SecretName string    `db:"secret_name" json:"secretName"`
	ClusterRef string    `db:"cluster_ref" json:"clusterRef"`
	Namespace  string    `db:"namespace" json:"namespace"`
	Hostname   string    `db:"hostname" json:"hostname"`
}

// ListSecretBindingOptions 限定项目、受控集群/Namespace 与精确域名，只暴露 active 绑定的名称。
func (m *Module) ListSecretBindingOptions(ctx context.Context, projectID uuid.UUID, caller identity.Caller, rawHostname string, page Page) ([]SecretBindingOption, error) {
	hostname, err := NormalizeHostname(rawHostname)
	if err != nil {
		return nil, ErrInvalidHost
	}
	page, err = page.normalized()
	if err != nil {
		return nil, err
	}
	result := make([]SecretBindingOption, 0)
	err = m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		if err := m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionManageAccessHosts); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &result, `SELECT id,secret_name,cluster_ref,namespace,hostname
 FROM access_secret_bindings WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3
 AND hostname=$4 AND state='active' ORDER BY secret_name,id LIMIT $5 OFFSET $6`,
			projectID, m.config.ClusterRef, m.config.Namespace, hostname, page.Limit, page.Offset)
	})
	return result, err
}

// GetHostOptions 仅为有入口管理权限的成员枚举运维预置的 Policy 标识，不暴露凭据。
func (m *Module) GetHostOptions(ctx context.Context, projectID uuid.UUID, caller identity.Caller) (HostOptions, error) {
	err := m.authorizer.Read(ctx, caller, func(tx *sqlx.Tx) error {
		return m.authorizer.RequireAuthorizedInTransaction(ctx, tx, projectID, caller, projectauth.PermissionManageAccessHosts)
	})
	if err != nil {
		return HostOptions{}, err
	}
	options := HostOptions{ClusterRef: m.config.ClusterRef, Namespace: m.config.Namespace, IssuerPolicies: make([]IssuerOption, 0, len(m.config.IssuerPolicies))}
	for key, policy := range m.config.IssuerPolicies {
		options.IssuerPolicies = append(options.IssuerPolicies, IssuerOption{Key: key, Kind: policy.Kind, Name: policy.Name})
	}
	sort.Slice(options.IssuerPolicies, func(i, j int) bool { return options.IssuerPolicies[i].Key < options.IssuerPolicies[j].Key })
	return options, nil
}
