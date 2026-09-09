package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/builddispatch"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// 队列运输可以重复，但只有成功取得数据库业务执行权才产生 BuildAttempt。
func TestBuildDispatchClaimCreatesExactlyOneAttempt(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-dispatch-project")
	application := createApplication(t, environment, project.ID, "build-dispatch-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-dispatch",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("b", 40)+`"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create build status = %d", response.StatusCode)
	}
	var acceptance buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode build acceptance: %v", err)
	}

	database, err := sqlx.Open("pgx", environment.databaseURL)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	operations := buildoperation.New(database)
	reserved, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
	if err != nil || len(reserved) != 1 {
		t.Fatalf("reserve dispatches = %d, %v", len(reserved), err)
	}
	if err := operations.ConfirmDispatch(context.Background(), reserved[0], "", time.Second); err != nil {
		t.Fatalf("confirm dispatch: %v", err)
	}

	claim, err := operations.ClaimDispatch(context.Background(), reserved[0].DispatchRef, buildoperation.ClaimRequest{
		WorkerID: "build-worker-one", LeaseDuration: time.Minute,
	})
	if err != nil || claim.Outcome != buildoperation.ClaimOutcomeClaimed {
		t.Fatalf("claim dispatch = %#v, %v", claim, err)
	}
	if claim.Lease.BuildID.String() != acceptance.Build.ID || claim.Lease.BuildAttemptNumber != 1 {
		t.Fatalf("build lease = %#v", claim.Lease)
	}

	duplicate, err := operations.ClaimDispatch(context.Background(), reserved[0].DispatchRef, buildoperation.ClaimRequest{
		WorkerID: "build-worker-two", LeaseDuration: time.Minute,
	})
	if err != nil || duplicate.Outcome != buildoperation.ClaimOutcomeIgnored {
		t.Fatalf("duplicate claim = %#v, %v", duplicate, err)
	}
	record, err := operations.Get(context.Background(), claim.Lease.BuildOperationID)
	if err != nil {
		t.Fatalf("get build operation: %v", err)
	}
	if record.Status != buildoperation.StatusRunning || record.AttemptCount != 1 || len(record.Attempts) != 1 {
		t.Fatalf("build operation after duplicate delivery = %#v", record)
	}
	if record.Attempts[0].ID != claim.Lease.BuildAttemptID || record.Attempts[0].Status != buildoperation.AttemptRunning {
		t.Fatalf("build attempt = %#v", record.Attempts[0])
	}
}

// 已受理的 Build 即使 API 不连接 Redis，也能由独立分发进程补发并取得业务执行权。
func TestBuildQueueDeliversAcceptedBuild(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-queue-project")
	application := createApplication(t, environment, project.ID, "build-queue-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-queue",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("c", 40)+`"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create build status = %d", response.StatusCode)
	}
	var acceptance buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode build acceptance: %v", err)
	}

	_, address := testsupport.StartRedis(t)
	database := openTestDatabase(t, environment.databaseURL)
	operations := buildoperation.New(database)
	executor := buildClaimExecutor{operations: operations}
	service, err := builddispatch.New(builddispatch.Config{
		RedisAddress: address, Queue: "orbitops-build-test", Concurrency: 2,
		PollInterval: 20 * time.Millisecond, RepairInterval: 50 * time.Millisecond,
		ConsumptionGrace: time.Second, TaskTimeout: time.Second, ShutdownTimeout: time.Second,
	}, operations, executor)
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
				t.Errorf("build queue exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("build queue did not stop")
		}
	})

	operationID, err := uuid.Parse(acceptance.BuildOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := operations.Get(ctx, operationID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == buildoperation.StatusRunning {
			if current.AttemptCount != 1 || len(current.Attempts) != 1 {
				t.Fatalf("attempts after queue delivery = %#v", current.Attempts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("build queue did not deliver accepted build")
}

type buildClaimExecutor struct{ operations *buildoperation.Module }

func (e buildClaimExecutor) RunDispatch(ctx context.Context, ref buildoperation.DispatchRef) (buildoperation.ClaimOutcome, error) {
	claim, err := e.operations.ClaimDispatch(ctx, ref, buildoperation.ClaimRequest{
		WorkerID: "build-queue-test", LeaseDuration: time.Minute,
	})
	return claim.Outcome, err
}
