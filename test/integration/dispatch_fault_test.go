package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releasedispatch"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/HasonoCell/OrbitOps/internal/releaseworker"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus"
)

func queueConfig(address string) releasedispatch.Config {
	return releasedispatch.Config{RedisAddress: address, Queue: "orbitops-release", PollInterval: 20 * time.Millisecond, RepairInterval: 50 * time.Millisecond,
		ConsumptionGrace: 250 * time.Millisecond, TaskTimeout: 10 * time.Second, ShutdownTimeout: time.Second, Concurrency: 4}
}

// startQueueTest 的清理先等所有执行者退出，再允许数据库/Redis fixture 关闭。
func startQueueTest(t *testing.T, service *releasedispatch.Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("queue exit: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("queue did not stop within deadline")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func awaitQueueCondition(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("queue condition not reached before deadline")
}

func awaitQueuedStatus(t *testing.T, operations *releaseoperation.Module, id string, status releaseoperation.ReleaseOperationStatus) releaseoperation.Record {
	t.Helper()
	var result releaseoperation.Record
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		result, err = operations.Get(context.Background(), uuid.MustParse(id))
		if err == nil && result.Status == status {
			return result
		}
		time.Sleep(20 * time.Millisecond)
	}
	stats, err := operations.ReadDispatchMetrics(context.Background())
	t.Fatalf("operation did not reach %s: state=%s attempts=%d dispatch=%+v error=%v", status, result.Status, result.AttemptCount, stats, err)
	return result
}

func newQueueWithPublisher(t *testing.T, environment *testEnvironment, address string, publisher releaseworker.Publisher) (*releasedispatch.Service, *releaseoperation.Module) {
	t.Helper()
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db, releaseoperation.WithAutomaticRetryPolicy(1, func(int) time.Duration { return 200 * time.Millisecond }))
	runner, err := releaseworker.New(releaseworker.Config{WorkerID: uuid.NewString(), LeaseDuration: 500 * time.Millisecond, ReleaseOperationTimeout: 5 * time.Second}, operations, delivery.New(db, operations, projectauth.New(db)), publisher)
	if err != nil {
		t.Fatal(err)
	}
	service, err := releasedispatch.New(queueConfig(address), operations, runner)
	if err != nil {
		t.Fatal(err)
	}
	return service, operations
}

