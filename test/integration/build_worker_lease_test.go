package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
)

// 慢外部调用持续三个租约窗口；仍存活的 Runner 应保持执行权，而不是制造恢复 Attempt。
func TestBuildWorkerRenewsLeaseDuringSlowExecutorCalls(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "slow-build-project")
	application := createApplication(t, environment, project.ID, "slow-build-application")
	db := openTestDatabase(t, environment.databaseURL)
	// 有限池下，外部 Job 调用不能占住事务连接；独立续租与修复扫描仍须取得连接。
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	operations := buildoperation.New(db, buildoperation.WithAuthorizer(projectauth.New(db, environment.identities)))
	for _, phase := range []string{"start", "observe", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "slow-build-"+phase,
				`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("a", 40)+`"}`)
			defer response.Body.Close()
			var acceptance buildAcceptanceDocument
			if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
				t.Fatalf("accept build status = %d", response.StatusCode)
			}
			items, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
			if err != nil || len(items) != 1 {
				t.Fatalf("reserve dispatch = %d, %v", len(items), err)
			}
			if err := operations.ConfirmDispatch(context.Background(), items[0], "", time.Second); err != nil {
				t.Fatal(err)
			}
			executor := &slowBuildExecutor{memoryBuildExecutor: memoryBuildExecutor{
				repository: acceptance.Build.DestinationRepository, digest: "sha256:" + strings.Repeat("b", 64),
			}, phase: phase, delay: 3 * time.Second}
			executor.afterWait = func() error {
				// 模拟另一 Worker 的修复扫描：长调用仍在进行时，不应发布接管任务。
				count, err := operations.RepairDispatches(context.Background(), 10)
				if err != nil {
					return err
				}
				if count != 0 {
					return errors.New("live worker was incorrectly reclaimed")
				}
				return nil
			}
			if phase == "cancel" {
				executor.afterStart = func() error {
					_, err := operations.Cancel(context.Background(), buildoperation.CancelCommand{
						BuildOperationID: uuid.MustParse(acceptance.BuildOperation.ID), Caller: environment.adminCaller,
						IdempotencyKey: "cancel-slow-build",
					})
					return err
				}
			}
			runner, err := buildworker.New(buildworker.Config{WorkerID: "slow-build-worker", LeaseDuration: time.Second,
				BuildTimeout: 10 * time.Second, PollInterval: 10 * time.Millisecond}, operations,
				build.New(db, build.Config{}, operations, projectauth.New(db, nil)), executor)
			if err != nil {
				t.Fatal(err)
			}
			if outcome, err := runner.RunDispatch(context.Background(), items[0].DispatchRef); err != nil || outcome != buildoperation.ClaimOutcomeClaimed {
				t.Fatalf("live worker lost execution during slow %s: outcome=%s error=%v", phase, outcome, err)
			}
			current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
			want := buildoperation.StatusSucceeded
			if phase == "cancel" {
				want = buildoperation.StatusCanceled
			}
			if err != nil || current.Status != want || len(current.Attempts) != 1 || current.Attempts[0].ExecutorUID == nil {
				t.Fatalf("slow %s result: status=%s attempts=%d error=%v", phase, current.Status, len(current.Attempts), err)
			}
		})
	}
}

