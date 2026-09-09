package integration_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestReleaseAcceptanceIsIdempotent(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	idempotencyKey := "release-idempotent-demo"
	requestBody := `{"imageReference":"registry.example/orbitops/demo@sha256:` +
		strings.Repeat("a", 64) + `"}`

	firstResponse := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		idempotencyKey,
		requestBody,
	)
	defer firstResponse.Body.Close()
	if firstResponse.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want %d", firstResponse.StatusCode, http.StatusCreated)
	}
	first := decodeReleaseAcceptance(t, firstResponse)

	replayedResponse := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		idempotencyKey,
		requestBody,
	)
	defer replayedResponse.Body.Close()
	if replayedResponse.StatusCode != http.StatusCreated {
		t.Fatalf("replayed status = %d, want %d", replayedResponse.StatusCode, http.StatusCreated)
	}
	replayed := decodeReleaseAcceptance(t, replayedResponse)

	if replayed.Release.ID != first.Release.ID {
		t.Errorf("replayed release id = %q, want %q", replayed.Release.ID, first.Release.ID)
	}
	if replayed.ReleaseOperation.ID != first.ReleaseOperation.ID {
		t.Errorf("replayed operation id = %q, want %q", replayed.ReleaseOperation.ID, first.ReleaseOperation.ID)
	}

	conflictBody := `{"imageReference":"registry.example/orbitops/demo@sha256:` +
		strings.Repeat("b", 64) + `"}`
	conflictResponse := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		idempotencyKey,
		conflictBody,
	)
	defer conflictResponse.Body.Close()
	if conflictResponse.StatusCode != http.StatusConflict {
		t.Fatalf("conflict status = %d, want %d", conflictResponse.StatusCode, http.StatusConflict)
	}
}

func TestReleaseRejectsInvalidImageReferences(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)

	cases := []struct {
		name           string
		idempotencyKey string
		requestBody    string
	}{
		{
			name:           "mutable tag",
			idempotencyKey: "release-mutable-image",
			requestBody:    `{"imageReference":"registry.example/orbitops/demo:latest"}`,
		},
		{
			name:           "invalid repository name",
			idempotencyKey: "release-invalid-repository",
			requestBody: `{"imageReference":"registry.example/OrbitOps/demo@sha256:` +
				strings.Repeat("a", 64) + `"}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := environment.postJSON(
				t,
				"/api/v1/deployment-targets/"+target.ID+"/releases",
				testCase.idempotencyKey,
				testCase.requestBody,
			)
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
			}
		})
	}
}
