package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestUserCanCreateProject(t *testing.T) {
	environment := newTestEnvironment(t)
	response := environment.postProject(
		t,
		"create-platform-project",
		`{"name":"Platform","slug":"platform"}`,
	)
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	project := decodeProject(t, response)

	if _, err := uuid.Parse(project.ID); err != nil {
		t.Errorf("id = %q, want UUID: %v", project.ID, err)
	}
	if project.Name != "Platform" {
		t.Errorf("name = %q, want %q", project.Name, "Platform")
	}
	if project.Slug != "platform" {
		t.Errorf("slug = %q, want %q", project.Slug, "platform")
	}
	wantCreatedBy := environment.users["local-developer"].ID.String()
	if project.CreatedBy != wantCreatedBy {
		t.Errorf("createdBy = %q, want %q", project.CreatedBy, wantCreatedBy)
	}
	if project.CreatedAt.IsZero() {
		t.Error("createdAt is zero")
	}
}

func TestRepeatedProjectCreateReturnsSameProject(t *testing.T) {
	environment := newTestEnvironment(t)
	requestBody := `{"name":"Delivery","slug":"delivery"}`
	idempotencyKey := "create-delivery-project"

	firstResponse := environment.postProject(t, idempotencyKey, requestBody)
	defer firstResponse.Body.Close()
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", firstResponse.StatusCode, http.StatusCreated)
	}
	firstProject := decodeProject(t, firstResponse)

	secondResponse := environment.postProject(t, idempotencyKey, requestBody)
	defer secondResponse.Body.Close()
	if secondResponse.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d, want %d", secondResponse.StatusCode, http.StatusCreated)
	}
	secondProject := decodeProject(t, secondResponse)

	if secondProject.ID != firstProject.ID {
		t.Errorf("replayed id = %q, want %q", secondProject.ID, firstProject.ID)
	}
	if !secondProject.CreatedAt.Equal(firstProject.CreatedAt) {
		t.Errorf(
			"replayed createdAt = %s, want %s",
			secondProject.CreatedAt,
			firstProject.CreatedAt,
		)
	}
}

func TestReusingIdempotencyKeyForDifferentProjectReturnsConflict(t *testing.T) {
	environment := newTestEnvironment(t)
	idempotencyKey := "create-conflicting-project"

	firstResponse := environment.postProject(
		t,
		idempotencyKey,
		`{"name":"First","slug":"first"}`,
	)
	defer firstResponse.Body.Close()
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", firstResponse.StatusCode, http.StatusCreated)
	}

	conflictResponse := environment.postProject(
		t,
		idempotencyKey,
		`{"name":"Second","slug":"second"}`,
	)
	defer conflictResponse.Body.Close()
	if conflictResponse.StatusCode != http.StatusConflict {
		t.Fatalf(
			"conflict status = %d, want %d",
			conflictResponse.StatusCode,
			http.StatusConflict,
		)
	}

	var errorDocument struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(conflictResponse.Body).Decode(&errorDocument); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if errorDocument.Code != "idempotency_conflict" {
		t.Errorf(
			"conflict code = %q, want %q",
			errorDocument.Code,
			"idempotency_conflict",
		)
	}
}
