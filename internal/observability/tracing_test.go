package observability_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/observability"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/propagation"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestTracingRejectsUnsafeEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", "http://example.com/v1/traces", "https://test:secret@example.com/v1/traces", "https://example.com/v1/traces?token=secret", "https://example.com/v1/traces#token"} {
		config := observability.DefaultTraceConfig()
		config.Exporter, config.Endpoint = "otlp", endpoint
		if config.Validate() == nil {
			t.Fatalf("unsafe endpoint accepted")
		}
	}
}

func TestMalformedOTLPHeadersFailWithoutExposingMaterial(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "private-secret-probe")
	config := observability.DefaultTraceConfig()
	config.Exporter, config.Endpoint = "otlp", "http://127.0.0.1:1/v1/traces"
	_, err := observability.NewTracing(context.Background(), config, nil, "test")
	if err == nil || strings.Contains(err.Error(), "private-secret-probe") {
		t.Fatal("invalid Header was not safely rejected")
	}
}

func TestSlowTraceReceiverDoesNotBlockSpanEndAndShutdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	config := observability.DefaultTraceConfig()
	config.Exporter, config.Endpoint = "otlp", server.URL+"/v1/traces"
	config.ExportTimeout, config.ShutdownTimeout = 30*time.Millisecond, 100*time.Millisecond
	tracing, err := observability.NewTracing(context.Background(), config, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for range 3000 {
		_, span := tracing.Provider.Tracer("test").Start(context.Background(), "bounded")
		span.End()
	}
	if time.Since(started) > time.Second {
		t.Fatal("full trace queue blocked business execution")
	}
	started = time.Now()
	_ = tracing.Shutdown(context.Background())
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("shutdown exceeded export budget")
	}
	if testutil.ToFloat64(tracing.ExportFailures) == 0 {
		t.Fatal("failed export was not counted")
	}
}

func TestDisabledTracingStillPropagatesW3CContext(t *testing.T) {
	config := observability.DefaultTraceConfig()
	config.Exporter = "off"
	tracing, err := observability.NewTracing(context.Background(), config, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	parent := "00-11111111111111111111111111111111-2222222222222222-01"
	ctx := tracing.Propagator.Extract(context.Background(), propagation.MapCarrier{"traceparent": parent})
	ctx, span := tracing.Provider.Tracer("test").Start(ctx, "noop")
	span.End()
	carrier := propagation.MapCarrier{}
	tracing.Propagator.Inject(ctx, carrier)
	if carrier.Get("traceparent") != parent {
		t.Fatal("disabled exporter discarded parent context")
	}
}

func TestTracingExportsRealProtobufAndRespectsParentSampling(t *testing.T) {
	received := make(chan *coltrace.ExportTraceServiceRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/traces" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("incorrect OTLP endpoint or encoding")
		}
		body, _ := io.ReadAll(r.Body)
		request := new(coltrace.ExportTraceServiceRequest)
		if err := proto.Unmarshal(body, request); err != nil {
			t.Error(err)
		}
		received <- request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	config := observability.DefaultTraceConfig()
	config.Exporter, config.Endpoint, config.SampleRatio = "otlp", server.URL+"/custom/traces", 0
	tracing, err := observability.NewTracing(context.Background(), config, slog.New(slog.NewTextHandler(io.Discard, nil)), "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := tracing.Propagator.Extract(context.Background(), propagation.MapCarrier{
		"traceparent": "00-11111111111111111111111111111111-2222222222222222-01"})
	_, span := tracing.Provider.Tracer("test").Start(ctx, "sampled parent")
	span.End()
	_, span = tracing.Provider.Tracer("test").Start(context.Background(), "unsampled root")
	span.End()
	if err := tracing.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-received:
		spans := request.ResourceSpans[0].ScopeSpans[0].Spans
		if len(spans) != 1 || spans[0].Name != "sampled parent" || len(spans[0].ParentSpanId) != 8 {
			t.Fatal("parent sampling or lineage changed")
		}
	case <-time.After(time.Second):
		t.Fatal("OTLP export not received")
	}
}
