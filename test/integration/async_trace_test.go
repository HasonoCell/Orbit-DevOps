package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestBuildTraceContinuesFromAPIWithoutTracingDuplicateAttempts(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer provider.Shutdown(context.Background())
	tracer := provider.Tracer("test")
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{Tracer: tracer, Propagator: propagation.TraceContext{}})
	project := createProject(t, environment, "trace-build")
	application := createApplication(t, environment, project.ID, "trace-build")
	ctx, root := tracer.Start(context.Background(), "caller")
	response := postTraced(t, environment, ctx, "/api/v1/applications/"+application.ID+"/builds",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("a", 40)+`"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("build status = %d", response.StatusCode)
	}
	var accepted buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&accepted); err != nil {
		t.Fatal(err)
	}
	db := openTestDatabase(t, environment.databaseURL)
	operations := buildoperation.New(db)
	executor := &tracedBuildExecutor{memoryBuildExecutor: memoryBuildExecutor{repository: accepted.Build.DestinationRepository, digest: "sha256:" + strings.Repeat("b", 64)}}
	runner, err := buildworker.New(buildworker.Config{WorkerID: "trace-worker", LeaseDuration: time.Second, BuildTimeout: 5 * time.Second,
		PollInterval: time.Millisecond, Tracer: tracer, Propagator: propagation.TraceContext{}}, operations,
		build.New(db, build.Config{}, operations, projectauth.New(db, nil)), executor)
	if err != nil {
		t.Fatal(err)
	}
	ref := reserveBuildWorkerDispatch(t, operations)
	if _, err := runner.RunDispatch(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.RunDispatch(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	root.End()
	if executor.parent.TraceID() != trace.SpanContextFromContext(ctx).TraceID() {
		t.Fatal("build lost API lineage")
	}
	count := 0
	for _, span := range exporter.GetSpans() {
		if span.Name == "build execution attempt" {
			count++
			if span.Parent.SpanID() == root.SpanContext().SpanID() {
				t.Fatal("worker bypassed API command span")
			}
		}
	}
	if count != 1 {
		t.Fatalf("duplicate transport created %d attempt spans", count)
	}
}

type tracedBuildExecutor struct {
	memoryBuildExecutor
	parent trace.SpanContext
}

func (e *tracedBuildExecutor) Start(ctx context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	e.parent = trace.SpanContextFromContext(ctx)
	return e.memoryBuildExecutor.Start(ctx, execution)
}

func TestGatewayEventPersistsAndRestoresCommandTrace(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	defer provider.Shutdown(context.Background())
	tracer := provider.Tracer("test")
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{Tracer: tracer, Propagator: propagation.TraceContext{}})
	project := createProject(t, environment, "trace-gateway")
	ctx, root := tracer.Start(context.Background(), "caller")
	defer root.End()
	response := postTraced(t, environment, ctx, "/api/v1/projects/"+project.ID+"/access-hosts", `{"hostname":"trace.example.test","tlsMode":"http_only"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("host status = %d", response.StatusCode)
	}
	db := openTestDatabase(t, environment.databaseURL)
	events := internalevent.New(db)
	ref := loadEvent(t, db, "project_gateway.reconcile.v1", uuid.MustParse(project.ID))
	metadata, found, err := events.ReadForConsumption(context.Background(), ref)
	if err != nil || !found || metadata.TraceParent == "" {
		t.Fatal("gateway command did not persist trace in its outbox transaction")
	}
	forged := ref
	forged.AggregateID = uuid.New()
	if _, found, err := events.ReadForConsumption(context.Background(), forged); err != nil || found {
		t.Fatal("metadata reader accepted forged identity")
	}
	_, address := testsupport.StartRedis(t)
	received := make(chan trace.SpanContext, 1)
	service, err := internalevent.NewService(internalevent.Config{RedisAddress: address, Queue: "trace-gateway", Topics: []string{ref.Topic}, Concurrency: 1,
		PollInterval: 20 * time.Millisecond, ConsumptionGrace: time.Second, TaskTimeout: time.Minute, ShutdownTimeout: time.Second,
		Tracer: tracer, Propagator: propagation.TraceContext{}}, events, eventExecutorFunc(func(ctx context.Context, _ internalevent.Ref) error {
		received <- trace.SpanContextFromContext(ctx)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	startInternalEvents(t, service)
	select {
	case parent := <-received:
		if parent.TraceID() != root.SpanContext().TraceID() {
			t.Fatal("event lost API lineage")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not consumed")
	}
}

type eventExecutorFunc func(context.Context, internalevent.Ref) error

func (f eventExecutorFunc) HandleEvent(ctx context.Context, ref internalevent.Ref) error {
	return f(ctx, ref)
}

func postTraced(t *testing.T, environment *testEnvironment, ctx context.Context, path, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, environment.server.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", integrationOrigin)
	request.Header.Set("X-Orbit-CSRF", "1")
	request.Header.Set("Idempotency-Key", uuid.NewString())
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, err := environment.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
