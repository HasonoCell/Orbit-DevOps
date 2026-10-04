package integration_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/taskqueue"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/hibiken/asynq"
)

func TestTaskQueueCloseWaitsForCanceledHandler(t *testing.T) {
	_, address := testsupport.StartRedis(t)
	queue, err := taskqueue.New(taskqueue.Config{RedisAddress: address, Queue: "drain", Concurrency: 1,
		PollInterval: 20 * time.Millisecond, TaskTimeout: time.Minute, ShutdownTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if err := queue.Start(asynq.HandlerFunc(func(ctx context.Context, _ *asynq.Task) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release // 模拟外部调用在响应取消后仍需完成的清理。
		return errors.New("execution_interrupted")
	})); err != nil {
		t.Fatal(err)
	}
	if err := queue.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil })); err == nil {
		t.Fatal("second consumer start accepted")
	}
	if _, err := queue.Send(context.Background(), asynq.NewTask("test", nil), time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not start")
	}
	done := make(chan error, 1)
	go func() { done <- queue.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not cancel handler")
	}
	select {
	case <-done:
		close(release)
		t.Fatal("transport closed before handler exited")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish")
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskQueueRedactsPanicBeforeRedisArchival(t *testing.T) {
	_, address := testsupport.StartRedis(t)
	var panics atomic.Uint64
	queue, err := taskqueue.New(taskqueue.Config{RedisAddress: address, Queue: "panic", Concurrency: 1,
		PollInterval: 20 * time.Millisecond, TaskTimeout: time.Minute, ShutdownTimeout: time.Second,
		OnPanic: func() { panics.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	if err := queue.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { panic("secret-probe") })); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Send(context.Background(), asynq.NewTask("test", nil), time.Now()); err != nil {
		t.Fatal(err)
	}
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer inspector.Close()
	awaitQueueCondition(t, 5*time.Second, func() bool {
		tasks, err := inspector.ListArchivedTasks("panic")
		return err == nil && len(tasks) == 1
	})
	tasks, err := inspector.ListArchivedTasks("panic")
	if err != nil {
		t.Fatal(err)
	}
	if panics.Load() != 1 || strings.Contains(tasks[0].LastErr, "secret-probe") || tasks[0].LastErr != taskqueue.ErrHandlerInterrupted.Error() {
		t.Fatal("panic did not become a counted, safe transport failure")
	}
}
