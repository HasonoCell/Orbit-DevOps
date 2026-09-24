package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type accessHostDocument struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Hostname  string `json:"hostname"`
	TLSMode   string `json:"tlsMode"`
}

type accessRouteDocument struct {
	ID                 string `json:"id"`
	HostID             string `json:"hostId"`
	PathPrefix         string `json:"pathPrefix"`
	DeploymentTargetID string `json:"deploymentTargetId"`
}

func createAccessTarget(t *testing.T, environment *testEnvironment, applicationID, key string, port int) deploymentTargetDocument {
	t.Helper()
	return requestAccessDocument[deploymentTargetDocument](t, environment.server, http.MethodPost,
		"/api/v1/applications/"+applicationID+"/deployment-targets", key,
		fmt.Sprintf(`{"stage":"development","replicas":1,"containerPort":%d}`, port), http.StatusCreated)
}

func requestAccessDocument[T any](t *testing.T, server *httptest.Server, method, path, key, body string, status int) T {
	t.Helper()
	response := requestJSON(t, server, method, path, key, body)
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("%s %s status = %d, want %d", method, path, response.StatusCode, status)
	}
	var document T
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return document
}

// TestAccessHostAndRoutePermissions 从公开 HTTP 契约验证 Host 与 Route 的分层权限。
func TestAccessHostAndRoutePermissions(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "access-project")
	application := createApplication(t, environment, project.ID, "access-application")
	target := createAccessTarget(t, environment, application.ID, "access-target", 8080)

	developer := environment.ensureActor(t, "access-developer")
	viewer := environment.ensureActor(t, "access-viewer")
	addMember(t, environment.server, project.ID, developer.ID.String(), "developer", "access-add-developer")
	addMember(t, environment.server, project.ID, viewer.ID.String(), "viewer", "access-add-viewer")
	developerServer := environment.serverForActor(t, "access-developer")
	viewerServer := environment.serverForActor(t, "access-viewer")
	path := "/api/v1/projects/" + project.ID + "/access-hosts"

	denied := requestJSON(t, developerServer, http.MethodPost, path, "access-developer-host",
		`{"hostname":"pay.example.test","tlsMode":"http_only"}`)
	defer denied.Body.Close()
	assertError(t, denied, http.StatusForbidden, "project_permission_denied")

	host := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, path,
		"access-create-host", `{"hostname":"Pay.Example.Test.","tlsMode":"http_only"}`, http.StatusCreated)
	if host.ID == "" || host.ProjectID != project.ID || host.Hostname != "pay.example.test" || host.TLSMode != "http_only" {
		t.Fatalf("host = %#v", host)
	}
	sameHost := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, path,
		"access-create-host", `{"hostname":"pay.example.test","tlsMode":"http_only"}`, http.StatusCreated)
	if sameHost.ID != host.ID {
		t.Fatalf("replayed host ID = %q, want %q", sameHost.ID, host.ID)
	}
	conflicting := requestJSON(t, environment.server, http.MethodPost, path, "access-create-host-again",
		`{"hostname":"pay.example.test","tlsMode":"http_only"}`)
	defer conflicting.Body.Close()
	assertError(t, conflicting, http.StatusConflict, "access_host_conflict")

	routePath := path + "/" + host.ID + "/routes"
	route := requestAccessDocument[accessRouteDocument](t, developerServer, http.MethodPost, routePath,
		"access-create-route", fmt.Sprintf(`{"pathPrefix":"/api","deploymentTargetId":%q}`, target.ID), http.StatusCreated)
	if route.ID == "" || route.HostID != host.ID || route.PathPrefix != "/api" || route.DeploymentTargetID != target.ID {
		t.Fatalf("route = %#v", route)
	}
	duplicatePath := requestJSON(t, developerServer, http.MethodPost, routePath, "access-duplicate-route",
		fmt.Sprintf(`{"pathPrefix":"/api/","deploymentTargetId":%q}`, target.ID))
	defer duplicatePath.Body.Close()
	assertError(t, duplicatePath, http.StatusConflict, "access_route_conflict")

	viewerWrite := requestJSON(t, viewerServer, http.MethodPost, routePath, "access-viewer-route",
		fmt.Sprintf(`{"pathPrefix":"/viewer","deploymentTargetId":%q}`, target.ID))
	defer viewerWrite.Body.Close()
	assertError(t, viewerWrite, http.StatusForbidden, "project_permission_denied")

	viewerRead := requestJSON(t, viewerServer, http.MethodGet, path+"/"+host.ID, "", "")
	defer viewerRead.Body.Close()
	if viewerRead.StatusCode != http.StatusOK {
		t.Errorf("viewer host status = %d", viewerRead.StatusCode)
	}

	deletedRoute := requestJSON(t, developerServer, http.MethodDelete, routePath+"/"+route.ID, "access-delete-route", "")
	defer deletedRoute.Body.Close()
	if deletedRoute.StatusCode != http.StatusAccepted {
		t.Fatalf("delete route status = %d", deletedRoute.StatusCode)
	}
	stillThere := requestJSON(t, viewerServer, http.MethodGet, path+"/"+host.ID, "", "")
	defer stillThere.Body.Close()
	if stillThere.StatusCode != http.StatusOK {
		t.Fatalf("host after last route status = %d", stillThere.StatusCode)
	}
	tombstone := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodDelete,
		path+"/"+host.ID, "access-delete-host", "", http.StatusAccepted)
	if tombstone.ID != host.ID {
		t.Fatalf("tombstone ID = %q, want %q", tombstone.ID, host.ID)
	}
}

