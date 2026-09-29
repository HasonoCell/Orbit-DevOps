package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
)

// ListProjectMembers 返回当前操作者可见的项目成员列表。
func (s *Server) ListProjectMembers(
	ctx context.Context,
	request api.ListProjectMembersRequestObject,
) (api.ListProjectMembersResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	limit, cursor := projectListParameters(request.Params.Limit, request.Params.Cursor)
	page, err := s.authorizer.ListMembers(
		httpRequestContext(ctx),
		request.ProjectId,
		requestCaller(ctx),
		limit,
		cursor,
	)
	if err != nil {
		if errors.Is(err, projectauth.ErrInvalidCursor) {
			return api.ListProjectMembers400JSONResponse{Code: "invalid_cursor", Message: "project member cursor is invalid"}, nil
		}
		if errors.Is(err, projectauth.ErrNotMember) {
			return api.ListProjectMembers404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		}
		return nil, err
	}

	items := make([]api.ProjectMember, 0, len(page.Items))
	for _, member := range page.Items {
		items = append(items, projectMemberResponse(member))
	}
	return api.ListProjectMembers200JSONResponse{Items: items, NextCursor: page.NextCursor}, nil
}

// ResolveProjectMemberCandidate 仅向能管理该项目成员的操作者返回精确、安全的用户投影。
func (s *Server) ResolveProjectMemberCandidate(ctx context.Context, request api.ResolveProjectMemberCandidateRequestObject) (api.ResolveProjectMemberCandidateResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	result, err := s.authorizer.ResolveMemberCandidate(httpRequestContext(ctx), request.ProjectId,
		requestCaller(ctx), string(request.Body.Kind), request.Body.Value)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrInvalidMember):
			return api.ResolveProjectMemberCandidate400JSONResponse{Code: "invalid_member_lookup", Message: "member lookup input is invalid"}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.ResolveProjectMemberCandidate403JSONResponse{Code: "project_permission_denied", Message: "current project role cannot manage members"}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.ResolveProjectMemberCandidate404JSONResponse{Code: "project_not_found", Message: "project not found"}, nil
		default:
			return nil, err
		}
	}
	response := api.ProjectMemberCandidateResult{Status: api.ProjectMemberCandidateResultStatus(result.Status)}
	if result.Candidate != nil {
		response.Candidate = &api.ProjectMemberCandidate{UserId: result.Candidate.UserID, DisplayName: result.Candidate.DisplayName}
	}
	return api.ResolveProjectMemberCandidate200JSONResponse(response), nil
}

// AddProjectMember 添加项目成员；权限、幂等和审计由项目权限模块统一处理。
func (s *Server) AddProjectMember(
	ctx context.Context,
	request api.AddProjectMemberRequestObject,
) (api.AddProjectMemberResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	member, err := s.authorizer.AddMember(
		httpRequestContext(ctx),
		projectauth.AddMemberCommand{
			ProjectID:      request.ProjectId,
			UserID:         request.Body.UserId,
			Role:           string(request.Body.Role),
			Caller:         requestCaller(ctx),
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrInvalidMember), errors.Is(err, projectauth.ErrInvalidRole):
			return api.AddProjectMember400JSONResponse{
				Code:    "invalid_project_member",
				Message: "user ID or project role is invalid",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.AddProjectMember403JSONResponse{
				Code:    "project_permission_denied",
				Message: "current project role cannot manage members",
			}, nil
		case errors.Is(err, projectauth.ErrNotMember):
			return api.AddProjectMember404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		case errors.Is(err, projectauth.ErrMemberExists):
			return api.AddProjectMember409JSONResponse{
				Code:    "project_member_exists",
				Message: "project member already exists",
			}, nil
		case errors.Is(err, projectauth.ErrIdempotencyConflict):
			return api.AddProjectMember409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	return api.AddProjectMember201JSONResponse(projectMemberResponse(member)), nil
}

// UpdateProjectMember 修改项目成员角色，并保护项目最后一个 owner。
func (s *Server) UpdateProjectMember(
	ctx context.Context,
	request api.UpdateProjectMemberRequestObject,
) (api.UpdateProjectMemberResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	member, err := s.authorizer.UpdateMember(
		httpRequestContext(ctx),
		projectauth.UpdateMemberCommand{
			ProjectID:      request.ProjectId,
			UserID:         request.UserId,
			Role:           string(request.Body.Role),
			Caller:         requestCaller(ctx),
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrInvalidMember), errors.Is(err, projectauth.ErrInvalidRole):
			return api.UpdateProjectMember400JSONResponse{
				Code:    "invalid_project_member",
				Message: "user ID or project role is invalid",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.UpdateProjectMember403JSONResponse{
				Code:    "project_permission_denied",
				Message: "current project role cannot manage members",
			}, nil
		case errors.Is(err, projectauth.ErrNotMember), errors.Is(err, projectauth.ErrMemberNotFound):
			return api.UpdateProjectMember404JSONResponse{
				Code:    "project_member_not_found",
				Message: "project or project member not found",
			}, nil
		case errors.Is(err, projectauth.ErrLastOwner):
			return api.UpdateProjectMember409JSONResponse{
				Code:    "last_project_owner",
				Message: "project must retain at least one owner",
			}, nil
		case errors.Is(err, projectauth.ErrIdempotencyConflict):
			return api.UpdateProjectMember409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	return api.UpdateProjectMember200JSONResponse(projectMemberResponse(member)), nil
}

// RemoveProjectMember 移除项目成员，并返回被移除的不可变命令结果。
func (s *Server) RemoveProjectMember(
	ctx context.Context,
	request api.RemoveProjectMemberRequestObject,
) (api.RemoveProjectMemberResponseObject, error) {
	observability.SetRequestProjectID(ctx, request.ProjectId)
	member, err := s.authorizer.RemoveMember(
		httpRequestContext(ctx),
		projectauth.RemoveMemberCommand{
			ProjectID:      request.ProjectId,
			UserID:         request.UserId,
			Caller:         requestCaller(ctx),
			IdempotencyKey: request.Params.IdempotencyKey,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, projectauth.ErrInvalidMember):
			return api.RemoveProjectMember400JSONResponse{
				Code:    "invalid_project_member",
				Message: "user ID is invalid",
			}, nil
		case errors.Is(err, projectauth.ErrForbidden):
			return api.RemoveProjectMember403JSONResponse{
				Code:    "project_permission_denied",
				Message: "current project role cannot manage members",
			}, nil
		case errors.Is(err, projectauth.ErrNotMember), errors.Is(err, projectauth.ErrMemberNotFound):
			return api.RemoveProjectMember404JSONResponse{
				Code:    "project_member_not_found",
				Message: "project or project member not found",
			}, nil
		case errors.Is(err, projectauth.ErrLastOwner):
			return api.RemoveProjectMember409JSONResponse{
				Code:    "last_project_owner",
				Message: "project must retain at least one owner",
			}, nil
		case errors.Is(err, projectauth.ErrIdempotencyConflict):
			return api.RemoveProjectMember409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		default:
			return nil, err
		}
	}
	return api.RemoveProjectMember200JSONResponse(projectMemberResponse(member)), nil
}

func projectMemberResponse(member projectauth.Member) api.ProjectMember {
	response := api.ProjectMember{
		ProjectId: member.ProjectID,
		UserId:    member.UserID,
		Role:      api.ProjectRole(member.Role),
		CreatedBy: member.CreatedBy,
		CreatedAt: member.CreatedAt,
		UpdatedAt: member.UpdatedAt,
	}
	if member.DisplayName != "" {
		response.DisplayName = &member.DisplayName
	}
	return response
}
