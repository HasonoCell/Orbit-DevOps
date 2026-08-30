package integration_test

import (
	"context"
	"net/http"
	"testing"
)

func TestUserCanRetrieveCreatedProject(t *testing.T) {
	environment := newTestEnvironment(t)

	createResponse := environment.postProject(
		t,
		"create-runtime-project",
		`{"name":"Runtime","slug":"runtime"}`,
	)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", createResponse.StatusCode, http.StatusCreated)
	}
	createdProject := decodeProject(t, createResponse)

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		environment.server.URL+"/api/v1/projects/"+createdProject.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("build get project request: %v", err)
	}

	response, err := environment.server.Client().Do(request)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	retrievedProject := decodeProject(t, response)
	if retrievedProject.ID != createdProject.ID {
		t.Errorf("retrieved id = %q, want %q", retrievedProject.ID, createdProject.ID)
	}
	if retrievedProject.Name != createdProject.Name {
		t.Errorf("retrieved name = %q, want %q", retrievedProject.Name, createdProject.Name)
	}
	if retrievedProject.Slug != createdProject.Slug {
		t.Errorf("retrieved slug = %q, want %q", retrievedProject.Slug, createdProject.Slug)
	}
	if retrievedProject.CreatedBy != createdProject.CreatedBy {
		t.Errorf(
			"retrieved createdBy = %q, want %q",
			retrievedProject.CreatedBy,
			createdProject.CreatedBy,
		)
	}
	if !retrievedProject.CreatedAt.Equal(createdProject.CreatedAt) {
		t.Errorf(
			"retrieved createdAt = %s, want %s",
			retrievedProject.CreatedAt,
			createdProject.CreatedAt,
		)
	}
}
