package access

import (
	"context"
	"net"
	"slices"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
)

// ControllerObservation 是一次 Kubernetes 点时回读；unknown 不可被数据库期望值替代。
type ControllerObservation struct {
	GatewayState        string             `json:"gatewayState"`
	ListenerState       string             `json:"listenerState"`
	Routes              []RouteObservation `json:"routes"`
	CertificateState    string             `json:"certificateState"`
	SecretState         string             `json:"secretState"`
	CertificateNotAfter *time.Time         `json:"certificateNotAfter,omitempty"`
	Addresses           []string           `json:"addresses"`
	ErrorCode           string             `json:"errorCode,omitempty"`
	ObservedAt          time.Time          `json:"observedAt"`
}

type RouteObservation struct {
	RouteID      uuid.UUID `json:"routeId"`
	Accepted     string    `json:"accepted"`
	ResolvedRefs string    `json:"resolvedRefs"`
}

type DNSObservation struct {
	State      string    `json:"state"`
	Answers    []string  `json:"answers"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

type SyncStatus struct {
	DesiredRevision int64   `json:"desiredRevision"`
	AppliedRevision int64   `json:"appliedRevision"`
	State           string  `json:"state"`
	LastErrorCode   *string `json:"lastErrorCode,omitempty"`
}

type HostStatus struct {
	Host       Host                  `json:"host"`
	Sync       SyncStatus            `json:"sync"`
	Controller ControllerObservation `json:"controller"`
	DNS        DNSObservation        `json:"dns"`
}

type ControllerObserver interface {
	ObserveHost(context.Context, Snapshot, HostSpec, []RouteSpec) (ControllerObservation, error)
}

// DNSResolver 是入口状态的外部 DNS 边界，测试可注入受控解析结果。
type DNSResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type syncStatusRow struct {
	DesiredRevision int64   `db:"desired_revision"`
	AppliedRevision int64   `db:"applied_revision"`
	State           string  `db:"state"`
	LastErrorCode   *string `db:"last_error_code"`
}

// GetHostStatus 先授权后回读；Orbit 调和、控制器和 DNS 三种事实各自保留独立状态。
func (m *Module) GetHostStatus(ctx context.Context, projectID, hostID uuid.UUID, caller identity.Caller, observer ControllerObserver) (HostStatus, error) {
	host, err := m.GetHost(ctx, projectID, hostID, caller)
	if err != nil {
		return HostStatus{}, err
	}
	var syncRow syncStatusRow
	if err := m.db.GetContext(ctx, &syncRow, `SELECT desired_revision,applied_revision,state,last_error_code FROM project_gateway_sync
		WHERE project_id=$1 AND cluster_ref=$2 AND namespace=$3`, projectID, m.config.ClusterRef, m.config.Namespace); err != nil {
		return HostStatus{}, err
	}
	result := HostStatus{Host: host, Sync: SyncStatus{DesiredRevision: syncRow.DesiredRevision,
		AppliedRevision: syncRow.AppliedRevision, State: syncRow.State, LastErrorCode: syncRow.LastErrorCode},
		Controller: ControllerObservation{GatewayState: "unknown", ListenerState: "unknown", CertificateState: "unknown",
			SecretState: "unknown", Routes: []RouteObservation{}, Addresses: []string{}, ObservedAt: time.Now().UTC()},
		DNS: DNSObservation{State: "unavailable", Answers: []string{}, ObservedAt: time.Now().UTC()}}
	if observer == nil {
		result.Controller.ErrorCode = "controller_not_configured"
		return result, nil
	}
	snapshot, err := m.LoadSnapshot(ctx, projectID)
	if err != nil {
		return HostStatus{}, err
	}
	var selected HostSpec
	for _, item := range snapshot.Hosts {
		if item.Host.ID == hostID {
			selected = item
			break
		}
	}
	if selected.Host.ID == uuid.Nil {
		return HostStatus{}, ErrHostNotFound
	}
	routes := make([]RouteSpec, 0)
	for _, item := range snapshot.Routes {
		if item.Route.HostID == hostID && item.Route.Lifecycle == "active" {
			routes = append(routes, item)
		}
	}
	readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	observed, observeErr := observer.ObserveHost(readContext, snapshot, selected, routes)
	cancel()
	if observeErr != nil {
		result.Controller.ErrorCode = "controller_read_failed"
		return result, nil
	}
	result.Controller = observed
	resolver := m.config.DNSResolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	result.DNS = verifyDNS(ctx, resolver, host.Hostname, observed.Addresses)
	return result, nil
}

func verifyDNS(ctx context.Context, resolver DNSResolver, hostname string, gatewayAddresses []string) DNSObservation {
	result := DNSObservation{State: "unavailable", Answers: []string{}, ObservedAt: time.Now().UTC()}
	if len(gatewayAddresses) == 0 {
		result.ErrorCode = "gateway_address_unavailable"
		return result
	}
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hostIPs, err := resolver.LookupIPAddr(query, hostname)
	if err != nil {
		result.ErrorCode = "dns_lookup_failed"
		return result
	}
	for _, answer := range hostIPs {
		result.Answers = append(result.Answers, answer.IP.String())
	}
	targets := make([]string, 0)
	for _, address := range gatewayAddresses {
		if parsed := net.ParseIP(address); parsed != nil {
			targets = append(targets, parsed.String())
			continue
		}
		addresses, lookupErr := resolver.LookupIPAddr(query, address)
		if lookupErr != nil {
			result.ErrorCode = "gateway_address_lookup_failed"
			return result
		}
		for _, item := range addresses {
			targets = append(targets, item.IP.String())
		}
	}
	if dnsMatchesGateway(result.Answers, targets) {
		result.State = "verified"
		return result
	}
	result.State = "mismatch"
	if len(result.Answers) == 0 {
		result.State = "unavailable"
		result.ErrorCode = "dns_no_records"
	}
	return result
}

func dnsMatchesGateway(answers, targets []string) bool {
	if len(answers) == 0 || len(targets) == 0 {
		return false
	}
	for _, answer := range answers {
		if !slices.Contains(targets, answer) {
			return false
		}
	}
	return true
}
