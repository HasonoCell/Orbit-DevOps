package observability_test

import (
	"context"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestMissingPersistedTraceDoesNotInheritTransportContext(t *testing.T) {
	propagator := propagation.TraceContext{}
	ctx := propagator.Extract(context.Background(), propagation.MapCarrier{"traceparent": "00-11111111111111111111111111111111-2222222222222222-01"})
	for _, parent := range []string{"", "invalid"} {
		if trace.SpanContextFromContext(observability.RestoreTrace(ctx, parent, "", propagator)).IsValid() {
			t.Fatal("missing persisted parent inherited accidental transport trace")
		}
	}
}
