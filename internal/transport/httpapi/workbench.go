package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/catalog"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
)

// ListApplicationWorkbench 将应用游标页与数据库批量摘要组合为一个只读入口。
// 不把 Kubernetes 观测加入列表请求；细粒度运行诊断仍由应用页按目标读取。
func (s *Server) ListApplicationWorkbench(ctx context.Context,
	request api.ListApplicationWorkbenchRequestObject) (api.ListApplicationWorkbenchResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	limit := 8
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	if limit < 1 || limit > 20 {
		return api.ListApplicationWorkbench400JSONResponse{Code: "invalid_limit", Message: "workbench page limit is invalid"}, nil
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	requestContext := httpRequestContext(ctx)
	caller := requestCaller(ctx)
	page, err := s.catalog.ListApplications(requestContext, request.ProjectId, caller, limit, cursor)
	if err != nil {
		switch {
		case errors.Is(err, catalog.ErrInvalidCursor):
			return api.ListApplicationWorkbench400JSONResponse{Code: "invalid_cursor", Message: "application cursor is invalid"}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ListApplicationWorkbench404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		default:
			return nil, err
		}
	}
	ids := make([]uuid.UUID, 0, len(page.Items))
	for _, application := range page.Items {
		ids = append(ids, application.ID)
	}
	summaries, err := s.workbench.Summarize(requestContext, request.ProjectId, caller, ids)
	if err != nil {
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.ListApplicationWorkbench404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		}
		return nil, err
	}
	items := make([]api.ApplicationWorkbenchItem, 0, len(page.Items))
	for _, application := range page.Items {
		summary := summaries[application.ID]
		item := api.ApplicationWorkbenchItem{
			Application: applicationResponse(application),
			Targets:     make([]api.WorkbenchTarget, 0, len(summary.Targets)),
			Pipelines:   make([]api.WorkbenchPipeline, 0, len(summary.Pipelines)),
		}
		if summary.Build != nil {
			item.Build = &api.WorkbenchBuild{
				Status: api.WorkbenchBuildStatus(summary.Build.Status), CreatedAt: summary.Build.CreatedAt,
			}
		}
		for _, target := range summary.Targets {
			converted := api.WorkbenchTarget{Id: target.ID, Stage: api.WorkbenchTargetStage(target.Stage)}
			if target.ReleaseStatus != nil {
				status := api.WorkbenchTargetReleaseStatus(*target.ReleaseStatus)
				converted.ReleaseStatus = &status
			}
			item.Targets = append(item.Targets, converted)
		}
		for _, pipeline := range summary.Pipelines {
			converted := api.WorkbenchPipeline{
				Id: pipeline.ID, Name: pipeline.Name, RunCreatedAt: pipeline.RunCreatedAt,
			}
			if pipeline.RunStatus != nil {
				status := api.WorkbenchPipelineRunStatus(*pipeline.RunStatus)
				converted.RunStatus = &status
			}
			item.Pipelines = append(item.Pipelines, converted)
		}
		items = append(items, item)
	}
	return api.ListApplicationWorkbench200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}
