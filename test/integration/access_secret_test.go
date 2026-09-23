package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"errors"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
)

type localTLSSecretFixture struct{}

func (localTLSSecretFixture) VerifyTLSSecret(_ context.Context, namespace, name, hostname string) error {
	if namespace == "orbit-devops-s1" && name == "pay-tls" && hostname == "pay.example.test" {
		return nil
	}
	return errors.New("fixture Secret is unavailable")
}

type accessSecretBindingDocument struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	Hostname   string `json:"hostname"`
	SecretName string `json:"secretName"`
	State      string `json:"state"`
}

// TestTLSSecretRegistrationIsPlatformScoped 检查登记权与项目 Host 使用权相互独立。
func TestTLSSecretRegistrationIsPlatformScoped(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{SecretVerifier: localTLSSecretFixture{}})
	project := createProject(t, environment, "binding-project")
	developerUser := environment.ensureActor(t, "binding-developer")
	addMember(t, environment.server, project.ID, developerUser.ID.String(), "developer", "binding-add-developer")
	developer := environment.serverForActor(t, "binding-developer")
	requestBody := fmt.Sprintf(`{"projectId":%q,"hostname":"Pay.Example.Test.","secretName":"pay-tls"}`, project.ID)
	denied := requestJSON(t, developer, http.MethodPost, "/api/v1/platform/access-secret-bindings", "binding-denied", requestBody)
	defer denied.Body.Close()
	assertError(t, denied, http.StatusForbidden, "platform_permission_denied")

	registered := requestJSON(t, environment.server, http.MethodPost, "/api/v1/platform/access-secret-bindings", "binding-register", requestBody)
	defer registered.Body.Close()
	if registered.StatusCode != http.StatusCreated {
		t.Fatalf("register binding status = %d", registered.StatusCode)
	}
	var binding accessSecretBindingDocument
	if err := json.NewDecoder(registered.Body).Decode(&binding); err != nil {
		t.Fatal(err)
	}
	if binding.ID == "" || binding.ProjectID != project.ID || binding.Hostname != "pay.example.test" || binding.SecretName != "pay-tls" || binding.State != "active" {
		t.Fatalf("binding = %#v", binding)
	}

	hostPath := "/api/v1/projects/" + project.ID + "/access-hosts"
	host := requestJSON(t, environment.server, http.MethodPost, hostPath, "binding-host",
		fmt.Sprintf(`{"hostname":"pay.example.test","tlsMode":"existing_secret","secretBindingId":%q}`, binding.ID))
	defer host.Body.Close()
	if host.StatusCode != http.StatusCreated {
		t.Fatalf("host using registered Secret status = %d", host.StatusCode)
	}

	revoked := requestJSON(t, environment.server, http.MethodDelete,
		"/api/v1/platform/access-secret-bindings/"+binding.ID, "binding-revoke", "")
	defer revoked.Body.Close()
	if revoked.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d", revoked.StatusCode)
	}
	var revokedBinding accessSecretBindingDocument
	if err := json.NewDecoder(revoked.Body).Decode(&revokedBinding); err != nil {
		t.Fatal(err)
	}
	if revokedBinding.State != "revoked" {
		t.Fatalf("revoked binding state = %q", revokedBinding.State)
	}
}