// TestAccessHostnameClaimAndRevokedMembership 从公开 API 验证数据库仲裁与即时授权。
func TestAccessHostnameClaimAndRevokedMembership(t *testing.T) {
	environment := newTestEnvironment(t)
	first := createProject(t, environment, "claim-first")
	second := requestAccessDocument[projectDocument](t, environment.server, http.MethodPost,
		"/api/v1/projects", "claim-second", `{"name":"Second","slug":"second"}`, http.StatusCreated)
	projects := []string{first.ID, second.ID}
	type outcome struct {
		status int
		code   string
		body   accessHostDocument
		err    error
	}
	results := make([]outcome, len(projects))
	var wait sync.WaitGroup
	for index, projectID := range projects {
		wait.Add(1)
		go func(index int, projectID string) {
			defer wait.Done()
			request, err := http.NewRequest(http.MethodPost, environment.server.URL+
				"/api/v1/projects/"+projectID+"/access-hosts",
				strings.NewReader(`{"hostname":"claim.example.test","tlsMode":"http_only"}`))
			if err != nil {
				results[index].err = err
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", integrationOrigin)
			request.Header.Set("X-Orbit-CSRF", "1")
			request.Header.Set("Idempotency-Key", fmt.Sprintf("claim-host-%d", index))
			response, err := environment.server.Client().Do(request)
			if err != nil {
				results[index].err = err
				return
			}
			defer response.Body.Close()
			results[index].status = response.StatusCode
			if response.StatusCode == http.StatusCreated {
				results[index].err = json.NewDecoder(response.Body).Decode(&results[index].body)
			} else {
				var document errorDocument
				results[index].err = json.NewDecoder(response.Body).Decode(&document)
				results[index].code = document.Code
			}
		}(index, projectID)
	}
	wait.Wait()
	created, conflicted := 0, 0
	for index, result := range results {
		if result.err != nil {
			t.Fatalf("claim %d: %v", index, result.err)
		}
		switch result.status {
		case http.StatusCreated:
			created++
			if result.body.ProjectID != projects[index] {
				t.Fatalf("claim %d belongs to project %q", index, result.body.ProjectID)
			}
		case http.StatusConflict:
			conflicted++
			if result.code != "access_host_conflict" {
				t.Fatalf("claim %d code = %q", index, result.code)
			}
		default:
			t.Fatalf("claim %d status = %d", index, result.status)
		}
	}
	if created != 1 || conflicted != 1 {
		t.Fatalf("claims = %+v, want one created and one conflict", results)
	}

	member := environment.ensureActor(t, "claim-developer")
	addMember(t, environment.server, first.ID, member.ID.String(), "developer", "claim-add-developer")
	developer := environment.serverForActor(t, "claim-developer")
	host := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost,
		"/api/v1/projects/"+first.ID+"/access-hosts", "claim-route-host",
		`{"hostname":"route-claim.example.test","tlsMode":"http_only"}`, http.StatusCreated)
	read := requestJSON(t, developer, http.MethodGet,
		"/api/v1/projects/"+first.ID+"/access-hosts/"+host.ID, "", "")
	defer read.Body.Close()
	if read.StatusCode != http.StatusOK {
		t.Fatalf("developer read status = %d", read.StatusCode)
	}
	application := createApplication(t, environment, first.ID, "claim-route-application")
	target := createAccessTarget(t, environment, application.ID, "claim-route-target", 8080)
	removeMember(t, environment.server, first.ID, member.ID.String(), "claim-remove-developer")
	denied := requestJSON(t, developer, http.MethodGet,
		"/api/v1/projects/"+first.ID+"/access-hosts/"+host.ID, "", "")
	defer denied.Body.Close()
	assertError(t, denied, http.StatusNotFound, "access_host_not_found")
	writeDenied := requestJSON(t, developer, http.MethodPost,
		"/api/v1/projects/"+first.ID+"/access-hosts/"+host.ID+"/routes", "claim-revoked-route",
		fmt.Sprintf(`{"pathPrefix":"/revoked","deploymentTargetId":%q}`, target.ID))
	defer writeDenied.Body.Close()
	assertError(t, writeDenied, http.StatusNotFound, "access_resource_not_found")
}

