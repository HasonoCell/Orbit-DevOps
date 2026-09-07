package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
)

// 用户受理的发布必须经过真实 PostgreSQL、Redis 和消费者得到业务终态。
func TestQueueDeliversAcceptedRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	accepted := createRelease(t, environment, "asynq-release")
	db := openTestDatabase(t, environment.databaseURL)
	operations := operation.New(db)
	runner, err := worker.New(worker.Config{WorkerID: "queue-test", LeaseDuration: time.Second, OperationTimeout: time.Second},
		operations, delivery.New(db, operations, projectauth.New(db)), &recordingPublisher{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := dispatch.New(dispatch.Config{RedisAddress: address, PollInterval: 20 * time.Millisecond, RepairInterval: 50 * time.Millisecond, ConsumptionGrace: time.Second, TaskTimeout: 3 * time.Second, ShutdownTimeout: time.Second, Concurrency: 2}, operations, runner)
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
				t.Errorf("queue exit: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("queue did not stop")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		current, err := operations.Get(ctx, mustOperationID(t, accepted.Operation.ID))
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == operation.StatusSucceeded {
			if current.AttemptCount != 1 {
				t.Fatalf("attempts = %d", current.AttemptCount)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("queue did not complete accepted release")
}
