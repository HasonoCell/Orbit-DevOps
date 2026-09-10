package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/pipeline"
)

type fixedSourceInspector struct{}

func (fixedSourceInspector) Resolve(_ context.Context, request pipeline.SourceRequest) (pipeline.SourceIdentity, error) {
	return pipeline.SourceIdentity{
		RepositoryID: 101, OwnerID: 202, RepositoryName: "example/orbitops-demo",
		RepositoryURL: "https://github.com/example/orbitops-demo.git",
		GitRef:        "refs/heads/" + request.Branch, HeadCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, nil
}

type deliveryPipelineDocument struct {
	Pipeline struct {
		ID                   string `json:"id"`
		ProjectID            string `json:"projectId"`
		CurrentRevision      int    `json:"currentRevision"`
		Enabled              bool   `json:"enabled"`
		ActivationGeneration int64  `json:"activationGeneration"`
	} `json:"pipeline"`
	Revision struct {
		Revision           int     `json:"revision"`
		RepositoryID       int64   `json:"repositoryId"`
		RepositoryOwnerID  int64   `json:"repositoryOwnerId"`
		GitRef             string  `json:"gitRef"`
		Mode               string  `json:"mode"`
		DeploymentTargetID *string `json:"deploymentTargetId"`
	} `json:"revision"`
}

func TestDeliveryPipelineRevisionAndActivationGeneration(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{GitSourceInspector: fixedSourceInspector{}})
	project := createProject(t, environment, "pipeline-project")
	application := createApplication(t, environment, project.ID, "pipeline-application")
	targetResponse := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/deployment-targets", "pipeline-target", `{"stage":"development","replicas":1,"containerPort":8080}`)
	defer targetResponse.Body.Close()
	if targetResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline target status = %d", targetResponse.StatusCode)
	}
	target := decodeDeploymentTarget(t, targetResponse)

	createBody := `{"name":"main","endpointKey":"public","repositoryUrl":"https://github.com/example/orbitops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"` + target.ID + `"}`
	createResponse := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/delivery-pipelines", "pipeline-create-v1", createBody)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline status = %d", createResponse.StatusCode)
	}
	created := decodeDeliveryPipeline(t, createResponse)
	if created.Pipeline.Enabled || created.Pipeline.CurrentRevision != 1 || created.Revision.RepositoryID != 101 {
		t.Fatalf("created pipeline = %#v", created)
	}

	updateBody := `{"expectedRevision":1,"endpointKey":"public","repositoryUrl":"https://github.com/example/orbitops-demo.git","branch":"release","mode":"build_only"}`
	updateResponse := environment.putJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID, "pipeline-update-v1", updateBody)
	defer updateResponse.Body.Close()
	if updateResponse.StatusCode != http.StatusOK {
		t.Fatalf("update pipeline status = %d", updateResponse.StatusCode)
	}
	updated := decodeDeliveryPipeline(t, updateResponse)
	if updated.Pipeline.CurrentRevision != 2 || updated.Revision.Revision != 2 || updated.Revision.GitRef != "refs/heads/release" || updated.Revision.Mode != "build_only" {
		t.Fatalf("updated pipeline = %#v", updated)
	}

	enabledResponse := environment.postJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID+"/enable", "pipeline-enable-v1", "")
	defer enabledResponse.Body.Close()
	if enabledResponse.StatusCode != http.StatusOK {
		t.Fatalf("enable pipeline status = %d", enabledResponse.StatusCode)
	}
	enabled := decodeDeliveryPipeline(t, enabledResponse)
	if !enabled.Pipeline.Enabled || enabled.Pipeline.ActivationGeneration != 1 {
		t.Fatalf("enabled pipeline = %#v", enabled)
	}

	disabledResponse := environment.postJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID+"/disable", "pipeline-disable-v1", "")
	defer disabledResponse.Body.Close()
	if disabledResponse.StatusCode != http.StatusOK {
		t.Fatalf("disable pipeline status = %d", disabledResponse.StatusCode)
	}
	disabled := decodeDeliveryPipeline(t, disabledResponse)
	if disabled.Pipeline.Enabled || disabled.Pipeline.ActivationGeneration != 2 {
		t.Fatalf("disabled pipeline = %#v", disabled)
	}
}

func decodeDeliveryPipeline(t *testing.T, response *http.Response) deliveryPipelineDocument {
	t.Helper()
	var document deliveryPipelineDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode delivery pipeline: %v", err)
	}
	return document
}