// TestAccessPlatformRoleDoesNotGrantProjectAccess 验证平台角色与项目角色分离，旧 Session 撤销后失效。
func TestAccessPlatformRoleDoesNotGrantProjectAccess(t *testing.T) {
	environment := newTestEnvironment(t)
	owner := environment.ensureActor(t, "access-private-owner")
	ownerServer := environment.serverForActor(t, "access-private-owner")
	created := requestJSON(t, ownerServer, http.MethodPost, "/api/v1/projects", "access-private-project",
		`{"name":"Private Access","slug":"private-access"}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create private Project status = %d", created.StatusCode)
	}
	project := decodeProject(t, created)
	if project.CreatedBy != owner.ID.String() {
		t.Fatalf("project owner = %q", project.CreatedBy)
	}
	path := "/api/v1/projects/" + project.ID + "/access-hosts"
	platformRead := requestJSON(t, environment.server, http.MethodGet, path, "", "")
	defer platformRead.Body.Close()
	assertError(t, platformRead, http.StatusNotFound, "project_not_found")
	platformWrite := requestJSON(t, environment.server, http.MethodPost, path, "platform-not-project-owner",
		`{"hostname":"platform.example.test","tlsMode":"http_only"}`)
	defer platformWrite.Body.Close()
	assertError(t, platformWrite, http.StatusNotFound, "project_not_found")

	projectAdmin := environment.ensureActor(t, "access-private-admin")
	addMember(t, ownerServer, project.ID, projectAdmin.ID.String(), "admin", "access-add-project-admin")
	adminServer := environment.serverForActor(t, "access-private-admin")
	adminWrite := requestJSON(t, adminServer, http.MethodPost, path, "project-admin-host",
		`{"hostname":"admin.example.test","tlsMode":"http_only"}`)
	defer adminWrite.Body.Close()
	if adminWrite.StatusCode != http.StatusCreated {
		t.Fatalf("project admin create Host status = %d", adminWrite.StatusCode)
	}
	parsed, err := url.Parse(adminServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldCookies := adminServer.Client().Jar.Cookies(parsed)
	if len(oldCookies) == 0 {
		t.Fatal("project admin login did not set Session Cookie")
	}
	logout := requestJSON(t, adminServer, http.MethodPost, "/api/v1/auth/logout", "", "")
	defer logout.Body.Close()
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
	oldSessionRequest, err := http.NewRequest(http.MethodPost, adminServer.URL+path,
		strings.NewReader(`{"hostname":"revoked.example.test","tlsMode":"http_only"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range oldCookies {
		oldSessionRequest.AddCookie(cookie)
	}
	oldSessionRequest.Header.Set("Content-Type", "application/json")
	oldSessionRequest.Header.Set("Origin", integrationOrigin)
	oldSessionRequest.Header.Set("X-Orbit-CSRF", "1")
	oldSessionRequest.Header.Set("Idempotency-Key", "revoked-admin-host")
	denied, err := http.DefaultClient.Do(oldSessionRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Body.Close()
	assertError(t, denied, http.StatusUnauthorized, "authentication_required")
}
