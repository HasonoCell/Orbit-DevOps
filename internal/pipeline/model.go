package pipeline

import (
	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"time"

	"github.com/google/uuid"
)

const (
	ProviderGitHub  = "github"
	ModeBuildOnly   = "build_only"
	ModeAutoRelease = "auto_release"
)

type Record struct {
	ID                   uuid.UUID `db:"id" json:"id"`
	ProjectID            uuid.UUID `db:"project_id" json:"projectId"`
	ApplicationID        uuid.UUID `db:"application_id" json:"applicationId"`
	Name                 string    `db:"name" json:"name"`
	CurrentRevision      int       `db:"current_revision" json:"currentRevision"`
	Enabled              bool      `db:"enabled" json:"enabled"`
	ActivationGeneration int64     `db:"activation_generation" json:"activationGeneration"`
	CreatedBy            string    `db:"created_by" json:"createdBy"`
	CreatedAt            time.Time `db:"created_at" json:"createdAt"`
	UpdatedAt            time.Time `db:"updated_at" json:"updatedAt"`
}

// Revision 冻结一次经过 Provider 核验的交付配置，历史 Run 永远引用原 Revision。
type Revision struct {
	PipelineID         uuid.UUID  `db:"delivery_pipeline_id" json:"deliveryPipelineId"`
	Revision           int        `db:"revision" json:"revision"`
	Provider           string     `db:"provider" json:"provider"`
	EndpointKey        string     `db:"endpoint_key" json:"endpointKey"`
	RepositoryID       int64      `db:"repository_id" json:"repositoryId"`
	RepositoryOwnerID  int64      `db:"repository_owner_id" json:"repositoryOwnerId"`
	RepositoryFullName string     `db:"repository_full_name" json:"repositoryFullName"`
	RepositoryURL      string     `db:"repository_url" json:"repositoryUrl"`
	GitRef             string     `db:"git_ref" json:"gitRef"`
	DockerfilePath     string     `db:"dockerfile_path" json:"dockerfilePath"`
	ContextPath        string     `db:"context_path" json:"contextPath"`
	Platform           string     `db:"platform" json:"platform"`
	Mode               string     `db:"mode" json:"mode"`
	DeploymentTargetID *uuid.UUID `db:"deployment_target_id" json:"deploymentTargetId,omitempty"`
	CreatedBy          string     `db:"revision_created_by" json:"createdBy"`
	CreatedAt          time.Time  `db:"revision_created_at" json:"createdAt"`
}

type Detail struct {
	Pipeline Record   `json:"pipeline"`
	Revision Revision `json:"revision"`
}

type CreateCommand struct {
	ApplicationID      uuid.UUID
	Name               string
	EndpointKey        string
	RepositoryURL      string
	Branch             string
	DockerfilePath     string
	ContextPath        string
	Mode               string
	DeploymentTargetID *uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
}

type UpdateCommand struct {
	PipelineID         uuid.UUID
	ExpectedRevision   int
	EndpointKey        string
	RepositoryURL      string
	Branch             string
	DockerfilePath     string
	ContextPath        string
	Mode               string
	DeploymentTargetID *uuid.UUID
	Caller             identity.Caller
	IdempotencyKey     string
}

type StateCommand struct {
	PipelineID     uuid.UUID
	Caller         identity.Caller
	IdempotencyKey string
}