// 取消、退出和续租失权都必须打断阻塞的 Observe；失权者不提交终态，新 Worker 只观察旧 Job。
func TestBuildWorkerInterruptsBlockedObserve(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "interrupt-build-project")
	application := createApplication(t, environment, project.ID, "interrupt-build-application")
	db := openTestDatabase(t, environment.databaseURL)
	for _, reason := range []string{"cancel", "shutdown", "lease-lost"} {
		t.Run(reason, func(t *testing.T) {
			var offset atomic.Int64
			operations := buildoperation.New(db, buildoperation.WithClock(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }),
				buildoperation.WithAuthorizer(projectauth.New(db, environment.identities)))
			response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "interrupt-"+reason,
				`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("c", 40)+`"}`)
			defer response.Body.Close()
			var acceptance buildAcceptanceDocument
			if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
				t.Fatalf("accept build status = %d", response.StatusCode)
			}
			ref := reserveBuildWorkerDispatch(t, operations)
			executor := &blockedBuildObserver{memoryBuildExecutor: memoryBuildExecutor{
				repository: acceptance.Build.DestinationRepository, digest: "sha256:" + strings.Repeat("d", 64),
			}, entered: make(chan struct{})}
			runner, err := buildworker.New(buildworker.Config{WorkerID: "interrupted-worker", LeaseDuration: time.Second,
				BuildTimeout: 10 * time.Second, PollInterval: time.Millisecond}, operations,
				build.New(db, build.Config{}, operations, projectauth.New(db, nil)), executor)
			if err != nil {
				t.Fatal(err)
			}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			done := make(chan error, 1)
			go func() { _, err := runner.RunDispatch(ctx, ref); done <- err }()
			select {
			case <-executor.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not enter Observe")
			}
			operationID := uuid.MustParse(acceptance.BuildOperation.ID)
			switch reason {
			case "cancel":
				if _, err := operations.Cancel(ctx, buildoperation.CancelCommand{BuildOperationID: operationID,
					Caller: environment.adminCaller, IdempotencyKey: "interrupt-cancel"}); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				stop()
			case "lease-lost":
				// 时钟推进只用于确定性触发真实数据库租约围栏，不修改业务表制造测试状态。
				offset.Store(int64(time.Minute))
			}
			select {
			case err := <-done:
				if reason == "cancel" && err != nil || reason == "shutdown" && !errors.Is(err, context.Canceled) ||
					reason == "lease-lost" && !errors.Is(err, buildoperation.ErrLeaseLost) {
					t.Fatalf("interruption %s returned %v", reason, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("blocked Observe or heartbeat leaked after interruption")
			}
			current, err := operations.Get(context.Background(), operationID)
			if err != nil {
				t.Fatal(err)
			}
			if reason == "cancel" {
				if current.Status != buildoperation.StatusCanceled || executor.cancelCount != 1 {
					t.Fatalf("cancel result = %s, deletes=%d", current.Status, executor.cancelCount)
				}
				return
			}
			if current.Status != buildoperation.StatusRunning || len(current.Attempts) != 1 {
				t.Fatalf("old worker submitted a terminal result: status=%s attempts=%d", current.Status, len(current.Attempts))
			}
			offset.Store(int64(2 * time.Minute))
			if count, err := operations.RepairDispatches(context.Background(), 10); err != nil || count != 1 {
				t.Fatalf("repair abandoned execution = %d, %v", count, err)
			}
			recovery, err := buildworker.New(buildworker.Config{WorkerID: "recovery-worker", LeaseDuration: time.Second,
				BuildTimeout: 5 * time.Second, PollInterval: time.Millisecond}, operations,
				build.New(db, build.Config{}, operations, projectauth.New(db, nil)), executor)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recovery.RunDispatch(context.Background(), reserveBuildWorkerDispatch(t, operations)); err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			current, err = operations.Get(context.Background(), operationID)
			if err != nil || current.Status != buildoperation.StatusSucceeded || len(current.Attempts) != 2 || executor.startCount != 1 {
				t.Fatalf("recovery: status=%s attempts=%d starts=%d error=%v", current.Status, len(current.Attempts), executor.startCount, err)
			}
		})
	}
}

func reserveBuildWorkerDispatch(t *testing.T, operations *buildoperation.Module) buildoperation.DispatchRef {
	t.Helper()
	items, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("reserve build dispatch = %d, %v", len(items), err)
	}
	if err := operations.ConfirmDispatch(context.Background(), items[0], "", time.Second); err != nil {
		t.Fatal(err)
	}
	return items[0].DispatchRef
}

