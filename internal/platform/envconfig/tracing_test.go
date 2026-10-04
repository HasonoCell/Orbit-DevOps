package envconfig_test

import (
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/envconfig"
	"testing"
)

func TestTraceConfigurationRejectsUnsupportedOrUnboundedValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"ORBIT_DEVOPS_TRACE_EXPORTER", "unknown"},
		{"ORBIT_DEVOPS_TRACE_SAMPLE_RATIO", "NaN"},
		{"ORBIT_DEVOPS_TRACE_SAMPLE_RATIO", "1.1"},
		{"ORBIT_DEVOPS_TRACE_EXPORT_TIMEOUT", "0s"},
		{"ORBIT_DEVOPS_TRACE_SHUTDOWN_TIMEOUT", "-1s"},
		{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := envconfig.LoadAPI(); err == nil {
				t.Fatal("invalid tracing config accepted")
			}
		})
	}
}
