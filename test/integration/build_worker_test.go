package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/builddispatch"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
)

// 内存 Executor 验证完整的队列、Runner、Lease、Attempt 与 Artifact 收束，不依赖 Kubernetes。
func TestBuildWorkerCompletesAcceptedBuildThroughQueue(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-worker-project")
	application := createApplication(t, environment, project.ID, "build-worker-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-worker",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("a", 40)+`"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create build status = %d", response.StatusCode)
	}
	var acceptance buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatal(err)
	}

	database := openTestDatabase(t, environment.databaseURL)
	operations := buildoperation.New(database)
	builds := build.New(database, build.Config{}, operations, projectauth.New(database))
	executor := &memoryBuildExecutor{
		repository: acceptance.Build.DestinationRepository,
		digest:     "sha256:" + strings.Repeat("b", 64),
	}
	runner, err := buildworker.New(buildworker.Config{
		WorkerID: "build-worker-test", LeaseDuration: time.Second,
		BuildTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond,
	}, operations, builds, executor)
	if err != nil {
		t.Fatal(err)
	}
	_, address := testsupport.StartRedis(t)
	service, err := builddispatch.New(builddispatch.Config{
		RedisAddress: address, Queue: "orbitops-build-worker-test", Concurrency: 2,
		PollInterval: 20 * time.Millisecond, RepairInterval: 50 * time.Millisecond,
		ConsumptionGrace: time.Second, TaskTimeout: 10 * time.Second, ShutdownTimeout: time.Second,
	}, operations, runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("build service exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("build service did not stop")
		}
	})

	operationID := uuid.MustParse(acceptance.BuildOperation.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := operations.Get(ctx, operationID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == buildoperation.StatusSucceeded {
			var count int
			if err := database.GetContext(ctx, &count, `SELECT count(*) FROM image_artifacts WHERE build_id = $1`, acceptance.Build.ID); err != nil {
				t.Fatal(err)
			}
			if count != 1 || len(current.Attempts) != 1 || current.Attempts[0].ExecutorUID == nil {
				t.Fatalf("completed build = count %d, operation %#v", count, current)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("build worker did not complete accepted build")
}

type memoryBuildExecutor struct {
	mu         sync.Mutex
	repository string
	digest     string
	started    map[string]bool
}

func (e *memoryBuildExecutor) Start(_ context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started == nil {
		e.started = map[string]bool{}
	}
	name := "memory-build-" + execution.BuildAttemptID.String()
	e.started[name] = true
	return buildworker.ExecutionIdentity{Name: name, UID: "uid-" + execution.BuildAttemptID.String()}, nil
}

func (e *memoryBuildExecutor) Observe(_ context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started[identity.Name] {
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseMissing}, nil
	}
	return buildworker.ExecutionObservation{
		Phase: buildworker.PhaseSucceeded, Identity: identity,
		Repository: e.repository, Digest: e.digest, LogExcerpt: "memory executor completed",
	}, nil
}

func (e *memoryBuildExecutor) Cancel(_ context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	return buildworker.ExecutionObservation{Phase: buildworker.PhaseCanceled, Identity: identity}, nil
}
