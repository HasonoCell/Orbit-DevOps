package integration_test

import (
	"net/http"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
)

func TestPipelineRejectsProductionAutoRelease(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{GitSourceInspector: fixedSourceInspector{}})
	project := createProject(t, environment, "stage-pipeline-project")
	application := createApplication(t, environment, project.ID, "stage-pipeline-application")
	targetPath := "/api/v1/applications/" + application.ID + "/deployment-targets"
	developmentResponse := environment.postJSON(t, targetPath, "stage-pipeline-development", `{"stage":"development","replicas":1,"containerPort":8080}`)
	development := decodeDeploymentTarget(t, developmentResponse)
	developmentResponse.Body.Close()
	productionResponse := environment.postJSON(t, targetPath, "stage-pipeline-production", `{"stage":"production","replicas":1,"containerPort":8080}`)
	production := decodeDeploymentTarget(t, productionResponse)
	productionResponse.Body.Close()
	pipelinePath := "/api/v1/applications/" + application.ID + "/delivery-pipelines"
	createBody := `{"name":"main","endpointKey":"integration","repositoryUrl":"https://github.com/example/orbit-devops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"` + production.ID + `"}`
	rejectedCreate := environment.postJSON(t, pipelinePath, "stage-pipeline-invalid-create", createBody)
	defer rejectedCreate.Body.Close()
	assertError(t, rejectedCreate, http.StatusBadRequest, "auto_release_target_stage_forbidden")

	createBody = `{"name":"main","endpointKey":"integration","repositoryUrl":"https://github.com/example/orbit-devops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"` + development.ID + `"}`
	createdResponse := environment.postJSON(t, pipelinePath, "stage-pipeline-valid-create", createBody)
	if createdResponse.StatusCode != http.StatusCreated {
		createdResponse.Body.Close()
		t.Fatalf("create development pipeline status = %d", createdResponse.StatusCode)
	}
	created := decodeDeliveryPipeline(t, createdResponse)
	createdResponse.Body.Close()
	updateBody := `{"expectedRevision":1,"endpointKey":"integration","repositoryUrl":"https://github.com/example/orbit-devops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"` + production.ID + `"}`
	rejectedUpdate := environment.putJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID, "stage-pipeline-invalid-update", updateBody)
	defer rejectedUpdate.Body.Close()
	assertError(t, rejectedUpdate, http.StatusBadRequest, "auto_release_target_stage_forbidden")

	// 模拟旧 Revision 或数据库异常数据，启用入口仍须重新核验目的 Stage。
	database := openTestDatabase(t, environment.databaseURL)
	if _, err := database.Exec(`UPDATE delivery_pipeline_revisions SET deployment_target_id=$1
		WHERE delivery_pipeline_id=$2 AND revision=1`, production.ID, created.Pipeline.ID); err != nil {
		t.Fatal(err)
	}
	rejectedEnable := environment.postJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID+"/enable", "stage-pipeline-invalid-enable", "")
	defer rejectedEnable.Body.Close()
	assertError(t, rejectedEnable, http.StatusBadRequest, "auto_release_target_stage_forbidden")
}
