package integration_test

import (
	"context"
	"math"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"errors"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
)

// PostgreSQL 失联时绝不无租约执行；积压查询不可达不能伪装成零，恢复后继续消费。
func TestQueueWaitsForDatabaseRecovery(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	accepted := createRelease(t, environment, "database-offline")
	var calls atomic.Int32
	service, operations := newQueueWithPublisher(t, environment, address, publisherFunc(func(context.Context, worker.PublishRequest) error { calls.Add(1); return nil }))
	if err := service.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	resume := pauseOwnedPostgres(t, environment)
	stop := startQueueTest(t, service)
	// defer 先恢复数据库，再执行测试 cleanup 中的 Worker 退出。
	defer resume()
	registry := prometheus.NewRegistry()
	registry.MustRegister(service)
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	nanFound := false
	for _, metric := range metrics {
		if metric.GetName() == "orbitops_dispatch_pending" {
			nanFound = math.IsNaN(metric.Metric[0].Gauge.GetValue())
		}
	}
	if !nanFound || calls.Load() != 0 {
		t.Fatalf("database outage reported empty or executed: NaN=%t calls=%d", nanFound, calls.Load())
	}
	resume()
	current := awaitQueuedStatus(t, operations, accepted.Operation.ID, operation.StatusSucceeded)
	stop()
	if current.AttemptCount != 1 || current.AutomaticRetryCount != 0 || calls.Load() != 1 {
		t.Fatalf("database recovery spent business budget: %+v calls=%d", current, calls.Load())
	}
}

// 用真实 Asynq 的五次重投直至归档验证两层预算；随后仅推进补偿时钟恢复原意图。
func TestQueueArchivedInfrastructureFailuresRemainRecoverable(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	accepted := createRelease(t, environment, "archived-infrastructure")
	db := openTestDatabase(t, environment.databaseURL)
	var offset atomic.Int64
	operations := operation.New(db, operation.WithClock(func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }))
	runner, err := worker.New(worker.Config{WorkerID: "archive-recovery", LeaseDuration: time.Second, OperationTimeout: 5 * time.Second}, operations, delivery.New(db, operations, projectauth.New(db)), publisherFunc(func(context.Context, worker.PublishRequest) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	config := queueConfig(address)
	config.ConsumptionGrace = time.Hour
	service, err := dispatch.New(config, operations, executorFunc(func(ctx context.Context, ref operation.DispatchRef) (operation.ClaimOutcome, error) {
		if !available.Load() {
			return "", errors.New("injected infrastructure unavailable")
		}
		return runner.RunDispatch(ctx, ref)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer inspector.Close()
	pending, err := inspector.ListPendingTasks(config.Queue)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending: %d %v", len(pending), err)
	}
	startQueueTest(t, service)
	awaitQueueCondition(t, 75*time.Second, func() bool {
		task, err := inspector.GetTaskInfo(config.Queue, pending[0].ID)
		return err == nil && task.State == asynq.TaskStateArchived && task.Retried == 5 && task.LastErr == "dispatch_execution_interrupted"
	})
	current, err := operations.Get(context.Background(), uuid.MustParse(accepted.Operation.ID))
	if err != nil || current.Status != operation.StatusPending || current.AttemptCount != 0 || current.AutomaticRetryCount != 0 {
		t.Fatalf("archival changed business state: %+v %v", current, err)
	}
	available.Store(true)
	offset.Store(int64(2 * time.Hour))
	current = awaitQueuedStatus(t, operations, accepted.Operation.ID, operation.StatusSucceeded)
	if current.AttemptCount != 1 || current.AutomaticRetryCount != 0 {
		t.Fatalf("archival recovery: %+v", current)
	}
}

// 暂停该 fixture 自己创建的容器，保留网络地址以模拟无响应数据库；不操作共享服务。
func pauseOwnedPostgres(t *testing.T, environment *testEnvironment) func() {
	t.Helper()
	id := environment.postgresContainerID
	if len(id) != 64 {
		t.Fatal("missing owned PostgreSQL container ID")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := exec.CommandContext(ctx, "docker", "pause", id).Run()
	cancel()
	if err != nil {
		t.Fatalf("pause owned PostgreSQL: %v", err)
	}
	var resumed atomic.Bool
	resume := func() {
		if resumed.CompareAndSwap(false, true) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := exec.CommandContext(ctx, "docker", "unpause", id).Run(); err != nil {
				t.Errorf("resume owned PostgreSQL: %v", err)
			}
		}
	}
	t.Cleanup(resume)
	return resume
}

// 续期连接失联必须在当前业务租约内取消外部调用，不能等 Asynq 的长任务超时。
func TestQueueDatabaseOutageStopsActiveExecutionBeforeLeaseExpiry(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	publisher := &cancelAwareRecoveryPublisher{entered: make(chan struct{}), stopped: make(chan struct{})}
	service, operations := newQueueWithPublisher(t, environment, address, publisher)
	accepted := createRelease(t, environment, "lease-database-outage")
	startQueueTest(t, service)
	select {
	case <-publisher.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher not entered")
	}
	resume := pauseOwnedPostgres(t, environment)
	defer resume()
	select {
	case <-publisher.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("database renewal outage did not stop external execution")
	}
	resume()
	current := awaitQueuedStatus(t, operations, accepted.Operation.ID, operation.StatusSucceeded)
	if current.AttemptCount != 2 || current.Attempts[0].Status != operation.AttemptOutcomeUnknown {
		t.Fatalf("active outage history: %+v", current)
	}
}

type cancelAwareRecoveryPublisher struct{ entered, stopped chan struct{} }

func (p *cancelAwareRecoveryPublisher) Publish(ctx context.Context, _ worker.PublishRequest) error {
	close(p.entered)
	<-ctx.Done()
	close(p.stopped)
	return ctx.Err()
}
func (p *cancelAwareRecoveryPublisher) InspectRecovery(context.Context, worker.PublishRequest) (worker.RecoveryObservation, error) {
	return worker.RecoveryObservation{Action: worker.RecoverySucceeded}, nil
}
func (p *cancelAwareRecoveryPublisher) ObserveRecovery(context.Context, worker.PublishRequest) error {
	return nil
}
