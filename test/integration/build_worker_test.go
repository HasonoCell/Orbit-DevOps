package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/builddispatch"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
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
		RedisAddress: address, Queue: "orbit-devops-build-worker-test", Concurrency: 2,
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

// 接管 Worker 必须先观察已经记录的旧 Job，不能因新 Attempt 再次启动外部构建。
func TestBuildWorkerRecoveryObservesExistingExecutorBeforeStarting(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-worker-recovery-project")
	application := createApplication(t, environment, project.ID, "build-worker-recovery-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-worker-recovery",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("c", 40)+`"}`)
	defer response.Body.Close()
	var acceptance buildAcceptanceDocument
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
		t.Fatalf("create recovery build status = %d", response.StatusCode)
	}

	database := openTestDatabase(t, environment.databaseURL)
	now := acceptance.Build.CreatedAt.Add(time.Second)
	operations := buildoperation.New(database, buildoperation.WithClock(func() time.Time { return now }))
	first := claimBuildDispatch(t, operations, "build-worker-lost")
	oldIdentity := buildworker.ExecutionIdentity{Name: "orbit-devops-build-" + first.BuildAttemptID.String(), UID: "existing-build-uid"}
	if err := operations.RecordExecutorIdentity(context.Background(), first, buildoperation.ExecutorIdentity{
		Name: oldIdentity.Name, UID: oldIdentity.UID,
	}); err != nil {
		t.Fatal(err)
	}
	now = first.ExpiresAt.Add(time.Second)
	if repaired, err := operations.RepairDispatches(context.Background(), 10); err != nil || repaired != 1 {
		t.Fatalf("repair expired execution = %d, %v", repaired, err)
	}
	items, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("reserve recovery dispatch = %d, %v", len(items), err)
	}
	if err := operations.ConfirmDispatch(context.Background(), items[0], "", time.Second); err != nil {
		t.Fatal(err)
	}

	executor := &memoryBuildExecutor{
		repository: acceptance.Build.DestinationRepository, digest: "sha256:" + strings.Repeat("d", 64),
		started: map[string]bool{oldIdentity.Name: true},
	}
	runner, err := buildworker.New(buildworker.Config{WorkerID: "build-worker-recovery", LeaseDuration: time.Minute,
		BuildTimeout: time.Minute, PollInterval: time.Millisecond}, operations,
		build.New(database, build.Config{}, operations, projectauth.New(database)), executor)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunDispatch(context.Background(), items[0].DispatchRef)
	if err != nil || outcome != buildoperation.ClaimOutcomeClaimed {
		t.Fatalf("run recovery dispatch = %s, %v", outcome, err)
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
	if err != nil || current.Status != buildoperation.StatusSucceeded || executor.startCount != 0 || len(current.Attempts) != 2 {
		t.Fatalf("recovered build = operation %#v, starts %d, error %v", current, executor.startCount, err)
	}
	if current.Attempts[1].ExecutorUID == nil || *current.Attempts[1].ExecutorUID != oldIdentity.UID {
		t.Fatalf("recovery attempt did not preserve executor identity: %#v", current.Attempts[1])
	}
}

// 已请求取消的恢复任务只能收束、取消已核验 Job 或进入人工处理，任何分支都不能启动新的外部构建。
func TestBuildWorkerCancelRecoveryNeverStartsNewExecutor(t *testing.T) {
	tests := []struct {
		name            string
		mode            string
		wantStatus      buildoperation.Status
		wantCancelCount int
	}{
		{name: "missing job confirms cancellation", mode: "missing", wantStatus: buildoperation.StatusCanceled},
		{name: "owned job is canceled", mode: "owned", wantStatus: buildoperation.StatusCanceled, wantCancelCount: 1},
		{name: "ownership conflict requires attention", mode: "conflict", wantStatus: buildoperation.StatusAttentionRequired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			project := createProject(t, environment, "build-worker-cancel-recovery-project")
			application := createApplication(t, environment, project.ID, "build-worker-cancel-recovery-application")
			response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-worker-cancel-recovery",
				`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("e", 40)+`"}`)
			defer response.Body.Close()
			var acceptance buildAcceptanceDocument
			if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
				t.Fatalf("create cancel recovery build status = %d", response.StatusCode)
			}

			database := openTestDatabase(t, environment.databaseURL)
			now := acceptance.Build.CreatedAt.Add(time.Second)
			operations := buildoperation.New(database,
				buildoperation.WithClock(func() time.Time { return now }),
				buildoperation.WithAuthorizer(projectauth.New(database)))
			first := claimBuildDispatch(t, operations, "build-worker-cancel-lost")
			operationID := uuid.MustParse(acceptance.BuildOperation.ID)
			if _, err := operations.Cancel(context.Background(), buildoperation.CancelCommand{
				BuildOperationID: operationID, ActorID: "local-developer", IdempotencyKey: "cancel-recovery-request",
			}); err != nil {
				t.Fatal(err)
			}
			now = first.ExpiresAt.Add(time.Second)
			if repaired, err := operations.RepairDispatches(context.Background(), 10); err != nil || repaired != 1 {
				t.Fatalf("repair canceled execution = %d, %v", repaired, err)
			}
			items, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
			if err != nil || len(items) != 1 {
				t.Fatalf("reserve canceled recovery dispatch = %d, %v", len(items), err)
			}
			if err := operations.ConfirmDispatch(context.Background(), items[0], "", time.Second); err != nil {
				t.Fatal(err)
			}

			executor := &memoryBuildExecutor{}
			oldName := "orbit-devops-build-" + first.BuildAttemptID.String()
			switch test.mode {
			case "owned":
				executor.started = map[string]bool{oldName: true}
			case "conflict":
				executor.observation = &buildworker.ExecutionObservation{Phase: buildworker.PhaseUnknown,
					ErrorCode: "build_job_ownership_conflict", ErrorSummary: "build job ownership does not match"}
			}
			runner, err := buildworker.New(buildworker.Config{WorkerID: "build-worker-cancel-recovery", LeaseDuration: time.Minute,
				BuildTimeout: time.Minute, PollInterval: time.Millisecond}, operations,
				build.New(database, build.Config{}, operations, projectauth.New(database)), executor)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := runner.RunDispatch(context.Background(), items[0].DispatchRef)
			if err != nil || outcome != buildoperation.ClaimOutcomeClaimed {
				t.Fatalf("run canceled recovery dispatch = %s, %v", outcome, err)
			}
			current, err := operations.Get(context.Background(), operationID)
			if err != nil || current.Status != test.wantStatus || executor.startCount != 0 ||
				executor.cancelCount != test.wantCancelCount || len(current.Attempts) != 2 {
				t.Fatalf("cancel recovery = operation %#v, starts %d, cancels %d, error %v",
					current, executor.startCount, executor.cancelCount, err)
			}
		})
	}
}

