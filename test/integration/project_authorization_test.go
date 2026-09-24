package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type projectMemberDocument struct {
	ProjectID string    `json:"projectId"`
	UserID    string    `json:"userId"`
	Role      string    `json:"role"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type errorDocument struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func TestProjectRolesProtectResourcesAndMembership(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "create-authorized-project")
	ownerServer := environment.server

	ownerMembers := requestJSON(
		t,
		ownerServer,
		http.MethodGet,
		"/api/v1/projects/"+project.ID+"/members",
		"",
		"",
	)
	defer ownerMembers.Body.Close()
	if ownerMembers.StatusCode != http.StatusOK {
		t.Fatalf("list initial members status = %d, want %d", ownerMembers.StatusCode, http.StatusOK)
	}
	initialMembers := decodeMembers(t, ownerMembers)
	if len(initialMembers) != 1 || initialMembers[0].UserID != environment.users["local-developer"].ID.String() || initialMembers[0].Role != "owner" {
		t.Fatalf("initial members = %#v, want project creator as sole owner", initialMembers)
	}

	developerUser := environment.ensureActor(t, "local-contributor")
	viewerUser := environment.ensureActor(t, "local-viewer")
	unauthorizedUser := environment.ensureActor(t, "unauthorized-member")
	temporaryUser := environment.ensureActor(t, "temporary-member")
	developer := addMember(t, ownerServer, project.ID, developerUser.ID.String(), "developer", "add-project-developer")
	replayedDeveloper := addMember(t, ownerServer, project.ID, developerUser.ID.String(), "developer", "add-project-developer")
	if replayedDeveloper != developer {
		t.Errorf("replayed member = %#v, want %#v", replayedDeveloper, developer)
	}
	addMember(t, ownerServer, project.ID, viewerUser.ID.String(), "viewer", "add-project-viewer")

	developerServer := environment.serverForActor(t, "local-contributor")
	viewerServer := environment.serverForActor(t, "local-viewer")
	outsiderServer := environment.serverForActor(t, "local-outsider")

	createApplication := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/projects/"+project.ID+"/applications",
		"developer-create-application",
		`{"name":"Authorized API","slug":"authorized-api"}`,
	)
	defer createApplication.Body.Close()
	if createApplication.StatusCode != http.StatusCreated {
		t.Fatalf("developer create application status = %d, want %d", createApplication.StatusCode, http.StatusCreated)
	}
	application := decodeApplication(t, createApplication)
	createTarget := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/applications/"+application.ID+"/deployment-targets",
		"developer-create-target",
		`{"stage":"development","replicas":1,"containerPort":8080}`,
	)
	defer createTarget.Body.Close()
	if createTarget.StatusCode != http.StatusCreated {
		t.Fatalf("developer create target status = %d, want %d", createTarget.StatusCode, http.StatusCreated)
	}
	target := decodeDeploymentTarget(t, createTarget)

	createRelease := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"developer-create-release",
		`{"imageReference":"registry.example/orbit-devops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	)
	defer createRelease.Body.Close()
	if createRelease.StatusCode != http.StatusCreated {
		t.Fatalf("developer create release status = %d, want %d", createRelease.StatusCode, http.StatusCreated)
	}
	acceptance := decodeReleaseAcceptance(t, createRelease)

	viewerRead := requestJSON(
		t,
		viewerServer,
		http.MethodGet,
		"/api/v1/applications/"+application.ID,
		"",
		"",
	)
	defer viewerRead.Body.Close()
	if viewerRead.StatusCode != http.StatusOK {
		t.Errorf("viewer read application status = %d, want %d", viewerRead.StatusCode, http.StatusOK)
	}
	for _, path := range []string{
		"/api/v1/deployment-targets/" + target.ID,
		"/api/v1/deployment-targets/" + target.ID + "/releases",
		"/api/v1/releases/" + acceptance.Release.ID,
		"/api/v1/releases/" + acceptance.Release.ID + "/diagnostics",
		"/api/v1/release-operations/" + acceptance.ReleaseOperation.ID,
	} {
		response := requestJSON(t, viewerServer, http.MethodGet, path, "", "")
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Errorf("viewer GET %s status = %d, want %d", path, response.StatusCode, http.StatusOK)
			continue
		}
		response.Body.Close()
	}

	viewerWrite := requestJSON(
		t,
		viewerServer,
		http.MethodPost,
		"/api/v1/projects/"+project.ID+"/applications",
		"viewer-create-application",
		`{"name":"Denied API","slug":"denied-api"}`,
	)
	defer viewerWrite.Body.Close()
	assertError(t, viewerWrite, http.StatusForbidden, "project_permission_denied")

	viewerTargetUpdate := requestJSON(
		t,
		viewerServer,
		http.MethodPut,
		"/api/v1/deployment-targets/"+target.ID,
		"viewer-update-target",
		`{"replicas":2,"containerPort":8080}`,
	)
	defer viewerTargetUpdate.Body.Close()
	assertError(t, viewerTargetUpdate, http.StatusForbidden, "project_permission_denied")

	viewerRelease := requestJSON(
		t,
		viewerServer,
		http.MethodPost,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"viewer-create-release",
		`{"imageReference":"registry.example/orbit-devops/demo@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`,
	)
	defer viewerRelease.Body.Close()
	assertError(t, viewerRelease, http.StatusForbidden, "project_permission_denied")

	developerRollback := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/releases/"+acceptance.Release.ID+"/rollback",
		"developer-rollback-release",
		"",
	)
	defer developerRollback.Body.Close()
	if developerRollback.StatusCode != http.StatusCreated {
		t.Errorf("developer rollback status = %d, want %d", developerRollback.StatusCode, http.StatusCreated)
	}
	viewerRollback := requestJSON(
		t,
		viewerServer,
		http.MethodPost,
		"/api/v1/releases/"+acceptance.Release.ID+"/rollback",
		"viewer-rollback-release",
		"",
	)
	defer viewerRollback.Body.Close()
	assertError(t, viewerRollback, http.StatusForbidden, "project_permission_denied")

	viewerCancel := requestJSON(
		t,
		viewerServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
		"viewer-cancel-operation",
		"",
	)
	defer viewerCancel.Body.Close()
	assertError(t, viewerCancel, http.StatusForbidden, "project_permission_denied")

	outsiderCancel := requestJSON(
		t,
		outsiderServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
		"outsider-cancel-operation",
		"",
	)
	defer outsiderCancel.Body.Close()
	assertError(t, outsiderCancel, http.StatusNotFound, "release_operation_not_found")

	developerCancel := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
		"developer-cancel-operation",
		"",
	)
	defer developerCancel.Body.Close()
	if developerCancel.StatusCode != http.StatusOK {
		t.Errorf("developer cancel status = %d, want %d", developerCancel.StatusCode, http.StatusOK)
	}

	developerMemberWrite := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/projects/"+project.ID+"/members",
		"developer-add-member",
		fmt.Sprintf(`{"userId":%q,"role":"viewer"}`, unauthorizedUser.ID.String()),
	)
	defer developerMemberWrite.Body.Close()
	assertError(t, developerMemberWrite, http.StatusForbidden, "project_permission_denied")

	addMember(t, ownerServer, project.ID, temporaryUser.ID.String(), "viewer", "add-temporary-member")
	updatedMember := updateMember(
		t,
		ownerServer,
		project.ID,
		temporaryUser.ID.String(),
		"developer",
		"update-temporary-member",
	)
	replayedUpdate := updateMember(
		t,
		ownerServer,
		project.ID,
		temporaryUser.ID.String(),
		"developer",
		"update-temporary-member",
	)
	if replayedUpdate != updatedMember {
		t.Errorf("replayed member update = %#v, want %#v", replayedUpdate, updatedMember)
	}
	removedMember := removeMember(
		t,
		ownerServer,
		project.ID,
		temporaryUser.ID.String(),
		"remove-temporary-member",
	)
	replayedRemove := removeMember(
		t,
		ownerServer,
		project.ID,
		temporaryUser.ID.String(),
		"remove-temporary-member",
	)
	if replayedRemove != removedMember {
		t.Errorf("replayed member removal = %#v, want %#v", replayedRemove, removedMember)
	}

	outsiderRead := requestJSON(
		t,
		outsiderServer,
		http.MethodGet,
		"/api/v1/projects/"+project.ID,
		"",
		"",
	)
	defer outsiderRead.Body.Close()
	assertError(t, outsiderRead, http.StatusNotFound, "project_not_found")
	for _, path := range []string{
		"/api/v1/applications/" + application.ID,
		"/api/v1/deployment-targets/" + target.ID,
		"/api/v1/deployment-targets/" + target.ID + "/releases",
		"/api/v1/releases/" + acceptance.Release.ID,
		"/api/v1/release-operations/" + acceptance.ReleaseOperation.ID,
	} {
		response := requestJSON(t, outsiderServer, http.MethodGet, path, "", "")
		if response.StatusCode != http.StatusNotFound {
			response.Body.Close()
			t.Errorf("outsider GET %s status = %d, want %d", path, response.StatusCode, http.StatusNotFound)
			continue
		}
		response.Body.Close()
	}
	outsiderRollback := requestJSON(
		t,
		outsiderServer,
		http.MethodPost,
		"/api/v1/releases/"+acceptance.Release.ID+"/rollback",
		"outsider-rollback-release",
		"",
	)
	defer outsiderRollback.Body.Close()
	assertError(t, outsiderRollback, http.StatusNotFound, "release_not_found")

	outsiderWrite := requestJSON(
		t,
		outsiderServer,
		http.MethodPost,
		"/api/v1/projects/"+project.ID+"/applications",
		"outsider-create-application",
		`{"name":"Hidden API","slug":"hidden-api"}`,
	)
	defer outsiderWrite.Body.Close()
	assertError(t, outsiderWrite, http.StatusNotFound, "project_not_found")

	demoteLastOwner := requestJSON(
		t,
		ownerServer,
		http.MethodPut,
		"/api/v1/projects/"+project.ID+"/members/"+environment.users["local-developer"].ID.String(),
		"demote-last-owner",
		`{"role":"developer"}`,
	)
	defer demoteLastOwner.Body.Close()
	assertError(t, demoteLastOwner, http.StatusConflict, "last_project_owner")

	removeLastOwner := requestJSON(
		t,
		ownerServer,
		http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/members/"+environment.users["local-developer"].ID.String(),
		"remove-last-owner",
		"",
	)
	defer removeLastOwner.Body.Close()
	assertError(t, removeLastOwner, http.StatusConflict, "last_project_owner")

	assertOneMemberAudit(t, environment.databaseURL, project.ID, developerUser.ID.String())
}