// 恢复中旧 Job 缺失，新 Job 已创建但 Start 响应丢失；取消必须指向本次新 Job，而非旧 Attempt。
func TestBuildWorkerCancelsRestartedJobWhenStartResponseLost(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "restart-cancel-project")
	application := createApplication(t, environment, project.ID, "restart-cancel-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "restart-cancel",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("e", 40)+`"}`)
	defer response.Body.Close()
	var acceptance buildAcceptanceDocument
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
		t.Fatalf("accept build status = %d", response.StatusCode)
	}
	db := openTestDatabase(t, environment.databaseURL)
	var offset atomic.Int64
	operations := buildoperation.New(db, buildoperation.WithClock(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }),
		buildoperation.WithAuthorizer(projectauth.New(db, environment.identities)))
	first, err := operations.ClaimDispatch(context.Background(), reserveBuildWorkerDispatch(t, operations),
		buildoperation.ClaimRequest{WorkerID: "lost-before-start", LeaseDuration: time.Second})
	if err != nil || first.Outcome != buildoperation.ClaimOutcomeClaimed {
		t.Fatalf("first claim = %s, %v", first.Outcome, err)
	}
	offset.Store(int64(time.Minute))
	if count, err := operations.RepairDispatches(context.Background(), 10); err != nil || count != 1 {
		t.Fatalf("repair old claim = %d, %v", count, err)
	}
	executor := &lostStartResponseExecutor{entered: make(chan struct{})}
	runner, err := buildworker.New(buildworker.Config{WorkerID: "restart-worker", LeaseDuration: time.Second,
		BuildTimeout: 5 * time.Second, PollInterval: time.Millisecond}, operations,
		build.New(db, build.Config{}, operations, projectauth.New(db, nil)), executor)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	ref := reserveBuildWorkerDispatch(t, operations)
	go func() { _, err := runner.RunDispatch(ctx, ref); done <- err }()
	select {
	case <-executor.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("recovery did not restart the missing Job")
	}
	if _, err := operations.Cancel(ctx, buildoperation.CancelCommand{BuildOperationID: uuid.MustParse(acceptance.BuildOperation.ID),
		Caller: environment.adminCaller, IdempotencyKey: "cancel-restarted-job"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restarted Job was not canceled promptly")
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
	if err != nil || current.Status != buildoperation.StatusCanceled || executor.cancelCount != 1 || executor.canceled != executor.created {
		t.Fatalf("restart cancellation: status=%s deletes=%d correctJob=%t error=%v", current.Status,
			executor.cancelCount, executor.canceled == executor.created, err)
	}
}

type lostStartResponseExecutor struct {
	memoryBuildExecutor
	entered  chan struct{}
	created  buildworker.ExecutionIdentity
	canceled buildworker.ExecutionIdentity
}

func (e *lostStartResponseExecutor) Start(ctx context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	name := "orbit-devops-build-" + execution.BuildAttemptID.String()
	e.created = buildworker.ExecutionIdentity{Name: name, UID: "uid-" + name}
	e.started = map[string]bool{name: true}
	e.startCount++
	close(e.entered)
	<-ctx.Done()
	return buildworker.ExecutionIdentity{}, ctx.Err()
}

func (e *lostStartResponseExecutor) Cancel(ctx context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	e.canceled = identity
	return e.memoryBuildExecutor.Cancel(ctx, identity)
}

type blockedBuildObserver struct {
	memoryBuildExecutor
	entered chan struct{}
	blocked atomic.Bool
}

func (e *blockedBuildObserver) Observe(ctx context.Context, execution buildworker.BuildExecution, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	if e.blocked.CompareAndSwap(false, true) {
		close(e.entered)
		<-ctx.Done()
		return buildworker.ExecutionObservation{}, ctx.Err()
	}
	return e.memoryBuildExecutor.Observe(ctx, execution, identity)
}

type slowBuildExecutor struct {
	memoryBuildExecutor
	phase      string
	delay      time.Duration
	afterStart func() error
	afterWait  func() error
}

func (e *slowBuildExecutor) wait(ctx context.Context, phase string) error {
	if e.phase != phase {
		return nil
	}
	timer := time.NewTimer(e.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if e.afterWait != nil {
			return e.afterWait()
		}
		return nil
	}
}

func (e *slowBuildExecutor) Start(ctx context.Context, execution buildworker.BuildExecution) (buildworker.ExecutionIdentity, error) {
	if err := e.wait(ctx, "start"); err != nil {
		return buildworker.ExecutionIdentity{}, err
	}
	identity, err := e.memoryBuildExecutor.Start(ctx, execution)
	if err == nil && e.afterStart != nil {
		err = e.afterStart()
	}
	return identity, err
}

func (e *slowBuildExecutor) Observe(ctx context.Context, execution buildworker.BuildExecution, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	if err := e.wait(ctx, "observe"); err != nil {
		return buildworker.ExecutionObservation{}, err
	}
	return e.memoryBuildExecutor.Observe(ctx, execution, identity)
}

func (e *slowBuildExecutor) Cancel(ctx context.Context, identity buildworker.ExecutionIdentity) (buildworker.ExecutionObservation, error) {
	if err := e.wait(ctx, "cancel"); err != nil {
		return buildworker.ExecutionObservation{}, err
	}
	return e.memoryBuildExecutor.Cancel(ctx, identity)
}