// Redis 不参与受理事务；停机积压在重启后自动推进，就绪探针真实反映依赖中断。
func TestQueueSurvivesRedisOutageDuringAcceptance(t *testing.T) {
	environment := newTestEnvironment(t)
	container, address := testsupport.StartRedis(t)
	service, operations := newQueueWithPublisher(t, environment, address, &recordingPublisher{})
	startQueueTest(t, service)
	ctx := context.Background()
	awaitQueueCondition(t, 5*time.Second, func() bool { return service.Ready(ctx) == nil })
	if err := container.Stop(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if service.Ready(ctx) == nil {
		t.Fatal("Redis outage reported ready")
	}
	accepted := createRelease(t, environment, "redis-offline")
	awaitQueueCondition(t, 5*time.Second, func() bool {
		stats, err := operations.ReadDispatchMetrics(ctx)
		return err == nil && stats.Reservations > 0
	})
	current, err := operations.Get(ctx, uuid.MustParse(accepted.ReleaseOperation.ID))
	if err != nil || current.Status != releaseoperation.StatusPending || current.AttemptCount != 0 {
		t.Fatalf("offline acceptance: %+v %v", current, err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatal(err)
	}
	_, originalPort, _ := net.SplitHostPort(address)
	restartedPort, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil || restartedPort.Port() != originalPort {
		t.Fatalf("Redis restart changed fixture port: before=%s after=%s error=%v", originalPort, restartedPort, err)
	}
	current = awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	if current.AttemptCount != 1 || current.AutomaticRetryCount != 0 {
		t.Fatalf("transport spent business budget: %+v", current)
	}
	awaitQueueCondition(t, 5*time.Second, func() bool { return service.Ready(ctx) == nil })
}

// 仅删除该测试独占 Redis 中已确认的指定消息，证明补偿不依赖原消息仍然存在。
func TestQueueRepairsPublishedMessageLoss(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	accepted := createRelease(t, environment, "lost-queued-message")
	service, operations := newQueueWithPublisher(t, environment, address, &recordingPublisher{})
	if err := service.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer inspector.Close()
	tasks, err := inspector.ListPendingTasks("orbitops-release")
	if err != nil || len(tasks) != 1 || tasks[0].MaxRetry != 5 {
		t.Fatalf("published tasks: %d %v", len(tasks), err)
	}
	if err := inspector.DeleteTask("orbitops-release", tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	startQueueTest(t, service)
	current := awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	stats, err := operations.ReadDispatchMetrics(context.Background())
	if err != nil || stats.Redeliveries < 1 || current.AttemptCount != 1 || current.AutomaticRetryCount != 0 {
		t.Fatalf("lost message recovery: %+v %+v %v", current, stats, err)
	}
}

// 真实队列同时承担失败、自动重试、显式重试和回滚，业务失败不触发第二层预算。
func TestQueuePreservesBusinessRetryAndRollback(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	var calls atomic.Int32
	publisher := publisherFunc(func(context.Context, releaseworker.PublishRequest) error {
		if calls.Add(1) <= 2 {
			return releaseworker.NewRetryableFailure("kubernetes_unavailable", "测试瞬时不可用")
		}
		return nil
	})
	service, operations := newQueueWithPublisher(t, environment, address, publisher)
	accepted := createRelease(t, environment, "queue-retry")
	startQueueTest(t, service)
	failed := awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusFailed)
	if failed.AttemptCount != 2 || failed.AutomaticRetryCount != 1 {
		t.Fatalf("automatic budget: %+v", failed)
	}
	retry := environment.postJSON(t, "/api/v1/release-operations/"+accepted.ReleaseOperation.ID+"/retry", "queue-explicit-retry", "")
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("retry status: %d", retry.StatusCode)
	}
	current := awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	if current.AttemptCount != 3 || current.AutomaticRetryCount != 0 {
		t.Fatalf("explicit retry: %+v", current)
	}
	response := environment.postJSON(t, "/api/v1/releases/"+accepted.Release.ID+"/rollback", "queue-rollback", "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("rollback: %d", response.StatusCode)
	}
	var rolled struct {
		ReleaseOperation struct {
			ID string `json:"id"`
		} `json:"releaseOperation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&rolled); err != nil {
		t.Fatal(err)
	}
	if rolled.ReleaseOperation.ID == accepted.ReleaseOperation.ID {
		t.Fatal("rollback reused operation")
	}
	awaitQueuedStatus(t, operations, rolled.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
}

type publisherFunc func(context.Context, releaseworker.PublishRequest) error

func (f publisherFunc) Publish(ctx context.Context, request releaseworker.PublishRequest) error {
	return f(ctx, request)
}

// 外层退出取消不是用户取消；停止续期后保留 running，由新的恢复意图读后写接管。
func TestQueueShutdownLeavesRecoverableReleaseOperation(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	entered := make(chan struct{})
	service, operations := newQueueWithPublisher(t, environment, address, publisherFunc(func(ctx context.Context, _ releaseworker.PublishRequest) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}))
	accepted := createRelease(t, environment, "shutdown")
	stop := startQueueTest(t, service)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher not started")
	}
	stop()
	current, err := operations.Get(context.Background(), uuid.MustParse(accepted.ReleaseOperation.ID))
	if err != nil || current.Status != releaseoperation.StatusRunning || current.AttemptCount != 1 {
		t.Fatalf("shutdown faked cancellation: %+v %v", current, err)
	}
	recovering, _ := newQueueWithPublisher(t, environment, address, &recoveryRecordingPublisher{observation: releaseworker.RecoveryObservation{Action: releaseworker.RecoverySucceeded}})
	startQueueTest(t, recovering)
	current = awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	if current.AttemptCount != 2 || current.Attempts[0].Status != releaseoperation.AttemptOutcomeUnknown {
		t.Fatalf("shutdown recovery: %+v", current)
	}
}

// 坏消息不能隔离合法意图；基础设施错误用稳定摘要落入 Redis，不能泄露原错误。
func TestQueueRejectsMalformedMessagesAndRedactsErrors(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	accepted := createRelease(t, environment, "bad-message")
	operations := releaseoperation.New(openTestDatabase(t, environment.databaseURL))
	var ref releaseoperation.DispatchRef
	db := openTestDatabase(t, environment.databaseURL)
	if err := db.Get(&ref, `SELECT id,operation_id,sequence,protocol_version FROM operation_dispatches WHERE operation_id=$1`, accepted.ReleaseOperation.ID); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	config := queueConfig(address)
	config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service, err := releasedispatch.New(config, operations, executorFunc(func(context.Context, releaseoperation.DispatchRef) (releaseoperation.ClaimOutcome, error) {
		return "", errors.New("secret-probe-not-real-credential")
	}))
	if err != nil {
		t.Fatal(err)
	}
	client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
	defer client.Close()
	bad := ref
	bad.ProtocolVersion = 99
	payload, _ := json.Marshal(bad)
	if _, err := client.Enqueue(asynq.NewTask(releasedispatch.TaskType, payload), asynq.Queue(config.Queue), asynq.MaxRetry(0)); err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(ref)
	if _, err := client.Enqueue(asynq.NewTask(releasedispatch.TaskType, payload), asynq.Queue(config.Queue), asynq.TaskID("secret-probe-untrusted-task-id"), asynq.MaxRetry(0)); err != nil {
		t.Fatal(err)
	}
	stop := startQueueTest(t, service)
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer inspector.Close()
	awaitQueueCondition(t, 5*time.Second, func() bool {
		tasks, err := inspector.ListArchivedTasks(config.Queue)
		return err == nil && len(tasks) >= 2
	})
	tasks, err := inspector.ListArchivedTasks(config.Queue)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if strings.Contains(task.LastErr, "secret-probe") {
			t.Fatal("raw error leaked into queue")
		}
	}
	stats, err := operations.ReadDispatchMetrics(context.Background())
	if err != nil || stats.Quarantined != 0 {
		t.Fatalf("bad message quarantined valid intent: %+v %v", stats, err)
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(accepted.ReleaseOperation.ID))
	if err != nil || current.AttemptCount != 0 {
		t.Fatalf("transport error created attempt: %+v %v", current, err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(service)
	metrics, err := registry.Gather()
	if err != nil || len(metrics) < 10 {
		t.Fatalf("queue metrics: %d %v", len(metrics), err)
	}
	stop()
	if strings.Contains(logs.String(), "secret-probe") {
		t.Fatal("raw error leaked into logs")
	}
}

type executorFunc func(context.Context, releaseoperation.DispatchRef) (releaseoperation.ClaimOutcome, error)

func (f executorFunc) RunDispatch(ctx context.Context, ref releaseoperation.DispatchRef) (releaseoperation.ClaimOutcome, error) {
	return f(ctx, ref)
}
