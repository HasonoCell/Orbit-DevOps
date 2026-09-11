package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/google/uuid"
)

// 成功构建产生的 ImageArtifact 可查询，并且只有匹配同一 Project/Application 的 Release 才能关联。
func TestImageArtifactCanBeQueriedAndLinkedToRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "artifact-project")
	application := createApplication(t, environment, project.ID, "artifact-application")
	acceptance := createBuildForArtifact(t, environment, application.ID, "artifact-build", "a")
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL))
	lease := claimBuildDispatch(t, operations, "artifact-worker")
	digest := "sha256:" + strings.Repeat("b", 64)
	artifact, err := operations.Succeed(context.Background(), lease, buildoperation.ArtifactResult{
		Repository: acceptance.Build.DestinationRepository, Digest: digest, LogExcerpt: "artifact created",
	})
	if err != nil {
		t.Fatal(err)
	}

	buildResponse := environment.get(t, "/api/v1/builds/"+acceptance.Build.ID)
	defer buildResponse.Body.Close()
	var completed buildAcceptanceDocument
	if buildResponse.StatusCode != http.StatusOK || json.NewDecoder(buildResponse.Body).Decode(&completed) != nil ||
		completed.ImageArtifact == nil || completed.ImageArtifact.ID != artifact.ID.String() {
		t.Fatalf("completed build = %#v, status %d", completed, buildResponse.StatusCode)
	}
	artifactResponse := environment.get(t, "/api/v1/image-artifacts/"+artifact.ID.String())
	defer artifactResponse.Body.Close()
	var artifactDocument imageArtifactDocument
	if artifactResponse.StatusCode != http.StatusOK || json.NewDecoder(artifactResponse.Body).Decode(&artifactDocument) != nil ||
		artifactDocument.ImageReference != artifact.ImageReference || artifactDocument.BuildID != acceptance.Build.ID {
		t.Fatalf("artifact response = %#v, status %d", artifactDocument, artifactResponse.StatusCode)
	}

	target := createTargetForApplication(t, environment, application.ID, "artifact-target")
	releaseResponse := environment.postJSON(t, "/api/v1/deployment-targets/"+target.ID+"/releases", "artifact-release",
		fmt.Sprintf(`{"imageReference":%q,"imageArtifactId":%q}`, artifact.ImageReference, artifact.ID))
	defer releaseResponse.Body.Close()
	if releaseResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create artifact release status = %d", releaseResponse.StatusCode)
	}
	release := decodeReleaseAcceptance(t, releaseResponse)
	if release.Release.ImageArtifactID == nil || *release.Release.ImageArtifactID != artifact.ID.String() {
		t.Fatalf("release artifact = %#v", release.Release.ImageArtifactID)
	}
	rollbackResponse := environment.postJSON(t, "/api/v1/releases/"+release.Release.ID+"/rollback", "artifact-rollback", "")
	defer rollbackResponse.Body.Close()
	if rollbackResponse.StatusCode != http.StatusCreated {
		t.Fatalf("rollback artifact release status = %d", rollbackResponse.StatusCode)
	}
	rollback := decodeReleaseAcceptance(t, rollbackResponse)
	if rollback.Release.ImageArtifactID == nil || *rollback.Release.ImageArtifactID != artifact.ID.String() {
		t.Fatalf("rollback artifact = %#v", rollback.Release.ImageArtifactID)
	}

	mismatch := environment.postJSON(t, "/api/v1/deployment-targets/"+target.ID+"/releases", "artifact-reference-mismatch",
		fmt.Sprintf(`{"imageReference":"%s@sha256:%s","imageArtifactId":%q}`,
			acceptance.Build.DestinationRepository, strings.Repeat("c", 64), artifact.ID))
	defer mismatch.Body.Close()
	assertError(t, mismatch, http.StatusBadRequest, "image_artifact_mismatch")

	otherApplication := createDistinctApplication(t, environment, project.ID, "artifact-other-application", "artifact-other")
	otherTarget := createTargetForApplication(t, environment, otherApplication.ID, "artifact-other-target")
	crossApplication := environment.postJSON(t, "/api/v1/deployment-targets/"+otherTarget.ID+"/releases", "artifact-cross-application",
		fmt.Sprintf(`{"imageReference":%q,"imageArtifactId":%q}`, artifact.ImageReference, artifact.ID))
	defer crossApplication.Body.Close()
	assertError(t, crossApplication, http.StatusNotFound, "image_artifact_not_found")

	missingArtifact := environment.postJSON(t, "/api/v1/deployment-targets/"+target.ID+"/releases", "artifact-missing",
		fmt.Sprintf(`{"imageReference":%q,"imageArtifactId":%q}`, artifact.ImageReference, uuid.New()))
	defer missingArtifact.Body.Close()
	assertError(t, missingArtifact, http.StatusNotFound, "image_artifact_not_found")
}

