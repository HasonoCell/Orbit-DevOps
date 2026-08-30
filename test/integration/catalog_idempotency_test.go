package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCatalogWritesAreScopedAndIdempotent(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "create-idempotency-project")
	sharedKey := "configure-delivery-catalog"
	applicationBody := `{"name":"Delivery API","slug":"delivery-api"}`

	firstApplicationResponse := environment.postJSON(
		t,
		"/api/v1/projects/"+project.ID+"/applications",
		sharedKey,
		applicationBody,
	)
	defer firstApplicationResponse.Body.Close()
	if firstApplicationResponse.StatusCode != http.StatusCreated {
		t.Fatalf(
			"first application status = %d, want %d",
			firstApplicationResponse.StatusCode,
			http.StatusCreated,
		)
	}
	firstApplication := decodeApplication(t, firstApplicationResponse)

	replayedApplicationResponse := environment.postJSON(
		t,
		"/api/v1/projects/"+project.ID+"/applications",
		sharedKey,
		applicationBody,
	)
	defer replayedApplicationResponse.Body.Close()
	if replayedApplicationResponse.StatusCode != http.StatusCreated {
		t.Fatalf(
			"replayed application status = %d, want %d",
			replayedApplicationResponse.StatusCode,
			http.StatusCreated,
		)
	}
	replayedApplication := decodeApplication(t, replayedApplicationResponse)
	if replayedApplication.ID != firstApplication.ID {
		t.Errorf("replayed application id = %q, want %q", replayedApplication.ID, firstApplication.ID)
	}

	targetBody := `{"stage":"development","replicas":2,"containerPort":8080}`
	firstTargetResponse := environment.postJSON(
		t,
		"/api/v1/applications/"+firstApplication.ID+"/deployment-targets",
		sharedKey,
		targetBody,
	)
	defer firstTargetResponse.Body.Close()
	if firstTargetResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first target status = %d, want %d", firstTargetResponse.StatusCode, http.StatusCreated)
	}
	firstTarget := decodeDeploymentTarget(t, firstTargetResponse)

	replayedTargetResponse := environment.postJSON(
		t,
		"/api/v1/applications/"+firstApplication.ID+"/deployment-targets",
		sharedKey,
		targetBody,
	)
	defer replayedTargetResponse.Body.Close()
	if replayedTargetResponse.StatusCode != http.StatusCreated {
		t.Fatalf(
			"replayed target status = %d, want %d",
			replayedTargetResponse.StatusCode,
			http.StatusCreated,
		)
	}
	replayedTarget := decodeDeploymentTarget(t, replayedTargetResponse)
	if replayedTarget.ID != firstTarget.ID {
		t.Errorf("replayed target id = %q, want %q", replayedTarget.ID, firstTarget.ID)
	}

	conflictResponse := environment.postJSON(
		t,
		"/api/v1/applications/"+firstApplication.ID+"/deployment-targets",
		sharedKey,
		`{"stage":"development","replicas":3,"containerPort":8080}`,
	)
	defer conflictResponse.Body.Close()
	if conflictResponse.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d, want %d", conflictResponse.StatusCode, http.StatusConflict)
	}

	var errorDocument struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(conflictResponse.Body).Decode(&errorDocument); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if errorDocument.Code != "idempotency_conflict" {
		t.Errorf("conflict code = %q, want %q", errorDocument.Code, "idempotency_conflict")
	}
}