func requestJSON(
	t *testing.T,
	server *httptest.Server,
	method string,
	path string,
	idempotencyKey string,
	body string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		method,
		server.URL+path,
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Origin", integrationOrigin)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("X-Orbit-CSRF", "1")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return response
}

func addMember(
	t *testing.T,
	server *httptest.Server,
	projectID string,
	userID string,
	role string,
	idempotencyKey string,
) projectMemberDocument {
	t.Helper()
	response := requestJSON(
		t,
		server,
		http.MethodPost,
		"/api/v1/projects/"+projectID+"/members",
		idempotencyKey,
		fmt.Sprintf(`{"userId":%q,"role":%q}`, userID, role),
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("add member status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var member projectMemberDocument
	if err := json.NewDecoder(response.Body).Decode(&member); err != nil {
		t.Fatalf("decode project member: %v", err)
	}
	return member
}

func updateMember(
	t *testing.T,
	server *httptest.Server,
	projectID string,
	userID string,
	role string,
	idempotencyKey string,
) projectMemberDocument {
	t.Helper()
	response := requestJSON(
		t,
		server,
		http.MethodPut,
		"/api/v1/projects/"+projectID+"/members/"+userID,
		idempotencyKey,
		fmt.Sprintf(`{"role":%q}`, role),
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("update member status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var member projectMemberDocument
	if err := json.NewDecoder(response.Body).Decode(&member); err != nil {
		t.Fatalf("decode updated project member: %v", err)
	}
	return member
}

func removeMember(
	t *testing.T,
	server *httptest.Server,
	projectID string,
	userID string,
	idempotencyKey string,
) projectMemberDocument {
	t.Helper()
	response := requestJSON(
		t,
		server,
		http.MethodDelete,
		"/api/v1/projects/"+projectID+"/members/"+userID,
		idempotencyKey,
		"",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("remove member status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var member projectMemberDocument
	if err := json.NewDecoder(response.Body).Decode(&member); err != nil {
		t.Fatalf("decode removed project member: %v", err)
	}
	return member
}

func decodeMembers(t *testing.T, response *http.Response) []projectMemberDocument {
	t.Helper()
	var page struct {
		Items []projectMemberDocument `json:"items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&page); err != nil {
		t.Fatalf("decode project members: %v", err)
	}
	return page.Items
}

func assertError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d", response.StatusCode, status)
	}
	var document errorDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if document.Code != code {
		t.Fatalf("error code = %q, want %q", document.Code, code)
	}
}

func assertOneMemberAudit(t *testing.T, databaseURL string, projectID string, memberUserID string) {
	t.Helper()
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open audit database: %v", err)
	}
	defer database.Close()

	var count int
	if err := database.QueryRowContext(
		context.Background(),
		`SELECT count(*)
		 FROM audit_records
		 WHERE target_id = $1
		   AND action = 'project_member.add'
		   AND actor_kind = 'user'
		   AND summary->>'memberUserId' = $2`,
		projectID,
		memberUserID,
	).Scan(&count); err != nil {
		t.Fatalf("count member audit records: %v", err)
	}
	if count != 1 {
		t.Fatalf("member audit count = %d, want 1", count)
	}
}
