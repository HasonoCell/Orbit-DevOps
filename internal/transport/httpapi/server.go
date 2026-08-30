package httpapi

import (
	"context"
	"errors"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/project"
	"github.com/gin-gonic/gin"
)

type Server struct {
	projects     *project.Module
	localActorID string
}

func (s *Server) GetProject(
	ctx context.Context,
	request api.GetProjectRequestObject,
) (api.GetProjectResponseObject, error) {
	requestContext := ctx
	if ginContext, ok := ctx.(*gin.Context); ok {
		requestContext = ginContext.Request.Context()
	}

	existingProject, err := s.projects.Get(requestContext, request.ProjectId)
	if err != nil {
		if errors.Is(err, project.ErrNotFound) {
			return api.GetProject404JSONResponse{
				Code:    "project_not_found",
				Message: "project not found",
			}, nil
		}
		return nil, err
	}

	return api.GetProject200JSONResponse{
		Id:        existingProject.ID,
		Name:      existingProject.Name,
		Slug:      existingProject.Slug,
		CreatedBy: existingProject.CreatedBy,
		CreatedAt: existingProject.CreatedAt,
	}, nil
}

func NewServer(projects *project.Module, localActorID string) *Server {
	return &Server{
		projects:     projects,
		localActorID: localActorID,
	}
}

func (s *Server) CreateProject(
	ctx context.Context,
	request api.CreateProjectRequestObject,
) (api.CreateProjectResponseObject, error) {
	requestContext := ctx
	if ginContext, ok := ctx.(*gin.Context); ok {
		requestContext = ginContext.Request.Context()
	}

	createdProject, err := s.projects.Create(requestContext, project.CreateCommand{
		Name:           request.Body.Name,
		Slug:           request.Body.Slug,
		ActorID:        s.localActorID,
		IdempotencyKey: request.Params.IdempotencyKey,
	})
	if err != nil {
		if errors.Is(err, project.ErrIdempotencyConflict) {
			return api.CreateProject409JSONResponse{
				Code:    "idempotency_conflict",
				Message: "idempotency key was already used with a different request",
			}, nil
		}
		return nil, err
	}

	return api.CreateProject201JSONResponse{
		Id:        createdProject.ID,
		Name:      createdProject.Name,
		Slug:      createdProject.Slug,
		CreatedBy: createdProject.CreatedBy,
		CreatedAt: createdProject.CreatedAt,
	}, nil
}
