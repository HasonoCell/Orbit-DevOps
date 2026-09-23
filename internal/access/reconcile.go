package access

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrLeaseLost = errors.New("project gateway reconciliation lease lost")

type HostSpec struct {
	Host          Host
	SecretName    string
	BindingActive bool
	Issuer        IssuerPolicy
}

type RouteSpec struct {
	Route         Route
	ContainerPort int
}

type Snapshot struct {
	ProjectID        uuid.UUID
	ClusterRef       string
	Namespace        string
	GatewayClassName string
	Revision         int64
	Hosts            []HostSpec
	Routes           []RouteSpec
}

type Lease struct {
	ProjectID uuid.UUID
	Token     uuid.UUID
	Revision  int64
}

type syncRow struct {
	DesiredRevision int64      `db:"desired_revision"`
	AppliedRevision int64      `db:"applied_revision"`
	State           string     `db:"state"`
	LeaseToken      *uuid.UUID `db:"lease_token"`
	LeaseExpiresAt  *time.Time `db:"lease_expires_at"`
	NextAttemptAt   time.Time  `db:"next_attempt_at"`
}

type hostSpecRow struct {
	Host
	SecretName   sql.NullString `db:"secret_name"`
	BindingState sql.NullString `db:"binding_state"`
}

type routeSpecRow struct {
	Route
	ContainerPort int `db:"container_port"`
}

