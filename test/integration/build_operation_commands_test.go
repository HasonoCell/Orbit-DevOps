package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// HTTP 用户命令只暴露 BuildOperation，不泄露内部 Dispatch，并遵守业务状态机。
func TestPendingBuildCanBeCanceledButNotRetried(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-command-project")
	application := createApplication(t, environment, project.ID, "build-command-application")
	created := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-command-create",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("a", 40)+`"}`)
	defer created.Body.Close()
	var acceptance buildAcceptanceDocument
	if created.StatusCode != http.StatusCreated || json.NewDecoder(created.Body).Decode(&acceptance) != nil {
		t.Fatalf("create build status = %d", created.StatusCode)
	}

	get := environment.get(t, "/api/v1/build-operations/"+acceptance.BuildOperation.ID)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get build operation status = %d", get.StatusCode)
	}
	var pending struct {
		Status   string `json:"status"`
		Attempts []any  `json:"attempts"`
	}
	if err := json.NewDecoder(get.Body).Decode(&pending); err != nil || pending.Status != "pending" || len(pending.Attempts) != 0 {
		t.Fatalf("pending build operation = %#v, %v", pending, err)
	}

	canceled := environment.postJSON(t, "/api/v1/build-operations/"+acceptance.BuildOperation.ID+"/cancel", "build-command-cancel", `{}`)
	defer canceled.Body.Close()
	if canceled.StatusCode != http.StatusOK {
		t.Fatalf("cancel build status = %d", canceled.StatusCode)
	}
	var result buildOperationDocument
	if err := json.NewDecoder(canceled.Body).Decode(&result); err != nil || result.Status != "canceled" {
		t.Fatalf("canceled build operation = %#v, %v", result, err)
	}

	retry := environment.postJSON(t, "/api/v1/build-operations/"+acceptance.BuildOperation.ID+"/retry", "build-command-retry", `{}`)
	defer retry.Body.Close()
	if retry.StatusCode != http.StatusConflict {
		t.Fatalf("retry canceled build status = %d", retry.StatusCode)
	}
}
