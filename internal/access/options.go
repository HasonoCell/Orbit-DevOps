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
