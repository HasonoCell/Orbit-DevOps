package integration_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/api"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
)

func TestApplicationWorkbenchBatchesCurrentPage(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{GitSourceInspector: fixedSourceInspector{}})
	project := createProject(t, environment, "workbench-project")
	first := createApplication(t, environment, project.ID, "workbench-first")
	second := createDistinctApplication(t, environment, project.ID, "workbench-second", "second")
	target := createTargetForApplication(t, environment, first.ID, "workbench-target")
	build := createBuildForArtifact(t, environment, first.ID, "workbench-build", "a")
	pipelineResponse := environment.postJSON(t, "/api/v1/applications/"+first.ID+"/delivery-pipelines",
		"workbench-pipeline", `{"name":"main","endpointKey":"workbench","repositoryUrl":"https://github.com/example/orbit-devops-demo.git","branch":"main","mode":"build_only"}`)
	defer pipelineResponse.Body.Close()
	if pipelineResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline status = %d, want 201", pipelineResponse.StatusCode)
	}
	pipeline := decodeDeliveryPipeline(t, pipelineResponse)

	path := "/api/v1/projects/" + project.ID + "/application-workbench?limit=1"
	firstPage := getWorkbenchPage(t, environment, path)
	if len(firstPage.Items) != 1 || firstPage.NextCursor == nil {
		t.Fatalf("first page = %#v, want one item and next cursor", firstPage)
	}
	secondPage := getWorkbenchPage(t, environment, path+"&cursor="+url.QueryEscape(*firstPage.NextCursor))
	if len(secondPage.Items) != 1 || secondPage.NextCursor != nil {
		t.Fatalf("second page = %#v, want one terminal item", secondPage)
	}
	items := map[string]api.ApplicationWorkbenchItem{}
	for _, item := range append(firstPage.Items, secondPage.Items...) {
		items[item.Application.Id.String()] = item
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want two distinct applications", items)
	}
	if got := items[first.ID]; len(got.Targets) != 1 || got.Targets[0].Id.String() != target.ID ||
		got.Build == nil || string(got.Build.Status) != build.BuildOperation.Status ||
		len(got.Pipelines) != 1 || got.Pipelines[0].Id.String() != pipeline.Pipeline.ID ||
		got.Pipelines[0].RunStatus != nil {
		t.Errorf("first application summary = %#v", got)
	}
	if got := items[second.ID]; len(got.Targets) != 0 || got.Build != nil || len(got.Pipelines) != 0 {
		t.Errorf("second application summary = %#v, want empty records", got)
	}

	for _, limit := range []string{"0", "21"} {
		response := environment.get(t, path+"&limit="+limit)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("limit %s status = %d, want 400", limit, response.StatusCode)
		}
	}
}

func getWorkbenchPage(t *testing.T, environment *testEnvironment, path string) api.ApplicationWorkbenchPage {
	t.Helper()
	response := environment.get(t, path)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("workbench status = %d, want 200", response.StatusCode)
	}
	var page api.ApplicationWorkbenchPage
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode workbench: %v", err)
	}
	return page
}
