package httpapi

import (
	"context"

	"github.com/HasonoCell/OrbitOps/internal/api"
	"github.com/HasonoCell/OrbitOps/internal/project"
	"github.com/gin-gonic/gin"
)

type Server struct {
	projects     *project.Module
	localActorID string
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
