package integration_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/releasedispatch"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/hibiken/asynq"
)

// 独立进程真正退出，恢复侧始终走真实 Redis 消费入口，不借消息调用 ClaimNext。
func TestQueueRecoversAcrossProcessInterruptions(t *testing.T) {
	for _, scenario := range []struct {
		mode             string
		action           releaseworker.RecoveryAction
		attempts         int
		publish, observe int
	}{
		{"after_enqueue", releaseworker.RecoverySucceeded, 1, 1, 0},
		{"before_publish", releaseworker.RecoveryApply, 2, 1, 0},
		{"before_commit", releaseworker.RecoverySucceeded, 2, 0, 0},
		{"during_rollout", releaseworker.RecoveryObserve, 2, 0, 1},
	} {
		t.Run(scenario.mode, func(t *testing.T) {
			environment := newTestEnvironment(t)
			_, address := testsupport.StartRedis(t)
			accepted := createRelease(t, environment, "queue-process-"+scenario.mode)
			runInterruptedWorkerProcess(t, environment.databaseURL, scenario.mode, filepath.Join(t.TempDir(), "applied"), address)
			if scenario.mode == "after_enqueue" {
				// 真实入队已成功但确认尚未提交，重复制同一消息模拟租约到期后的补发。
				db := openTestDatabase(t, environment.databaseURL)
				var ref releaseoperation.DispatchRef
				if err := db.Get(&ref, `SELECT id,operation_id,sequence,protocol_version FROM operation_dispatches WHERE operation_id=$1 AND published_at IS NULL`, accepted.ReleaseOperation.ID); err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(ref)
				client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
				defer client.Close()
				if _, err := client.Enqueue(asynq.NewTask(releasedispatch.TaskType, payload), asynq.Queue("orbit-devops-release")); err != nil {
					t.Fatal(err)
				}
			}
			publisher := &recoveryRecordingPublisher{observation: releaseworker.RecoveryObservation{Action: scenario.action}}
			service, operations := newQueueWithPublisher(t, environment, address, publisher)
			stop := startQueueTest(t, service)
			current := awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
			stop()
			if current.AttemptCount != scenario.attempts {
				t.Fatalf("attempt count %d, want %d", current.AttemptCount, scenario.attempts)
			}
			if scenario.attempts == 2 && current.Attempts[0].Status != releaseoperation.AttemptOutcomeUnknown {
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
