package envconfig_test

import (
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
)

func TestProcessesUseBoundedDatabaseDefaults(t *testing.T) {
	t.Setenv("ORBIT_DEVOPS_GATEWAY_CLASS_NAME", "test-gateway")
	for _, name := range []string{"ORBIT_DEVOPS_DB_MAX_OPEN_CONNS", "ORBIT_DEVOPS_DB_MAX_IDLE_CONNS",
		"ORBIT_DEVOPS_DB_CONN_MAX_IDLE_TIME", "ORBIT_DEVOPS_DB_CONN_MAX_LIFETIME", "ORBIT_DEVOPS_DB_PING_TIMEOUT"} {
		t.Setenv(name, "")
	}
	api, err := envconfig.LoadAPI()
	if err != nil {
		t.Fatal(err)
	}
	release, err := envconfig.LoadReleaseWorker()
	if err != nil {
		t.Fatal(err)
	}
	build, err := envconfig.LoadBuildWorker()
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := envconfig.LoadPipelineWorker()
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := envconfig.LoadGatewayWorker()
	if err != nil {
		t.Fatal(err)
	}
	if api.DatabasePool.MaxOpenConns != 20 || release.DatabasePool.MaxOpenConns != 12 ||
		build.DatabasePool.MaxOpenConns != 8 || pipeline.DatabasePool.MaxOpenConns != 12 || gateway.DatabasePool.MaxOpenConns != 8 {
		t.Fatal("process connection budgets differ from the agreed defaults")
	}
	if api.DatabasePool.MaxIdleConns != 4 || api.DatabasePool.ConnMaxIdleTime != 5*time.Minute ||
		api.DatabasePool.ConnMaxLifetime != 30*time.Minute || api.DatabasePool.PingTimeout != 3*time.Second {
		t.Fatal("database lifecycle is not bounded")
	}
}

func TestDatabasePoolRejectsInvalidBudgets(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"ORBIT_DEVOPS_DB_MAX_OPEN_CONNS", "0"},
		{"ORBIT_DEVOPS_DB_MAX_IDLE_CONNS", "21"},
		{"ORBIT_DEVOPS_DB_MAX_IDLE_CONNS", "-1"},
		{"ORBIT_DEVOPS_DB_PING_TIMEOUT", "0s"},
		{"ORBIT_DEVOPS_DB_CONN_MAX_LIFETIME", "-1s"},
		{"ORBIT_DEVOPS_DB_CONN_MAX_IDLE_TIME", "forever"},
	} {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := envconfig.LoadAPI(); err == nil {
				t.Fatal("invalid database budget accepted")
			}
		})
	}
	t.Setenv("ORBIT_DEVOPS_DB_MAX_IDLE_CONNS", "0")
	if _, err := envconfig.LoadAPI(); err != nil {
		t.Fatalf("zero idle connections rejected: %v", err)
	}
}
