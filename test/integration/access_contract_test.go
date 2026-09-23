package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// TestAccessHostAndRoutePermissions 从公开 HTTP 契约验证 Host 与 Route 的分层权限。
func TestAccessHostAndRoutePermissions(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "access-project")
	application := createApplication(t, environment, project.ID, "access-application")
	targetResponse := requestJSON(t, environment.server, http.MethodPost,
		"/api/v1/applications/"+application.ID+"/deployment-targets", "access-target",
		`{"stage":"development","replicas":1,"containerPort":8080}`)
	defer targetResponse.Body.Close()
	if targetResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create target status = %d", targetResponse.StatusCode)
	}
	target := decodeDeploymentTarget(t, targetResponse)

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

	created := requestJSON(t, environment.server, http.MethodPost, path, "access-create-host",
		`{"hostname":"Pay.Example.Test.","tlsMode":"http_only"}`)
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create host status = %d", created.StatusCode)
	}
	var host accessHostDocument
	if err := json.NewDecoder(created.Body).Decode(&host); err != nil {
		t.Fatal(err)
	}
	if host.ID == "" || host.ProjectID != project.ID || host.Hostname != "pay.example.test" || host.TLSMode != "http_only" {
		t.Fatalf("host = %#v", host)
	}
	replayed := requestJSON(t, environment.server, http.MethodPost, path, "access-create-host",
		`{"hostname":"pay.example.test","tlsMode":"http_only"}`)
	defer replayed.Body.Close()
	if replayed.StatusCode != http.StatusCreated {
		t.Fatalf("replay host status = %d", replayed.StatusCode)
	}
	var sameHost accessHostDocument
	if err := json.NewDecoder(replayed.Body).Decode(&sameHost); err != nil {
		t.Fatal(err)
	}
	if sameHost.ID != host.ID {
		t.Fatalf("replayed host ID = %q, want %q", sameHost.ID, host.ID)
	}
	conflicting := requestJSON(t, environment.server, http.MethodPost, path, "access-create-host-again",
		`{"hostname":"pay.example.test","tlsMode":"http_only"}`)
	defer conflicting.Body.Close()
	assertError(t, conflicting, http.StatusConflict, "access_host_conflict")

	routePath := path + "/" + host.ID + "/routes"
	routeResponse := requestJSON(t, developerServer, http.MethodPost, routePath, "access-create-route",
		fmt.Sprintf(`{"pathPrefix":"/api","deploymentTargetId":%q}`, target.ID))
	defer routeResponse.Body.Close()
	if routeResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create route status = %d", routeResponse.StatusCode)
	}
	var route accessRouteDocument
	if err := json.NewDecoder(routeResponse.Body).Decode(&route); err != nil {
		t.Fatal(err)
	}
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
	deletedHost := requestJSON(t, environment.server, http.MethodDelete, path+"/"+host.ID, "access-delete-host", "")
	defer deletedHost.Body.Close()
	if deletedHost.StatusCode != http.StatusAccepted {
		t.Fatalf("delete host status = %d", deletedHost.StatusCode)
	}
	var tombstone accessHostDocument
	if err := json.NewDecoder(deletedHost.Body).Decode(&tombstone); err != nil {
		t.Fatal(err)
	}
	if tombstone.ID != host.ID {
		t.Fatalf("tombstone ID = %q, want %q", tombstone.ID, host.ID)
	}
}