// BuildOperation 元数据对 viewer 可见，但日志摘录需要 develop 权限且不内嵌在普通详情中。
func TestBuildAttemptLogsRequireDevelopPermission(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-log-project")
	application := createApplication(t, environment, project.ID, "build-log-application")
	addMember(t, environment.server, project.ID, "build-log-viewer", "viewer", "add-build-log-viewer")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-log",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("e", 40)+`"}`)
	defer response.Body.Close()
	var acceptance buildAcceptanceDocument
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
		t.Fatalf("create log build status = %d", response.StatusCode)
	}
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL))
	lease := claimBuildDispatch(t, operations, "build-log-worker")
	const sensitiveExcerpt = "build-secret-marker"
	if _, err := operations.Fail(context.Background(), lease, buildoperation.Failure{
		Code: "dockerfile_build_failed", Summary: "build failed", Disposition: buildoperation.NonRetryable,
		LogExcerpt: sensitiveExcerpt,
	}); err != nil {
		t.Fatal(err)
	}

	viewerServer := environment.serverForActor(t, "build-log-viewer")
	operationResponse := requestJSON(t, viewerServer, http.MethodGet,
		"/api/v1/build-operations/"+acceptance.BuildOperation.ID, "", "")
	operationBody, err := io.ReadAll(operationResponse.Body)
	operationResponse.Body.Close()
	if err != nil || operationResponse.StatusCode != http.StatusOK || strings.Contains(string(operationBody), sensitiveExcerpt) {
		t.Fatalf("viewer operation status=%d body=%s error=%v", operationResponse.StatusCode, operationBody, err)
	}

	viewerLog := requestJSON(t, viewerServer, http.MethodGet,
		"/api/v1/build-attempts/"+lease.BuildAttemptID.String()+"/log", "", "")
	defer viewerLog.Body.Close()
	assertError(t, viewerLog, http.StatusForbidden, "project_permission_denied")
	ownerLog := requestJSON(t, environment.server, http.MethodGet,
		"/api/v1/build-attempts/"+lease.BuildAttemptID.String()+"/log", "", "")
	defer ownerLog.Body.Close()
	if ownerLog.StatusCode != http.StatusOK {
		t.Fatalf("owner log status = %d", ownerLog.StatusCode)
	}
	var document struct {
		Excerpt string `json:"excerpt"`
	}
	if json.NewDecoder(ownerLog.Body).Decode(&document) != nil || document.Excerpt != sensitiveExcerpt {
		t.Fatalf("owner build log = %#v", document)
	}
}

type memoryBuildExecutor struct {
	mu          sync.Mutex
	repository  string
	digest      string
	started     map[string]bool
	startCount  int
	cancelCount int
	observation *buildworker.ExecutionObservation
}

func (e *memoryBuildExecutor) Start(_ context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started == nil {
		e.started = map[string]bool{}
	}
	name := "memory-build-" + execution.BuildAttemptID.String()
	e.started[name] = true
	e.startCount++
	return buildworker.ExecutionIdentity{Name: name, UID: "uid-" + execution.BuildAttemptID.String()}, nil
}

func (e *memoryBuildExecutor) Observe(_ context.Context, _ buildworker.BuildExecution, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.observation != nil {
		observation := *e.observation
		observation.Identity = identity
		return observation, nil
	}
	if !e.started[identity.Name] {
		return buildworker.ExecutionObservation{Phase: buildworker.PhaseMissing}, nil
	}
	if identity.UID == "" {
		identity.UID = "uid-" + identity.Name
	}
	return buildworker.ExecutionObservation{
		Phase: buildworker.PhaseSucceeded, Identity: identity,
		Repository: e.repository, Digest: e.digest, LogExcerpt: "memory executor completed",
	}, nil
}

func (e *memoryBuildExecutor) Cancel(_ context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	e.mu.Lock()
	e.cancelCount++
	e.mu.Unlock()
	return buildworker.ExecutionObservation{Phase: buildworker.PhaseCanceled, Identity: identity}, nil
}
