package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

// ListAccessEligibleTargets 只暴露当前 Host 网络边界内的目标，不让浏览器拼接项目全量目录。
func (s *Server) ListAccessEligibleTargets(ctx context.Context, request api.ListAccessEligibleTargetsRequestObject) (api.ListAccessEligibleTargetsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	items, err := s.access.ListEligibleTargets(httpRequestContext(ctx), request.ProjectId, request.HostId, requestCaller(ctx), accessPage(request.Params.Limit, request.Params.Offset))
	if err != nil {
		if errors.Is(err, access.ErrHostNotFound) || errors.Is(err, projectauth.ErrNotMember) {
			return api.ListAccessEligibleTargets404JSONResponse{Code: "access_host_not_found", Message: "access host not found"}, nil
		}
		return nil, err
	}
	response := make(api.ListAccessEligibleTargets200JSONResponse, 0, len(items))
	for _, item := range items {
		response = append(response, api.AccessEligibleTarget{Id: item.ID, ApplicationId: item.ApplicationID,
			ApplicationName: item.ApplicationName, Stage: api.AccessEligibleTargetStage(item.Stage),
			ClusterRef: item.ClusterRef, Namespace: item.Namespace})
	}
	return response, nil
}

// ListTargetAccessRoutes 经目标项目权限过滤后返回关联 Host/Route，包含异步清理状态。
func (s *Server) ListTargetAccessRoutes(ctx context.Context, request api.ListTargetAccessRoutesRequestObject) (api.ListTargetAccessRoutesResponseObject, error) {
	items, err := s.access.ListTargetRoutes(httpRequestContext(ctx), request.DeploymentTargetId, requestCaller(ctx), accessPage(request.Params.Limit, request.Params.Offset))
	if err != nil {
		if errors.Is(err, access.ErrTargetNotFound) {
			return api.ListTargetAccessRoutes404JSONResponse{Code: "deployment_target_not_found", Message: "deployment target not found"}, nil
		}
		return nil, err
	}
	response := make(api.ListTargetAccessRoutes200JSONResponse, 0, len(items))
	for _, item := range items {
		response = append(response, api.AccessTargetRoute{RouteId: item.RouteID, HostId: item.HostID,
			Hostname: item.Hostname, PathPrefix: item.PathPrefix,
			RouteLifecycle: api.AccessTargetRouteRouteLifecycle(item.RouteLifecycle),
			HostLifecycle:  api.AccessTargetRouteHostLifecycle(item.HostLifecycle)})
	}
	return response, nil
}
