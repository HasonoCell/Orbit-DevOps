package integration_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/hibiken/asynq"
)

// 独立进程真正退出，恢复侧始终走真实 Redis 消费入口，不借消息调用 ClaimNext。
func TestQueueRecoversAcrossProcessInterruptions(t *testing.T) {
	for _, scenario := range []struct {
		mode             string
		action           worker.RecoveryAction
		attempts         int
		publish, observe int
	}{
		{"after_enqueue", worker.RecoverySucceeded, 1, 1, 0},
		{"before_publish", worker.RecoveryApply, 2, 1, 0},
		{"before_commit", worker.RecoverySucceeded, 2, 0, 0},
		{"during_rollout", worker.RecoveryObserve, 2, 0, 1},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			environment := newTestEnvironment(t)
			_, address := testsupport.StartRedis(t)
			accepted := createRelease(t, environment, "queue-process-"+scenario.mode)
			runInterruptedWorkerProcess(t, environment.databaseURL, scenario.mode, filepath.Join(t.TempDir(), "applied"), address)
			if scenario.mode == "after_enqueue" {
				// 真实入队已成功但确认尚未提交，重复制同一消息模拟租约到期后的补发。
				db := openTestDatabase(t, environment.databaseURL)
				var ref operation.DispatchRef
				if err := db.Get(&ref, `SELECT id,operation_id,generation,version FROM operation_dispatches WHERE operation_id=$1 AND published_at IS NULL`, accepted.Operation.ID); err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(ref)
				client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
				defer client.Close()
				if _, err := client.Enqueue(asynq.NewTask(dispatch.TaskType, payload), asynq.Queue("orbitops-release")); err != nil {
					t.Fatal(err)
				}
			}
			publisher := &recoveryRecordingPublisher{observation: worker.RecoveryObservation{Action: scenario.action}}
			service, operations := newQueueWithPublisher(t, environment, address, publisher)
			stop := startQueueTest(t, service)
			current := awaitQueuedStatus(t, operations, accepted.Operation.ID, operation.StatusSucceeded)
			stop()
			if current.AttemptCount != scenario.attempts {
				t.Fatalf("attempt count %d, want %d", current.AttemptCount, scenario.attempts)
			}
			if scenario.attempts == 2 && current.Attempts[0].Status != operation.AttemptOutcomeUnknown {
				t.Fatalf("unknown attempt overwritten: %+v", current.Attempts)
			}
			wantInspect := scenario.attempts - 1
			if publisher.inspectCalls != wantInspect || publisher.publishCalls != scenario.publish || publisher.observeCalls != scenario.observe {
				t.Fatalf("recovery calls inspect/publish/observe=%d/%d/%d", publisher.inspectCalls, publisher.publishCalls, publisher.observeCalls)
			}
			stats, err := operations.ReadDispatchMetrics(context.Background())
			if err != nil || stats.Pending+stats.Published != 0 {
				t.Fatalf("unresolved dispatch: %+v %v", stats, err)
			}
		})
	}
}
