package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/catalog"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

func (s *Server) ListProjects(ctx context.Context, request api.ListProjectsRequestObject) (api.ListProjectsResponseObject, error) {
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.projects.List(httpRequestContext(ctx), requestCaller(ctx), limit, cursor)
	if err != nil {
		if errors.Is(err, project.ErrInvalidCursor) {
			return api.ListProjects400JSONResponse{Code: "invalid_cursor", Message: "project cursor is invalid"}, nil
		}
		return nil, err
	}
	items := make([]api.Project, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, projectResponse(item))
	}
	return api.ListProjects200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) GetProjectPermissions(ctx context.Context, request api.GetProjectPermissionsRequestObject) (api.GetProjectPermissionsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	permissions, err := s.authorizer.GetPermissions(httpRequestContext(ctx), request.ProjectId, requestCaller(ctx))
	if err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.GetProjectPermissions404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		}
		return nil, err
	}
	allowed := make([]api.ProjectPermission, 0, len(permissions.Allowed))
	for _, permission := range permissions.Allowed {
		allowed = append(allowed, api.ProjectPermission(permission))
	}
	return api.GetProjectPermissions200JSONResponse{ProjectId: permissions.ProjectID,
		Role: api.ProjectRole(permissions.Role), Allowed: allowed}, nil
}

func (s *Server) ListApplications(ctx context.Context, request api.ListApplicationsRequestObject) (api.ListApplicationsResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.catalog.ListApplications(httpRequestContext(ctx), request.ProjectId, requestCaller(ctx), limit, cursor)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrInvalidCursor):
			return api.ListApplications400JSONResponse{Code: "invalid_cursor", Message: "application cursor is invalid"}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ListApplications404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		default:
			return nil, err
		}
	}
	items := make([]api.Application, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, applicationResponse(item))
	}
	return api.ListApplications200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func (s *Server) ListDeploymentTargets(ctx context.Context, request api.ListDeploymentTargetsRequestObject) (api.ListDeploymentTargetsResponseObject, error) {
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.catalog.ListDeploymentTargets(httpRequestContext(ctx), request.ApplicationId, requestCaller(ctx), limit, cursor)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrInvalidCursor):
			return api.ListDeploymentTargets400JSONResponse{Code: "invalid_cursor", Message: "deployment target cursor is invalid"}, nil
		case errors.Is(err, catalog.ErrApplicationNotFound), errors.Is(err, projectauth.ErrNotMember):
			return api.ListDeploymentTargets404JSONResponse{Code: "application_not_found", Message: "application not found"}, nil
		default:
			return nil, err
		}
	}
	items := make([]api.DeploymentTarget, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, deploymentTargetResponse(item))
	}
	if len(page.Items) > 0 {
		observability.SetRequestProjectID(ctx, page.Items[0].ProjectID)
	}
	return api.ListDeploymentTargets200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

func projectListParameters(limitValue *int, cursorValue *string) (int, string) {
	limit, cursor := 20, ""
	if limitValue != nil {
		limit = *limitValue
	}
	if cursorValue != nil {
		cursor = *cursorValue
	}
	return limit, cursor
}

func projectResponse(item project.Project) api.Project {
	return api.Project{Id: item.ID, Name: item.Name, Slug: item.Slug, CreatedBy: item.CreatedBy, CreatedAt: item.CreatedAt}
}
