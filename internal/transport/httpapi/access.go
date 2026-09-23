package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/idempotency"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

func accessHostResponse(host access.Host) api.AccessHost {
	return api.AccessHost{Id: host.ID, ProjectId: host.ProjectID, ClusterRef: host.ClusterRef,
		Namespace: host.Namespace, Hostname: host.Hostname, TlsMode: api.AccessHostTlsMode(host.TLSMode),
		IssuerPolicyKey: host.IssuerPolicyKey, SecretBindingId: host.SecretBindingID,
		Lifecycle: api.AccessHostLifecycle(host.Lifecycle), CreatedAt: host.CreatedAt, UpdatedAt: host.UpdatedAt}
}

func accessRouteResponse(route access.Route) api.AccessRoute {
	return api.AccessRoute{Id: route.ID, HostId: route.HostID, DeploymentTargetId: route.DeploymentTargetID,
		PathPrefix: route.PathPrefix, Lifecycle: api.AccessRouteLifecycle(route.Lifecycle),
		CreatedAt: route.CreatedAt, UpdatedAt: route.UpdatedAt}
}

func accessPage(limit *api.AccessPageLimit, offset *api.AccessPageOffset) access.Page {
	page := access.Page{}
	if limit != nil {
		page.Limit = *limit
	}
	if offset != nil {
		page.Offset = *offset
	}
	return page
}

func (s *Server) ListAccessHosts(ctx context.Context, request api.ListAccessHostsRequestObject) (api.ListAccessHostsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	hosts, err := s.access.ListHosts(httpRequestContext(ctx), request.ProjectId, requestCaller(ctx), accessPage(request.Params.Limit, request.Params.Offset))
	if err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.ListAccessHosts404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		}
		return nil, err
	}
	items := make(api.ListAccessHosts200JSONResponse, 0, len(hosts))
	for _, host := range hosts {
		items = append(items, accessHostResponse(host))
	}
	return items, nil
}

