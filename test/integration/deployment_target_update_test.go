package integration_test

import (
	"net/http"
	"testing"
)

func TestDeploymentTargetUpdateIsIdempotentAndDoesNotRewriteRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	releaseResponse := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"target-update-release",
		`{"imageReference":"registry.example/orbit-devops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	)
	defer releaseResponse.Body.Close()
	if releaseResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create release status = %d, want %d", releaseResponse.StatusCode, http.StatusCreated)
	}
	release := decodeReleaseAcceptance(t, releaseResponse).Release

	firstUpdate := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"target-update-first",
		`{"replicas":1,"containerPort":9090}`,
	)
	defer firstUpdate.Body.Close()
	if firstUpdate.StatusCode != http.StatusOK {
		t.Fatalf("first update status = %d, want %d", firstUpdate.StatusCode, http.StatusOK)
	}
	firstResult := decodeDeploymentTarget(t, firstUpdate)
	if firstResult.Replicas != 1 || firstResult.ContainerPort != 9090 {
		t.Errorf("first update = %#v, want replicas 1 and port 9090", firstResult)
	}

	secondUpdate := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"target-update-second",
		`{"replicas":3,"containerPort":7070}`,
	)
	defer secondUpdate.Body.Close()
	if secondUpdate.StatusCode != http.StatusOK {
		t.Fatalf("second update status = %d, want %d", secondUpdate.StatusCode, http.StatusOK)
	}
	secondResult := decodeDeploymentTarget(t, secondUpdate)

	replay := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"target-update-first",
		`{"replicas":1,"containerPort":9090}`,
	)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusOK {
		t.Fatalf("update replay status = %d, want %d", replay.StatusCode, http.StatusOK)
	}
	replayedResult := decodeDeploymentTarget(t, replay)
	if replayedResult.Replicas != firstResult.Replicas ||
		replayedResult.ContainerPort != firstResult.ContainerPort ||
		!replayedResult.UpdatedAt.Equal(firstResult.UpdatedAt) {
		t.Errorf("replayed update = %#v, want original result %#v", replayedResult, firstResult)
	}

	currentResponse := environment.get(t, "/api/v1/deployment-targets/"+target.ID)
	defer currentResponse.Body.Close()
	current := decodeDeploymentTarget(t, currentResponse)
	if current.Replicas != secondResult.Replicas || current.ContainerPort != secondResult.ContainerPort {
		t.Errorf("current target = %#v, want latest update %#v", current, secondResult)
	}

	releaseQuery := environment.get(t, "/api/v1/releases/"+release.ID)
	defer releaseQuery.Body.Close()
	immutableRelease := decodeRelease(t, releaseQuery)
	if immutableRelease.TargetSnapshot.Replicas != target.Replicas ||
		immutableRelease.TargetSnapshot.ContainerPort != target.ContainerPort {
		t.Errorf("release snapshot changed after target update: %#v", immutableRelease.TargetSnapshot)
	}

	conflict := environment.putJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID,
		"target-update-first",
		`{"replicas":4,"containerPort":6060}`,
	)
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Errorf("update conflict status = %d, want %d", conflict.StatusCode, http.StatusConflict)
	}
}
