package integration_test

import (
	"net/http"
	"testing"
)

func TestRuntimeSnapshotDoesNotInventStateWhenKubernetesIsUnavailable(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)

	response := environment.get(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/runtime-snapshot",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	snapshot := decodeRuntimeSnapshot(t, response)
	if snapshot.DeploymentTargetID != target.ID {
		t.Errorf("deploymentTargetId = %q, want %q", snapshot.DeploymentTargetID, target.ID)
	}
	if snapshot.Source != "kubernetes" {
		t.Errorf("source = %q, want kubernetes", snapshot.Source)
	}
	if snapshot.Freshness != "unavailable" {
		t.Errorf("freshness = %q, want unavailable", snapshot.Freshness)
	}
	if snapshot.ObservedAt.IsZero() {
		t.Error("observedAt is zero")
	}
	if snapshot.DeploymentExists || snapshot.ReleaseID != nil {
		t.Error("unavailable snapshot invented Deployment or Release state")
	}
	if snapshot.DesiredReplicas != 0 || snapshot.ReadyReplicas != 0 {
		t.Error("unavailable snapshot copied desired target state into runtime state")
	}
	if snapshot.ErrorCategory == nil || *snapshot.ErrorCategory != "kubernetes_not_configured" {
		t.Errorf("errorCategory = %v, want kubernetes_not_configured", snapshot.ErrorCategory)
	}
}
