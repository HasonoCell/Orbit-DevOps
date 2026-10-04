package taskqueue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/taskqueue"
	"github.com/hibiken/asynq"
)

func TestTransportValidatesPolicyAndClosesIdempotently(t *testing.T) {
	config := taskqueue.Config{RedisAddress: "127.0.0.1:1", Queue: "test", Concurrency: 1,
		PollInterval: time.Second, TaskTimeout: time.Minute, ShutdownTimeout: time.Second, MaxRetry: 5}
	queue, err := taskqueue.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := queue.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error { return nil })); !errors.Is(err, taskqueue.ErrStopping) {
		t.Fatalf("closed transport start = %v", err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	config.Concurrency = 0
	if _, err := taskqueue.New(config); err == nil {
		t.Fatal("unbounded consumer accepted")
	}
	config.Concurrency = 1
	config.MaxRetry = -1
	if _, err := taskqueue.New(config); err == nil {
		t.Fatal("negative transport retry accepted")
	}
}
