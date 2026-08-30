package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestUserCanCreateAndRetrieveApplication(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "create-application-project")

	createResponse := environment.postJSON(
		t,
		"/api/v1/projects/"+project.ID+"/applications",
		"create-delivery-application",
		`{"name":"Delivery API","slug":"delivery-api"}`,
	)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", createResponse.StatusCode, http.StatusCreated)
	}

	created := decodeApplication(t, createResponse)
	if _, err := uuid.Parse(created.ID); err != nil {
		t.Errorf("id = %q, want UUID: %v", created.ID, err)
	}
	if created.ProjectID != project.ID {
		t.Errorf("projectId = %q, want %q", created.ProjectID, project.ID)
	}
	if created.Name != "Delivery API" {
		t.Errorf("name = %q, want %q", created.Name, "Delivery API")
	}
	if created.Slug != "delivery-api" {
		t.Errorf("slug = %q, want %q", created.Slug, "delivery-api")
	}
	if created.CreatedBy != "local-developer" {
		t.Errorf("createdBy = %q, want %q", created.CreatedBy, "local-developer")
	}
	if created.CreatedAt.IsZero() {
		t.Error("createdAt is zero")
	}

	getResponse := environment.get(t, "/api/v1/applications/"+created.ID)
	defer getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want %d", getResponse.StatusCode, http.StatusOK)
	}

	retrieved := decodeApplication(t, getResponse)
	if retrieved.ID != created.ID ||
		retrieved.ProjectID != created.ProjectID ||
		retrieved.Name != created.Name ||
		retrieved.Slug != created.Slug ||
		retrieved.CreatedBy != created.CreatedBy ||
		!retrieved.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("retrieved application = %#v, want equivalent to %#v", retrieved, created)
	}
}

func createProject(
	t *testing.T,
	environment *testEnvironment,
	idempotencyKey string,
) projectDocument {
	t.Helper()

	response := environment.postProject(
		t,
		idempotencyKey,
		`{"name":"Platform","slug":"platform"}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create project status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	return decodeProject(t, response)
}
