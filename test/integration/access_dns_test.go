package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/app"
)

type acceptanceDNSResolver struct {
	answers map[string][]net.IPAddr
	errors  map[string]error
}

func (r acceptanceDNSResolver) LookupIPAddr(_ context.Context, hostname string) ([]net.IPAddr, error) {
	if err := r.errors[hostname]; err != nil {
		return nil, err
	}
	return r.answers[hostname], nil
}

type acceptanceControllerObserver struct{}

func (acceptanceControllerObserver) ObserveHost(_ context.Context, _ access.Snapshot, _ access.HostSpec,
	_ []access.RouteSpec) (access.ControllerObservation, error) {
	return access.ControllerObservation{GatewayState: "ready", ListenerState: "ready",
		CertificateState: "not_applicable", SecretState: "not_applicable",
		Routes: []access.RouteObservation{}, Addresses: []string{"192.0.2.10"}, ObservedAt: time.Now().UTC()}, nil
}

// TestAccessDNSStatusRemainsIndependentOfControllerReady 通过公开状态接口覆盖四种 DNS 结果。
func TestAccessDNSStatusRemainsIndependentOfControllerReady(t *testing.T) {
	resolver := acceptanceDNSResolver{answers: map[string][]net.IPAddr{
		"verified.example.test": {{IP: net.ParseIP("192.0.2.10")}},
		"mismatch.example.test": {{IP: net.ParseIP("192.0.2.11")}},
		"empty.example.test":    {},
	}, errors: map[string]error{"failed.example.test": errors.New("fixture resolver unavailable")}}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		AccessObserver: acceptanceControllerObserver{}, AccessDNSResolver: resolver,
	})
	project := createProject(t, environment, "access-dns")
	for _, scenario := range []struct {
		hostname, state, code string
	}{
		{"verified.example.test", "verified", ""},
		{"mismatch.example.test", "mismatch", ""},
		{"empty.example.test", "unavailable", "dns_no_records"},
		{"failed.example.test", "unavailable", "dns_lookup_failed"},
	} {
		t.Run(scenario.hostname, func(t *testing.T) {
			path := "/api/v1/projects/" + project.ID + "/access-hosts"
			host := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, path,
				"dns-"+scenario.hostname,
				fmt.Sprintf(`{"hostname":%q,"tlsMode":"http_only"}`, scenario.hostname), http.StatusCreated)
			status := requestJSON(t, environment.server, http.MethodGet,
				path+"/"+host.ID+"/status", "", "")
			defer status.Body.Close()
			if status.StatusCode != http.StatusOK {
				t.Fatalf("read status = %d", status.StatusCode)
			}
			var document struct {
				Controller struct {
					GatewayState string `json:"gatewayState"`
				} `json:"controller"`
				DNS struct {
					State     string `json:"state"`
					ErrorCode string `json:"errorCode"`
				} `json:"dns"`
			}
			if err := json.NewDecoder(status.Body).Decode(&document); err != nil {
				t.Fatal(err)
			}
			if document.Controller.GatewayState != "ready" || document.DNS.State != scenario.state ||
				document.DNS.ErrorCode != scenario.code {
				t.Fatalf("controller=%q DNS=%+v, want DNS=%q/%q", document.Controller.GatewayState,
					document.DNS, scenario.state, scenario.code)
			}
		})
	}
}