// AcquireReconcileLease 串行化一个 Project Gateway；到期租约可被其他 Worker 接管。
func (m *Module) AcquireReconcileLease(ctx context.Context, projectID uuid.UUID, duration time.Duration) (Lease, bool, error) {
	if projectID == uuid.Nil || duration <= 0 {
		return Lease{}, false, errors.New("invalid gateway lease request")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return Lease{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var row syncRow
	if err := tx.GetContext(ctx, &row, `SELECT desired_revision,applied_revision,state,lease_token,lease_expires_at,next_attempt_at
		FROM project_gateway_sync WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3 FOR UPDATE`,
		projectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Lease{}, false, nil
		}
		return Lease{}, false, err
	}
	now := time.Now().UTC()
	if row.LeaseToken != nil && row.LeaseExpiresAt != nil && now.Before(*row.LeaseExpiresAt) {
		return Lease{}, false, tx.Commit()
	}
	if row.State == "applied" && now.Before(row.NextAttemptAt) {
		return Lease{}, false, tx.Commit()
	}
	token := uuid.New()
	if _, err := tx.ExecContext(ctx, `UPDATE project_gateway_sync SET state='applying',lease_token=$4,
		lease_expires_at=$5,updated_at=$6 WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`,
		projectID, m.config.ClusterRef, m.config.Namespace, token, now.Add(duration), now); err != nil {
		return Lease{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, false, err
	}
	return Lease{ProjectID: projectID, Token: token, Revision: row.DesiredRevision}, true, nil
}

// LoadSnapshot 用同一数据库快照组装 Host、Route 与受控 Secret 引用，不信任队列载荷。
func (m *Module) LoadSnapshot(ctx context.Context, projectID uuid.UUID) (Snapshot, error) {
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := Snapshot{ProjectID: projectID, ClusterRef: m.config.ClusterRef,
		Namespace: m.config.Namespace, GatewayClassName: m.config.GatewayClassName,
		Hosts: make([]HostSpec, 0), Routes: make([]RouteSpec, 0)}
	if err := tx.GetContext(ctx, &snapshot.Revision, `SELECT desired_revision FROM project_gateway_sync
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`, projectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return Snapshot{}, err
	}
	var hosts []hostSpecRow
	if err := tx.SelectContext(ctx, &hosts, `SELECT h.`+hostColumnsWithAlias+`,b.secret_name,b.state AS binding_state
		FROM access_hosts h LEFT JOIN access_secret_bindings b ON b.id=h.secret_binding_id
		WHERE h.project_id=$1 AND h.cluster_ref=$2 AND h.namespace=$3 ORDER BY h.id`,
		projectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return Snapshot{}, err
	}
	for _, row := range hosts {
		spec := HostSpec{Host: row.Host, SecretName: row.SecretName.String, BindingActive: row.BindingState.String == "active"}
		if row.Host.IssuerPolicyKey != nil {
			spec.Issuer = m.config.IssuerPolicies[*row.Host.IssuerPolicyKey]
		}
		snapshot.Hosts = append(snapshot.Hosts, spec)
	}
	var routes []routeSpecRow
	if err := tx.SelectContext(ctx, &routes, `SELECT r.id,r.host_id,r.deployment_target_id,r.path_prefix,r.lifecycle,
		r.created_at,r.updated_at,t.container_port FROM access_routes r
		JOIN access_hosts h ON h.id=r.host_id JOIN deployment_targets t ON t.id=r.deployment_target_id
		JOIN applications a ON a.id=t.application_id
		WHERE h.project_id=$1 AND h.cluster_ref=$2 AND h.namespace=$3
		AND a.project_id=h.project_id AND t.cluster_ref=h.cluster_ref AND t.namespace=h.namespace ORDER BY r.id`,
		projectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return Snapshot{}, err
	}
	for _, row := range routes {
		snapshot.Routes = append(snapshot.Routes, RouteSpec{Route: row.Route, ContainerPort: row.ContainerPort})
	}
	if err := tx.Commit(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// CheckLease 是每次 Kubernetes 写入前的围栏；超时或新修订出现时停止旧轮次。
func (m *Module) CheckLease(ctx context.Context, lease Lease, revision int64) error {
	var valid bool
	err := m.db.GetContext(ctx, &valid, `SELECT EXISTS(SELECT 1 FROM project_gateway_sync
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3 AND lease_token=$4
		AND lease_expires_at>clock_timestamp() AND desired_revision=$5 AND state='applying')`,
		lease.ProjectID, m.config.ClusterRef, m.config.Namespace, lease.Token, revision)
	if err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	return nil
}

// CompleteReconcile 仅由持当前租约且修订未变化的 Worker 清理 tombstone 和推进 applied。
func (m *Module) CompleteReconcile(ctx context.Context, lease Lease, revision int64) error {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var row syncRow
	if err := tx.GetContext(ctx, &row, `SELECT desired_revision,applied_revision,state,lease_token,lease_expires_at,next_attempt_at
		FROM project_gateway_sync WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3 FOR UPDATE`,
		lease.ProjectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return err
	}
	if row.LeaseToken == nil || *row.LeaseToken != lease.Token || row.LeaseExpiresAt == nil || time.Now().UTC().After(*row.LeaseExpiresAt) {
		return ErrLeaseLost
	}
	now := time.Now().UTC()
	if row.DesiredRevision != revision {
		if _, err := tx.ExecContext(ctx, `UPDATE project_gateway_sync SET state='pending',lease_token=NULL,
			lease_expires_at=NULL,next_attempt_at=$4,updated_at=$4 WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`,
			lease.ProjectID, m.config.ClusterRef, m.config.Namespace, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_routes WHERE lifecycle='deleting' AND host_id IN
		(SELECT id FROM access_hosts WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3)`,
		lease.ProjectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM access_hosts WHERE lifecycle='deleting' AND project_id=$1 AND cluster_ref=$2 AND namespace=$3`,
		lease.ProjectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE project_gateway_sync SET applied_revision=$4,state='applied',
		lease_token=NULL,lease_expires_at=NULL,next_attempt_at=$5,last_error_code=NULL,updated_at=$5
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`, lease.ProjectID, m.config.ClusterRef,
		m.config.Namespace, revision, now.Add(time.Minute)); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Module) FailReconcile(ctx context.Context, lease Lease, errorCode string, attention bool) error {
	state, next := "retrying", time.Now().UTC().Add(5*time.Second)
	if attention {
		state, next = "attention_required", time.Now().UTC().Add(time.Minute)
	}
	_, err := m.db.ExecContext(ctx, `UPDATE project_gateway_sync SET state=$5,lease_token=NULL,
		lease_expires_at=NULL,next_attempt_at=$6,last_error_code=$7,updated_at=clock_timestamp()
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3 AND lease_token=$4`, lease.ProjectID,
		m.config.ClusterRef, m.config.Namespace, lease.Token, state, next, errorCode)
	return err
}

func (m *Module) DueProjects(ctx context.Context, limit int) ([]uuid.UUID, error) {
	if limit < 1 || limit > 100 {
		return nil, errors.New("invalid gateway scan limit")
	}
	var result []uuid.UUID
	err := m.db.SelectContext(ctx, &result, `SELECT project_id FROM project_gateway_sync
		WHERE cluster_ref=$1 AND namespace=$2 AND next_attempt_at<=clock_timestamp()
		AND (lease_expires_at IS NULL OR lease_expires_at<=clock_timestamp())
		ORDER BY next_attempt_at,project_id LIMIT $3`, m.config.ClusterRef, m.config.Namespace, limit)
	return result, err
}

const hostColumnsWithAlias = `id,h.project_id,h.cluster_ref,h.namespace,h.hostname,h.tls_mode,
	h.issuer_policy_key,h.secret_binding_id,h.lifecycle,h.created_at,h.updated_at`
