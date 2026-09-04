package integration_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestReleaseAcceptanceAtomicallyCreatesPendingOperation(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	imageReference := "registry.example/orbitops/demo@sha256:" + strings.Repeat("a", 64)

	createResponse := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"release-demo-v1",
		`{"imageReference":"`+imageReference+`"}`,
	)
	defer createResponse.Body.Close()
	if createResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want %d", createResponse.StatusCode, http.StatusCreated)
	}

	acceptance := decodeReleaseAcceptance(t, createResponse)
	if _, err := uuid.Parse(acceptance.Release.ID); err != nil {
		t.Errorf("release id = %q, want UUID: %v", acceptance.Release.ID, err)
	}
	if acceptance.Release.DeploymentTargetID != target.ID {
		t.Errorf(
			"deploymentTargetId = %q, want %q",
			acceptance.Release.DeploymentTargetID,
			target.ID,
		)
	}
	if acceptance.Release.ImageReference != imageReference {
		t.Errorf("imageReference = %q, want %q", acceptance.Release.ImageReference, imageReference)
	}
	wantSnapshot := releaseTargetSnapshot{
		ProjectID:     target.ProjectID,
		ApplicationID: target.ApplicationID,
		Stage:         target.Stage,
		ClusterRef:    target.ClusterRef,
		Namespace:     target.Namespace,
		Replicas:      target.Replicas,
		ContainerPort: target.ContainerPort,
	}
	if acceptance.Release.TargetSnapshot != wantSnapshot {
		t.Errorf("targetSnapshot = %#v, want %#v", acceptance.Release.TargetSnapshot, wantSnapshot)
	}
	if acceptance.Release.CreatedBy != "local-developer" {
		t.Errorf("createdBy = %q, want %q", acceptance.Release.CreatedBy, "local-developer")
	}
	if acceptance.Release.CreatedAt.IsZero() {
		t.Error("release createdAt is zero")
	}

	if _, err := uuid.Parse(acceptance.Operation.ID); err != nil {
		t.Errorf("operation id = %q, want UUID: %v", acceptance.Operation.ID, err)
	}
	if acceptance.Operation.Type != "release.deploy" {
		t.Errorf("operation type = %q, want %q", acceptance.Operation.Type, "release.deploy")
	}
	if acceptance.Operation.ReleaseID != acceptance.Release.ID {
		t.Errorf(
			"operation releaseId = %q, want %q",
			acceptance.Operation.ReleaseID,
			acceptance.Release.ID,
		)
	}
	if acceptance.Operation.DeploymentTargetID != target.ID {
		t.Errorf(
			"operation deploymentTargetId = %q, want %q",
			acceptance.Operation.DeploymentTargetID,
			target.ID,
		)
	}
	if acceptance.Operation.CreatedBy != "local-developer" {
		t.Errorf("operation createdBy = %q, want %q", acceptance.Operation.CreatedBy, "local-developer")
	}
	if acceptance.Operation.IdempotencyKey != "release-demo-v1" {
		t.Errorf(
			"operation idempotencyKey = %q, want %q",
			acceptance.Operation.IdempotencyKey,
			"release-demo-v1",
		)
	}
	if acceptance.Operation.Status != "pending" {
		t.Errorf("operation status = %q, want %q", acceptance.Operation.Status, "pending")
	}
	if acceptance.Operation.AttemptCount != 0 {
		t.Errorf("attemptCount = %d, want 0", acceptance.Operation.AttemptCount)
	}
	if acceptance.Operation.AutomaticRetryCount != 0 {
		t.Errorf("automaticRetryCount = %d, want 0", acceptance.Operation.AutomaticRetryCount)
	}
	if acceptance.Operation.RecoveryRequired {
		t.Error("new operation unexpectedly requires recovery")
	}
	if acceptance.Operation.QueuedAt.IsZero() || acceptance.Operation.AvailableAt.IsZero() {
		t.Error("pending operation has no scheduling timestamps")
	}
	if acceptance.Operation.ErrorCode != nil || acceptance.Operation.ErrorSummary != nil ||
		acceptance.Operation.RetryDisposition != nil {
		t.Error("pending operation contains terminal error")
	}
	if acceptance.Operation.StartedAt != nil || acceptance.Operation.FinishedAt != nil {
		t.Error("pending operation contains terminal timestamps")
	}

	releaseResponse := environment.get(t, "/api/v1/releases/"+acceptance.Release.ID)
	defer releaseResponse.Body.Close()
	if releaseResponse.StatusCode != http.StatusOK {
		t.Fatalf("get release status = %d, want %d", releaseResponse.StatusCode, http.StatusOK)
	}
	retrievedRelease := decodeRelease(t, releaseResponse)
	if retrievedRelease.ID != acceptance.Release.ID {
		t.Errorf("retrieved release id = %q, want %q", retrievedRelease.ID, acceptance.Release.ID)
	}

	operationResponse := environment.get(t, "/api/v1/operations/"+acceptance.Operation.ID)
	defer operationResponse.Body.Close()
	if operationResponse.StatusCode != http.StatusOK {
		t.Fatalf("get operation status = %d, want %d", operationResponse.StatusCode, http.StatusOK)
	}
	retrievedOperation := decodeOperation(t, operationResponse)
	if retrievedOperation.ID != acceptance.Operation.ID {
		t.Errorf("retrieved operation id = %q, want %q", retrievedOperation.ID, acceptance.Operation.ID)
	}
}

func createDeploymentTarget(
	t *testing.T,
	environment *testEnvironment,
) deploymentTargetDocument {
	t.Helper()

	project := createProject(t, environment, "create-release-project")
	application := createApplication(
		t,
		environment,
		project.ID,
		"create-release-application",
	)
	response := environment.postJSON(
		t,
		"/api/v1/applications/"+application.ID+"/deployment-targets",
		"create-release-target",
		`{"stage":"development","replicas":2,"containerPort":8080}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create target status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	target := decodeDeploymentTarget(t, response)
	target.ProjectID = project.ID
	return target
}