func (s *Server) CreateAccessHost(ctx context.Context, request api.CreateAccessHostRequestObject) (api.CreateAccessHostResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	created, err := s.access.CreateHost(httpRequestContext(ctx), access.HostCommand{ProjectID: request.ProjectId,
		Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey,
		Input: access.HostInput{Hostname: request.Body.Hostname, TLSMode: string(request.Body.TlsMode),
			IssuerPolicyKey: request.Body.IssuerPolicyKey, SecretBindingID: request.Body.SecretBindingId}})
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidHost):
			return api.CreateAccessHost400JSONResponse{Code: "invalid_access_host", Message: "access host is invalid"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CreateAccessHost403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access hosts"}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.CreateAccessHost404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		case errors.Is(err, access.ErrConflict), errors.Is(err, idempotency.ErrConflict):
			return api.CreateAccessHost409JSONResponse{Code: "access_host_conflict", Message: "access host or idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.CreateAccessHost201JSONResponse(accessHostResponse(created)), nil
}

func (s *Server) GetAccessHost(ctx context.Context, request api.GetAccessHostRequestObject) (api.GetAccessHostResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	host, err := s.access.GetHost(httpRequestContext(ctx), request.ProjectId, request.HostId, requestCaller(ctx))
	if err != nil {
		if errors.Is(err, access.ErrHostNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetAccessHost404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		}
		return nil, err
	}
	return api.GetAccessHost200JSONResponse(accessHostResponse(host)), nil
}

func (s *Server) GetAccessHostStatus(ctx context.Context, request api.GetAccessHostStatusRequestObject) (api.GetAccessHostStatusResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	status, err := s.access.GetHostStatus(httpRequestContext(ctx), request.ProjectId, request.HostId, requestCaller(ctx), s.accessObserver)
	if err != nil {
		if errors.Is(err, access.ErrHostNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetAccessHostStatus404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		}
		return nil, err
	}
	routes := make([]api.AccessRouteObservation, 0, len(status.Controller.Routes))
	for _, route := range status.Controller.Routes {
		routes = append(routes, api.AccessRouteObservation{RouteId: route.RouteID,
			Accepted: api.AccessRouteObservationAccepted(route.Accepted), ResolvedRefs: api.AccessRouteObservationResolvedRefs(route.ResolvedRefs)})
	}
	return api.GetAccessHostStatus200JSONResponse{
		Host: accessHostResponse(status.Host),
		Sync: api.AccessSyncStatus{DesiredRevision: status.Sync.DesiredRevision,
			AppliedRevision: status.Sync.AppliedRevision, State: api.AccessSyncStatusState(status.Sync.State),
			LastErrorCode: status.Sync.LastErrorCode},
		Controller: api.AccessControllerStatus{GatewayState: api.AccessControllerStatusGatewayState(status.Controller.GatewayState),
			ListenerState:       api.AccessControllerStatusListenerState(status.Controller.ListenerState),
			CertificateState:    api.AccessControllerStatusCertificateState(status.Controller.CertificateState),
			CertificateNotAfter: status.Controller.CertificateNotAfter,
			SecretState:         api.AccessControllerStatusSecretState(status.Controller.SecretState),
			Addresses:           status.Controller.Addresses, Routes: routes, ObservedAt: status.Controller.ObservedAt,
			ErrorCode: optionalErrorCode(status.Controller.ErrorCode)},
		Dns: api.AccessDnsStatus{State: api.AccessDnsStatusState(status.DNS.State), Answers: status.DNS.Answers,
			ErrorCode: optionalErrorCode(status.DNS.ErrorCode), ObservedAt: status.DNS.ObservedAt},
	}, nil
}

func optionalErrorCode(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s *Server) ListAccessRoutes(ctx context.Context, request api.ListAccessRoutesRequestObject) (api.ListAccessRoutesResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	routes, err := s.access.ListRoutes(httpRequestContext(ctx), request.ProjectId, request.HostId, requestCaller(ctx), accessPage(request.Params.Limit, request.Params.Offset))
	if err != nil {
		if errors.Is(err, access.ErrHostNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.ListAccessRoutes404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		}
		return nil, err
	}
	items := make(api.ListAccessRoutes200JSONResponse, 0, len(routes))
	for _, route := range routes {
		items = append(items, accessRouteResponse(route))
	}
	return items, nil
}

func (s *Server) CreateAccessRoute(ctx context.Context, request api.CreateAccessRouteRequestObject) (api.CreateAccessRouteResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	route, err := s.access.CreateRoute(httpRequestContext(ctx), access.RouteCommand{ProjectID: request.ProjectId,
		HostID: request.HostId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey,
		PathPrefix: request.Body.PathPrefix, DeploymentTargetID: request.Body.DeploymentTargetId})
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidRoute):
			return api.CreateAccessRoute400JSONResponse{Code: "invalid_access_route", Message: "access route is invalid"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.CreateAccessRoute403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access routes"}, nil
		case errors.Is(err, projectauth.ErrNotMember), errors.Is(err, access.ErrHostNotFound), errors.Is(err, access.ErrRouteNotFound):
			return api.CreateAccessRoute404JSONResponse{Code: "access_resource_not_found", Message: "access host or target not found"}, nil
		case errors.Is(err, access.ErrConflict), errors.Is(err, idempotency.ErrConflict):
			return api.CreateAccessRoute409JSONResponse{Code: "access_route_conflict", Message: "access route or idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.CreateAccessRoute201JSONResponse(accessRouteResponse(route)), nil
}

func (s *Server) GetAccessRoute(ctx context.Context, request api.GetAccessRouteRequestObject) (api.GetAccessRouteResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	route, err := s.access.GetRoute(httpRequestContext(ctx), request.ProjectId, request.HostId, request.RouteId, requestCaller(ctx))
	if err != nil {
		if errors.Is(err, access.ErrRouteNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.GetAccessRoute404JSONResponse{Code: "access_route_not_found", Message: "access route not found"}, nil
		}
		return nil, err
	}
	return api.GetAccessRoute200JSONResponse(accessRouteResponse(route)), nil
}

func (s *Server) UpdateAccessHost(ctx context.Context, request api.UpdateAccessHostRequestObject) (api.UpdateAccessHostResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	host, err := s.access.UpdateHost(httpRequestContext(ctx), access.UpdateHostCommand{ProjectID: request.ProjectId,
		HostID: request.HostId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey,
		Input: access.HostInput{Hostname: request.Body.Hostname, TLSMode: string(request.Body.TlsMode),
			IssuerPolicyKey: request.Body.IssuerPolicyKey, SecretBindingID: request.Body.SecretBindingId}})
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidHost):
			return api.UpdateAccessHost400JSONResponse{Code: "invalid_access_host", Message: "access host is invalid"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.UpdateAccessHost403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access hosts"}, nil
		case errors.Is(err, access.ErrHostNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.UpdateAccessHost404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		case errors.Is(err, access.ErrConflict), errors.Is(err, idempotency.ErrConflict):
			return api.UpdateAccessHost409JSONResponse{Code: "access_host_conflict", Message: "access host or idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.UpdateAccessHost200JSONResponse(accessHostResponse(host)), nil
}
func (s *Server) DeleteAccessHost(ctx context.Context, request api.DeleteAccessHostRequestObject) (api.DeleteAccessHostResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	host, err := s.access.DeleteHost(httpRequestContext(ctx), access.DeleteCommand{ProjectID: request.ProjectId,
		HostID: request.HostId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrForbidden):
			return api.DeleteAccessHost403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access hosts"}, nil
		case errors.Is(err, access.ErrHostNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.DeleteAccessHost404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.DeleteAccessHost409JSONResponse{Code: "idempotency_conflict", Message: "idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.DeleteAccessHost202JSONResponse(accessHostResponse(host)), nil
}
func (s *Server) UpdateAccessRoute(ctx context.Context, request api.UpdateAccessRouteRequestObject) (api.UpdateAccessRouteResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	route, err := s.access.UpdateRoute(httpRequestContext(ctx), access.UpdateRouteCommand{ProjectID: request.ProjectId,
		HostID: request.HostId, RouteID: request.RouteId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey,
		PathPrefix: request.Body.PathPrefix, DeploymentTargetID: request.Body.DeploymentTargetId})
	if err != nil {
		switch {
		case errors.Is(err, access.ErrInvalidRoute):
			return api.UpdateAccessRoute400JSONResponse{Code: "invalid_access_route", Message: "access route is invalid"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.UpdateAccessRoute403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access routes"}, nil
		case errors.Is(err, access.ErrHostNotFound), errors.Is(err, access.ErrRouteNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.UpdateAccessRoute404JSONResponse{Code: "access_route_not_found", Message: "access route or target not found"}, nil
		case errors.Is(err, access.ErrConflict), errors.Is(err, idempotency.ErrConflict):
			return api.UpdateAccessRoute409JSONResponse{Code: "access_route_conflict", Message: "access route or idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.UpdateAccessRoute200JSONResponse(accessRouteResponse(route)), nil
}
func (s *Server) DeleteAccessRoute(ctx context.Context, request api.DeleteAccessRouteRequestObject) (api.DeleteAccessRouteResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	route, err := s.access.DeleteRoute(httpRequestContext(ctx), access.DeleteCommand{ProjectID: request.ProjectId,
		HostID: request.HostId, RouteID: request.RouteId, Caller: requestCaller(ctx), IdempotencyKey: request.Params.IdempotencyKey})
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrForbidden):
			return api.DeleteAccessRoute403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage access routes"}, nil
		case errors.Is(err, access.ErrRouteNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.DeleteAccessRoute404JSONResponse{Code: "access_route_not_found", Message: "access route not found"}, nil
		case errors.Is(err, idempotency.ErrConflict):
			return api.DeleteAccessRoute409JSONResponse{Code: "idempotency_conflict", Message: "idempotency key conflicts"}, nil
		default:
			return nil, err
		}
	}
	return api.DeleteAccessRoute202JSONResponse(accessRouteResponse(route)), nil
}
