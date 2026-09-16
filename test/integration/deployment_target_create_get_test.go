package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestUserCanCreateAndRetrieveDevelopmentTarget(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "create-target-project")
	application := createApplication(
		t,
		environment,
		project.ID,
		"create-target-application",
	)

	createResponse := environment.postJSON(
		t,
		"/api/v1/applications/"+application.ID+"/deployment-targets",
		"create-development-target",
		`{"stage":"development","replicas":2,"containerPort":8080}`,
	)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", createResponse.StatusCode, http.StatusCreated)
	}

	created := decodeDeploymentTarget(t, createResponse)
	if _, err := uuid.Parse(created.ID); err != nil {
		t.Errorf("id = %q, want UUID: %v", created.ID, err)
	}
	if created.ApplicationID != application.ID {
		t.Errorf("applicationId = %q, want %q", created.ApplicationID, application.ID)
	}
	if created.Stage != "development" {
		t.Errorf("stage = %q, want %q", created.Stage, "development")
	}
	if created.ClusterRef != "kind-orbit-devops-s1" {
		t.Errorf("clusterRef = %q, want server-managed cluster", created.ClusterRef)
	}
	if created.Namespace != "orbit-devops-s1" {
		t.Errorf("namespace = %q, want server-managed namespace", created.Namespace)
	}
	if created.Replicas != 2 {
		t.Errorf("replicas = %d, want %d", created.Replicas, 2)
	}
	if created.ContainerPort != 8080 {
		t.Errorf("containerPort = %d, want %d", created.ContainerPort, 8080)
	}
	wantCreatedBy := environment.users["local-developer"].ID.String()
	if created.CreatedBy != wantCreatedBy {
		t.Errorf("createdBy = %q, want %q", created.CreatedBy, wantCreatedBy)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("createdAt or updatedAt is zero")
	}

	getResponse := environment.get(t, "/api/v1/deployment-targets/"+created.ID)
	defer getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want %d", getResponse.StatusCode, http.StatusOK)
	}

	retrieved := decodeDeploymentTarget(t, getResponse)
	if retrieved.ID != created.ID ||
		retrieved.ApplicationID != created.ApplicationID ||
		retrieved.Stage != created.Stage ||
		retrieved.ClusterRef != created.ClusterRef ||
		retrieved.Namespace != created.Namespace ||
		retrieved.Replicas != created.Replicas ||
		retrieved.ContainerPort != created.ContainerPort ||
		retrieved.CreatedBy != created.CreatedBy ||
		!retrieved.CreatedAt.Equal(created.CreatedAt) ||
		!retrieved.UpdatedAt.Equal(created.UpdatedAt) {
		t.Errorf("retrieved target = %#v, want equivalent to %#v", retrieved, created)
	}
}

func createApplication(
	t *testing.T,
	environment *testEnvironment,
	projectID string,
	idempotencyKey string,
) applicationDocument {
	t.Helper()

	response := environment.postJSON(
		t,
		"/api/v1/projects/"+projectID+"/applications",
		idempotencyKey,
		`{"name":"Delivery API","slug":"delivery-api"}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create application status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	return decodeApplication(t, response)
}
