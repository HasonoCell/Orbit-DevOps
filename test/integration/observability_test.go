package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHealthAndMetricsExposeControlPlaneState(t *testing.T) {
	environment := newTestEnvironment(t)
	delayed := createRelease(t, environment, "metrics-delayed")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(
		db,
		releaseoperation.WithAutomaticRetryPolicy(2, func(int) time.Duration { return time.Hour }),
	)
	lease := claimReleaseOperation(t, operations, "metrics-worker")
	if lease.ReleaseOperationID.String() != delayed.ReleaseOperation.ID {
		t.Fatalf("metrics operation claim = %s, want %s", lease.ReleaseOperationID, delayed.ReleaseOperation.ID)
	}
	if _, err := operations.Fail(context.Background(), lease, releaseoperation.Failure{
		Code: "temporary_outage", Summary: "temporary controlled failure", Disposition: releaseoperation.Retryable,
	}); err != nil {
		t.Fatalf("schedule delayed metrics operation: %v", err)
	}
	createRelease(t, environment, "metrics-available")

	denied := environment.get(
		t,
		"/api/v1/projects/00000000-0000-4000-8000-000000000001",
	)
	denied.Body.Close()
	if denied.StatusCode != http.StatusNotFound {
		t.Fatalf("metrics authorization denial status = %d", denied.StatusCode)
	}
	firstProject := environment.postProject(
		t,
		"metrics-idempotency-conflict",
		`{"name":"Metrics One","slug":"metrics-one"}`,
	)
	firstProject.Body.Close()
	conflict := environment.postProject(
		t,
		"metrics-idempotency-conflict",
		`{"name":"Metrics Two","slug":"metrics-two"}`,
	)
	conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Fatalf("metrics idempotency conflict status = %d", conflict.StatusCode)
	}
	snapshot, err := operations.ReadMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("read operation metrics snapshot: %v", err)
	}
	if snapshot.PendingAvailable != 1 || snapshot.PendingDelayed != 1 {
		t.Fatalf("pending operation metrics snapshot = %#v", snapshot)
	}

	health := environment.get(t, "/healthz")
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.StatusCode, http.StatusOK)
	}

	metrics := environment.get(t, "/metrics")
	defer metrics.Body.Close()
	payload, err := io.ReadAll(metrics.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	text := string(payload)
	for _, want := range []string{
		"orbit_devops_http_requests_total",
		"orbit_devops_http_request_duration_seconds",
		"orbit_devops_pending_release_operations 2",
		`orbit_devops_release_operation_status{status="pending"} 2`,
		`orbit_devops_pending_release_operation_state{availability="available"} 1`,
		`orbit_devops_pending_release_operation_state{availability="delayed"} 1`,
		`orbit_devops_release_operation_events{event="operation.claimed"} 1`,
		`orbit_devops_release_attempt_errors{error_code="temporary_outage"} 1`,
		`orbit_devops_authorization_denials_total{reason="not_member"} 1`,
		`orbit_devops_idempotency_conflicts_total{command="project.create"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
}

func TestReleaseRequestLogIncludesControlPlaneCorrelations(t *testing.T) {
	var output bytes.Buffer
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		Logger: slog.New(slog.NewJSONHandler(&output, nil)),
	})
	target := createDeploymentTarget(t, environment)
	output.Reset()

	response := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"correlated-release",
		`{"imageReference":"registry.example/orbit-devops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create release status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatalf("decode request log: %v\n%s", err, output.String())
	}
	for key, want := range map[string]string{
		"actor_id":        "local-developer",
		"project_id":      target.ProjectID,
		"idempotency_key": "correlated-release",
		"route":           "/api/v1/deployment-targets/:deploymentTargetId/releases",
	} {
		if got := entry[key]; got != want {
			t.Errorf("log %s = %#v, want %q", key, got, want)
		}
	}
	if requestID, ok := entry["request_id"].(string); !ok || requestID == "" {
		t.Errorf("log request_id = %#v, want a non-empty string", entry["request_id"])
	}
}

func TestReleaseTraceContinuesIntoWorkerAttempt(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown trace provider: %v", err)
		}
	})
	propagator := propagation.TraceContext{}
	tracer := provider.Tracer("orbit-devops-test")
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		Tracer:     tracer,
		Propagator: propagator,
	})
	target := createDeploymentTarget(t, environment)

	requestContext, rootSpan := tracer.Start(context.Background(), "test-client")
	rootTraceID := trace.SpanContextFromContext(requestContext).TraceID()
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		environment.server.URL+"/api/v1/deployment-targets/"+target.ID+"/releases",
		bytes.NewBufferString(`{"imageReference":"registry.example/orbit-devops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`),
	)
	if err != nil {
		t.Fatalf("build release request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "trace-release")
	propagator.Inject(requestContext, propagation.HeaderCarrier(request.Header))
	response, err := environment.server.Client().Do(request)
	rootSpan.End()
	if err != nil {
		t.Fatalf("create traced release: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create traced release status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
	var acceptance releaseAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode traced release: %v", err)
	}

	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	releases := delivery.New(db, operations, projectauth.New(db))
	publisher := &traceRecordingPublisher{}
	runner, err := releaseworker.New(releaseworker.Config{
		WorkerID:                "trace-worker",
		LeaseDuration:           time.Second,
		ReleaseOperationTimeout: time.Second,
		Tracer:                  tracer,
		Propagator:              propagator,
	}, operations, releases, publisher)
	if err != nil {
		t.Fatalf("create traced worker: %v", err)
	}
	processed, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run traced worker: %v", err)
	}
	if !processed {
		t.Fatal("traced worker did not process the release")
	}
	if publisher.traceID != rootTraceID {
		t.Errorf("publisher trace ID = %s, want %s", publisher.traceID, rootTraceID)
	}
	if !publisher.spanContext.IsValid() {
		t.Fatal("publisher received no valid worker span context")
	}
}

type traceRecordingPublisher struct {
	traceID     trace.TraceID
	spanContext trace.SpanContext
}

func (p *traceRecordingPublisher) Publish(ctx context.Context, _ releaseworker.PublishRequest) error {
	p.spanContext = trace.SpanContextFromContext(ctx)
	p.traceID = p.spanContext.TraceID()
	return nil
}
