package envconfig

import (
	"errors"
	"os"
	"strconv"

	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
)

func loadTracing() (observability.TraceConfig, error) {
	config := observability.DefaultTraceConfig()
	config.Exporter = value("ORBIT_DEVOPS_TRACE_EXPORTER", config.Exporter)
	config.Endpoint = os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	config.Protocol = value("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", value("OTEL_EXPORTER_OTLP_PROTOCOL", config.Protocol))
	var err error
	config.SampleRatio, err = strconv.ParseFloat(value("ORBIT_DEVOPS_TRACE_SAMPLE_RATIO", "1"), 64)
	if err != nil {
		return config, errors.New("invalid trace sample ratio")
	}
	config.ExportTimeout, err = duration("ORBIT_DEVOPS_TRACE_EXPORT_TIMEOUT", config.ExportTimeout)
	if err != nil {
		return config, err
	}
	config.ShutdownTimeout, err = duration("ORBIT_DEVOPS_TRACE_SHUTDOWN_TIMEOUT", config.ShutdownTimeout)
	if err != nil {
		return config, err
	}
	return config, config.Validate()
}
