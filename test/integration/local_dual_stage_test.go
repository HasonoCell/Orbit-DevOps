package integration_test

import (
	"net/http"
	"strings"
	"testing"
)

// 同一应用的两个逻辑阶段共享受控集群与 Namespace，但 Target 身份和发布历史必须独立。
func TestLocalDualStageTargetsAndManualRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "dual-stage-project")
	application := createApplication(t, environment, project.ID, "dual-stage-application")
	path := "/api/v1/applications/" + application.ID + "/deployment-targets"

	developmentResponse := environment.postJSON(t, path, "dual-stage-development", `{"stage":"development","replicas":1,"containerPort":8080}`)
	if developmentResponse.StatusCode != http.StatusCreated {
		developmentResponse.Body.Close()
		t.Fatalf("create development target status = %d", developmentResponse.StatusCode)
	}
	development := decodeDeploymentTarget(t, developmentResponse)
	developmentResponse.Body.Close()
	productionResponse := environment.postJSON(t, path, "dual-stage-production", `{"stage":"production","replicas":2,"containerPort":8080}`)
	if productionResponse.StatusCode != http.StatusCreated {
		productionResponse.Body.Close()
		t.Fatalf("create production target status = %d", productionResponse.StatusCode)
	}
	production := decodeDeploymentTarget(t, productionResponse)
	productionResponse.Body.Close()
	if production.Stage != "production" || development.ID == production.ID ||
		development.ClusterRef != production.ClusterRef || development.Namespace != production.Namespace {
		t.Fatalf("dual-stage targets = dev %#v, prod %#v", development, production)
	}

	duplicate := environment.postJSON(t, path, "dual-stage-production-duplicate", `{"stage":"production","replicas":2,"containerPort":8080}`)
	defer duplicate.Body.Close()
	assertError(t, duplicate, http.StatusConflict, "deployment_target_stage_conflict")
	invalid := environment.postJSON(t, path, "dual-stage-invalid", `{"stage":"staging","replicas":1,"containerPort":8080}`)
	defer invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid stage status = %d, want 400", invalid.StatusCode)
	}
	changeStage := environment.putJSON(t, "/api/v1/deployment-targets/"+production.ID, "dual-stage-change-stage", `{"stage":"development","replicas":2,"containerPort":8080}`)
	defer changeStage.Body.Close()
	if changeStage.StatusCode != http.StatusBadRequest {
		t.Fatalf("change stage status = %d, want 400", changeStage.StatusCode)
	}
	update := environment.putJSON(t, "/api/v1/deployment-targets/"+production.ID, "dual-stage-update-production", `{"replicas":3,"containerPort":9090}`)
	if update.StatusCode != http.StatusOK {
		update.Body.Close()
		t.Fatalf("update production target status = %d", update.StatusCode)
	}
	updatedProduction := decodeDeploymentTarget(t, update)
	update.Body.Close()
	if updatedProduction.Stage != "production" || updatedProduction.Replicas != 3 || updatedProduction.ContainerPort != 9090 {
		t.Fatalf("updated production target = %#v", updatedProduction)
	}

	developerUser := environment.ensureActor(t, "dual-stage-developer")
	viewerUser := environment.ensureActor(t, "dual-stage-viewer")
	addMember(t, environment.server, project.ID, developerUser.ID.String(), "developer", "dual-stage-add-developer")
	addMember(t, environment.server, project.ID, viewerUser.ID.String(), "viewer", "dual-stage-add-viewer")
	developer := environment.serverForActor(t, "dual-stage-developer")
	viewer := environment.serverForActor(t, "dual-stage-viewer")
	imageReference := "registry.example/orbit-devops/demo@sha256:" + strings.Repeat("d", 64)
	body := `{"imageReference":"` + imageReference + `"}`
	developmentReleaseResponse := environment.postJSON(t, "/api/v1/deployment-targets/"+development.ID+"/releases", "dual-stage-release-development", body)
	if developmentReleaseResponse.StatusCode != http.StatusCreated {
		developmentReleaseResponse.Body.Close()
		t.Fatalf("create development release status = %d", developmentReleaseResponse.StatusCode)
	}
	developmentRelease := decodeReleaseAcceptance(t, developmentReleaseResponse)
	developmentReleaseResponse.Body.Close()
	productionReleaseResponse := requestJSON(t, developer, http.MethodPost, "/api/v1/deployment-targets/"+production.ID+"/releases", "dual-stage-release-production", body)
	if productionReleaseResponse.StatusCode != http.StatusCreated {
		productionReleaseResponse.Body.Close()
		t.Fatalf("developer create production release status = %d", productionReleaseResponse.StatusCode)
	}
	productionRelease := decodeReleaseAcceptance(t, productionReleaseResponse)
	productionReleaseResponse.Body.Close()
	if developmentRelease.Release.ID == productionRelease.Release.ID ||
		developmentRelease.Release.TargetSnapshot.Stage != "development" ||
		productionRelease.Release.TargetSnapshot.Stage != "production" ||
		developmentRelease.Release.ImageReference != productionRelease.Release.ImageReference ||
		productionRelease.ReleaseOperation.DeploymentTargetID != production.ID {
		t.Fatalf("dual-stage releases = dev %#v, prod %#v", developmentRelease, productionRelease)
	}
	denied := requestJSON(t, viewer, http.MethodPost, "/api/v1/deployment-targets/"+production.ID+"/releases", "dual-stage-viewer-release", body)
	defer denied.Body.Close()
	assertError(t, denied, http.StatusForbidden, "project_permission_denied")

	for _, target := range []struct{ id, releaseID string }{{development.ID, developmentRelease.Release.ID}, {production.ID, productionRelease.Release.ID}} {
		response := environment.get(t, "/api/v1/deployment-targets/"+target.id+"/releases")
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("list %s release history status = %d", target.id, response.StatusCode)
		}
		page := decodeReleaseHistoryPage(t, response)
		response.Body.Close()
		if len(page.Items) != 1 || page.Items[0].Release.ID != target.releaseID {
			t.Fatalf("target %s history = %#v", target.id, page)
		}
	}
}
