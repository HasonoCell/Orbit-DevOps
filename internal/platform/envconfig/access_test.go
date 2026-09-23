package envconfig_test

import (
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
)

func TestGatewayWorkerRequiresControlledClassAndIssuerPolicy(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_GATEWAY_CLASS_NAME", "")
	if _, err := envconfig.LoadGatewayWorker(); err == nil {
		t.Fatal("GatewayClass must be explicitly selected")
	}
	t.Setenv("ORBIT_DEVOPS_GATEWAY_CLASS_NAME", "local-gateway")
	t.Setenv("ORBIT_DEVOPS_ACCESS_ISSUER_POLICIES_JSON", `{"local":{"kind":"ClusterIssuer","name":"local-ca"}}`)
	config, err := envconfig.LoadGatewayWorker()
	if err != nil {
		t.Fatal(err)
	}
	if config.Access.IssuerPolicies["local"].Name != "local-ca" || config.Queue.Name != "orbit-devops-gateway" {
		t.Fatalf("unexpected access policy: %+v", config.Access)
	}
	t.Setenv("ORBIT_DEVOPS_ACCESS_ISSUER_POLICIES_JSON", `{"bad":{"kind":"Secret","name":"arbitrary"}}`)
	if _, err := envconfig.LoadGatewayWorker(); err == nil {
		t.Fatal("uncontrolled issuer kind must fail")
	}
}