// 构建历史使用稳定游标分页，并在成功 Build 上携带对应制品。
func TestBuildHistoryUsesStableCursorAndIncludesArtifact(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-history-project")
	application := createApplication(t, environment, project.ID, "build-history-application")
	first := createBuildForArtifact(t, environment, application.ID, "history-build-1", "d")
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL))
	artifact, err := operations.Succeed(context.Background(), claimBuildDispatch(t, operations, "history-worker"), buildoperation.ArtifactResult{
		Repository: first.Build.DestinationRepository, Digest: "sha256:" + strings.Repeat("e", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := createBuildForArtifact(t, environment, application.ID, "history-build-2", "f")
	third := createBuildForArtifact(t, environment, application.ID, "history-build-3", "1")

	firstPageResponse := environment.get(t, "/api/v1/applications/"+application.ID+"/builds?limit=2")
	defer firstPageResponse.Body.Close()
	var firstPage struct {
		Items      []buildAcceptanceDocument `json:"items"`
		NextCursor *string                   `json:"nextCursor"`
	}
	if firstPageResponse.StatusCode != http.StatusOK || json.NewDecoder(firstPageResponse.Body).Decode(&firstPage) != nil ||
		len(firstPage.Items) != 2 || firstPage.NextCursor == nil {
		t.Fatalf("first build history page = %#v, status %d", firstPage, firstPageResponse.StatusCode)
	}
	secondPageResponse := environment.get(t, "/api/v1/applications/"+application.ID+"/builds?limit=2&cursor="+url.QueryEscape(*firstPage.NextCursor))
	defer secondPageResponse.Body.Close()
	var secondPage struct {
		Items []buildAcceptanceDocument `json:"items"`
	}
	if secondPageResponse.StatusCode != http.StatusOK || json.NewDecoder(secondPageResponse.Body).Decode(&secondPage) != nil || len(secondPage.Items) != 1 {
		t.Fatalf("second build history page = %#v, status %d", secondPage, secondPageResponse.StatusCode)
	}
	seen := map[string]*imageArtifactDocument{}
	for _, item := range append(firstPage.Items, secondPage.Items...) {
		seen[item.Build.ID] = item.ImageArtifact
	}
	if len(seen) != 3 || seen[first.Build.ID] == nil || seen[first.Build.ID].ID != artifact.ID.String() ||
		seen[second.Build.ID] != nil || seen[third.Build.ID] != nil {
		t.Fatalf("build history contents = %#v", seen)
	}

	invalid := environment.get(t, "/api/v1/applications/"+application.ID+"/builds?cursor=invalid")
	defer invalid.Body.Close()
	assertError(t, invalid, http.StatusBadRequest, "invalid_build_cursor")
}

func createBuildForArtifact(t *testing.T, environment *testEnvironment, applicationID, key, commitDigit string) buildAcceptanceDocument {
	t.Helper()
	response := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/builds", key,
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat(commitDigit, 40)+`"}`)
	defer response.Body.Close()
	var acceptance buildAcceptanceDocument
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
		t.Fatalf("create build %s status = %d", key, response.StatusCode)
	}
	return acceptance
}

func createTargetForApplication(t *testing.T, environment *testEnvironment, applicationID, key string) deploymentTargetDocument {
	t.Helper()
	response := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/deployment-targets", key,
		`{"stage":"development","replicas":1,"containerPort":8080}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create target %s status = %d", key, response.StatusCode)
	}
	return decodeDeploymentTarget(t, response)
}

func createDistinctApplication(t *testing.T, environment *testEnvironment, projectID, key, slug string) applicationDocument {
	t.Helper()
	response := environment.postJSON(t, "/api/v1/projects/"+projectID+"/applications", key,
		fmt.Sprintf(`{"name":"Artifact Other","slug":%q}`, slug))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create application %s status = %d", key, response.StatusCode)
	}
	return decodeApplication(t, response)
}
