package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type buildDocument struct {
	ID                    string    `json:"id"`
	ProjectID             string    `json:"projectId"`
	ApplicationID         string    `json:"applicationId"`
	RepositoryURL         string    `json:"repositoryUrl"`
	SourceCommit          string    `json:"sourceCommit"`
	DockerfilePath        string    `json:"dockerfilePath"`
	ContextPath           string    `json:"contextPath"`
	Platform              string    `json:"platform"`
	DestinationRepository string    `json:"destinationRepository"`
	CreatedBy             string    `json:"createdBy"`
	CreatedAt             time.Time `json:"createdAt"`
}

type buildOperationDocument struct {
	ID           string `json:"id"`
	BuildID      string `json:"buildId"`
	Status       string `json:"status"`
	AttemptCount int    `json:"attemptCount"`
}

type buildAcceptanceDocument struct {
	Build          buildDocument          `json:"build"`
	BuildOperation buildOperationDocument `json:"buildOperation"`
	ImageArtifact  *imageArtifactDocument `json:"imageArtifact"`
}

type imageArtifactDocument struct {
	ID             string    `json:"id"`
	BuildID        string    `json:"buildId"`
	ProjectID      string    `json:"projectId"`
	ApplicationID  string    `json:"applicationId"`
	Repository     string    `json:"repository"`
	Digest         string    `json:"digest"`
	ImageReference string    `json:"imageReference"`
	Platform       string    `json:"platform"`
	CreatedAt      time.Time `json:"createdAt"`
}

// 创建 Build 必须在一次受理中同时留下不可变输入和可恢复的待执行工作。
func TestBuildAcceptanceAtomicallyCreatesPendingBuildOperation(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "create-build-project")
	application := createApplication(t, environment, project.ID, "create-build-application")
	commit := strings.Repeat("a", 40)

	response := environment.postJSON(
		t,
		"/api/v1/applications/"+application.ID+"/builds",
		"build-demo-v1",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+commit+`","dockerfilePath":"Dockerfile","contextPath":"."}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create build status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	var acceptance buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode build acceptance: %v", err)
	}
	if acceptance.Build.ProjectID != project.ID || acceptance.Build.ApplicationID != application.ID {
		t.Fatalf("build ownership = %s/%s, want %s/%s", acceptance.Build.ProjectID, acceptance.Build.ApplicationID, project.ID, application.ID)
	}
	if acceptance.Build.RepositoryURL != "https://github.com/example/demo.git" || acceptance.Build.SourceCommit != commit {
		t.Fatalf("build source = %q@%q", acceptance.Build.RepositoryURL, acceptance.Build.SourceCommit)
	}
	if acceptance.Build.DockerfilePath != "Dockerfile" || acceptance.Build.ContextPath != "." {
		t.Fatalf("build paths = %q/%q", acceptance.Build.DockerfilePath, acceptance.Build.ContextPath)
	}
	if acceptance.Build.Platform != "linux/amd64" {
		t.Fatalf("build platform = %q, want linux/amd64", acceptance.Build.Platform)
	}
	wantRepository := "registry.example/orbit-devops/" + project.ID + "/" + application.ID
	if acceptance.Build.DestinationRepository != wantRepository {
		t.Fatalf("destination repository = %q, want %q", acceptance.Build.DestinationRepository, wantRepository)
	}
	if acceptance.Build.CreatedBy != environment.users["local-developer"].ID.String() || acceptance.Build.CreatedAt.IsZero() {
		t.Fatalf("build creation metadata = %q/%s", acceptance.Build.CreatedBy, acceptance.Build.CreatedAt)
	}
	if acceptance.BuildOperation.BuildID != acceptance.Build.ID || acceptance.BuildOperation.Status != "pending" || acceptance.BuildOperation.AttemptCount != 0 {
		t.Fatalf("build operation = %#v, want pending operation for build", acceptance.BuildOperation)
	}

	getResponse := environment.get(t, "/api/v1/builds/"+acceptance.Build.ID)
	defer getResponse.Body.Close()
	if getResponse.StatusCode != http.StatusOK {
		t.Fatalf("get build status = %d, want %d", getResponse.StatusCode, http.StatusOK)
	}
	var retrieved buildAcceptanceDocument
	if err := json.NewDecoder(getResponse.Body).Decode(&retrieved); err != nil {
		t.Fatalf("decode retrieved build: %v", err)
	}
	if retrieved.Build.ID != acceptance.Build.ID || retrieved.BuildOperation.ID != acceptance.BuildOperation.ID {
		t.Fatalf("retrieved build = %#v, want acceptance identifiers", retrieved)
	}
}

func TestBuildAcceptanceIsIdempotentAndRejectsUntrustedSource(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "idempotent-build-project")
	application := createApplication(t, environment, project.ID, "idempotent-build-application")
	body := `{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"` + strings.Repeat("c", 40) + `"}`

	first := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "same-build", body)
	defer first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first build status = %d", first.StatusCode)
	}
	var firstAcceptance buildAcceptanceDocument
	if err := json.NewDecoder(first.Body).Decode(&firstAcceptance); err != nil {
		t.Fatalf("decode first build: %v", err)
	}

	replay := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "same-build", body)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusCreated {
		t.Fatalf("replayed build status = %d", replay.StatusCode)
	}
	var replayed buildAcceptanceDocument
	if err := json.NewDecoder(replay.Body).Decode(&replayed); err != nil {
		t.Fatalf("decode replayed build: %v", err)
	}
	if replayed.Build.ID != firstAcceptance.Build.ID || replayed.BuildOperation.ID != firstAcceptance.BuildOperation.ID {
		t.Fatalf("idempotent replay created different resources: %#v / %#v", firstAcceptance, replayed)
	}

	untrusted := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "untrusted-build",
		`{"repositoryUrl":"https://127.0.0.1/private.git","sourceCommit":"`+strings.Repeat("d", 40)+`"}`)
	defer untrusted.Body.Close()
	if untrusted.StatusCode != http.StatusBadRequest {
		t.Fatalf("untrusted repository status = %d, want %d", untrusted.StatusCode, http.StatusBadRequest)
	}
}
