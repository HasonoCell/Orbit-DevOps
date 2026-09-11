package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releasedispatch"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// 队列乱序、重复和多消费者不能绕过目标 FIFO；长期阻塞目标不能饿死独立目标。
func TestQueueFIFOAndAttentionReleaseAcrossWorkers(t *testing.T) {
	environment := newTestEnvironment(t)
	_, address := testsupport.StartRedis(t)
	target := createDeploymentTarget(t, environment)
	first := createReleaseForTarget(t, environment, target.ID, "blocked-head")
	second := createReleaseForTarget(t, environment, target.ID, "blocked-tail")
	otherTarget := createDeploymentTargetWithSuffix(t, environment, "independent")
	other := createReleaseForTarget(t, environment, otherTarget.ID, "independent")
	publisher := publisherFunc(func(_ context.Context, request releaseworker.PublishRequest) error {
		if request.ReleaseOperationID.String() == first.ReleaseOperation.ID {
			return releaseworker.NewUnknownOutcome("delivery_outcome_unknown", "需要人工确认", false)
		}
		return nil
	})
	service, operations := newQueueWithPublisher(t, environment, address, publisher)
	secondService, _ := newQueueWithPublisher(t, environment, address, publisher)
	db := openTestDatabase(t, environment.databaseURL)
	var tail releaseoperation.DispatchRef
	if err := db.Get(&tail, `SELECT id,operation_id,sequence,protocol_version FROM operation_dispatches WHERE operation_id=$1`, second.ReleaseOperation.ID); err != nil {
		t.Fatal(err)
	}
	client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
	defer client.Close()
	payload, _ := json.Marshal(tail)
	for n := 0; n < 12; n++ {
		if _, err := client.Enqueue(asynq.NewTask(releasedispatch.TaskType, payload), asynq.Queue("orbit-devops-release")); err != nil {
			t.Fatal(err)
		}
	}
	startQueueTest(t, service)
	startQueueTest(t, secondService)
	awaitQueuedStatus(t, operations, first.ReleaseOperation.ID, releaseoperation.StatusAttentionRequired)
	current := awaitQueuedStatus(t, operations, other.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	if current.AttemptCount != 1 {
		t.Fatalf("duplicate independent attempts: %d", current.AttemptCount)
	}
	current, err := operations.Get(context.Background(), tail.ReleaseOperationID)
	if err != nil || current.Status != releaseoperation.StatusPending || current.AttemptCount != 0 {
		t.Fatalf("tail bypassed attention head: %+v %v", current, err)
	}
	// 后继的乱序物理消息已经全部确认；解除前序后必须靠持久化意图恢复，而非剩余消息碰巧到达。
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer inspector.Close()
	awaitQueueCondition(t, 5*time.Second, func() bool {
		info, err := inspector.GetQueueInfo("orbit-devops-release")
		return err == nil && info.Pending == 0 && info.Active == 0 && info.Scheduled == 0 && info.Retry == 0
	})
	addMember(t, environment.server, target.ProjectID, "queue-developer", "developer", "queue-member")
	developer := environment.serverForActor(t, "queue-developer")
	denied := requestJSON(t, developer, http.MethodPost, "/api/v1/release-operations/"+first.ReleaseOperation.ID+"/fail", "queue-denied", `{"reason":"不能越权结束未知发布"}`)
	defer denied.Body.Close()
	assertError(t, denied, http.StatusForbidden, "project_permission_denied")
	resolved := environment.postJSON(t, "/api/v1/release-operations/"+first.ReleaseOperation.ID+"/fail", "queue-owner-resolve", `{"reason":"已核验外部状态，允许后继推进"}`)
	resolved.Body.Close()
	if resolved.StatusCode != http.StatusOK {
		t.Fatalf("owner resolution: %d", resolved.StatusCode)
	}
	current = awaitQueuedStatus(t, operations, second.ReleaseOperation.ID, releaseoperation.StatusSucceeded)
	if current.AttemptCount != 1 {
		t.Fatalf("tail duplicate attempts: %d", current.AttemptCount)
	}
}

// 排队取消、运行取消和失联取消分别保留原语义，旧消息不能重新发布。
func TestQueueCancellationAndLostCancellation(t *testing.T) {
	for _, mode := range []string{"pending", "running", "lost"} {
		t.Run(mode, func(t *testing.T) {
			environment := newTestEnvironment(t)
			container, address := testsupport.StartRedis(t)
			accepted := createRelease(t, environment, "cancel-"+mode)
			entered := make(chan struct{}, 1)
			service, operations := newQueueWithPublisher(t, environment, address, publisherFunc(func(ctx context.Context, _ releaseworker.PublishRequest) error {
				entered <- struct{}{}
				<-ctx.Done()
				return ctx.Err()
			}))
			ctx := context.Background()
			items, err := operations.ReserveDispatches(ctx, 10, time.Second)
			if err != nil || len(items) != 1 {
				t.Fatalf("reserve: %+v %v", items, err)
			}
			if mode == "running" {
				// 提前释放测试占用，正式链路自行入队和领取。
				if err := operations.ConfirmDispatch(ctx, items[0], "queue_unavailable", time.Second); err != nil {
					t.Fatal(err)
				}
				startQueueTest(t, service)
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("running cancellation not entered")
				}
			} else if mode == "lost" {
				claim, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, releaseoperation.ClaimRequest{WorkerID: "lost-cancel-worker", LeaseDuration: 500 * time.Millisecond})
				if err != nil || claim.Outcome != releaseoperation.ClaimOutcomeClaimed {
					t.Fatalf("lost claim: %+v %v", claim, err)
				}
			}
			response := environment.postJSON(t, "/api/v1/release-operations/"+accepted.ReleaseOperation.ID+"/cancel", "queue-cancel-"+mode, "")
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("cancel: %d", response.StatusCode)
			}
			if mode != "running" {
				if err := container.Stop(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if err := container.Start(ctx); err != nil {
					t.Fatal(err)
				}
				client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
				defer client.Close()
				payload, _ := json.Marshal(items[0].DispatchRef)
				if _, err := client.Enqueue(asynq.NewTask(releasedispatch.TaskType, payload), asynq.Queue("orbit-devops-release")); err != nil {
					t.Fatal(err)
				}
				startQueueTest(t, service)
			}
			want := releaseoperation.StatusCanceled
			if mode == "lost" {
				want = releaseoperation.StatusAttentionRequired
			}
			current := awaitQueuedStatus(t, operations, accepted.ReleaseOperation.ID, want)
			attempts := 1
			if mode == "pending" {
				attempts = 0
			}
			if current.AttemptCount != attempts {
				t.Fatalf("cancellation added attempt: %+v", current)
			}
			if mode == "pending" || mode == "lost" {
				select {
				case <-entered:
					t.Fatal("canceled operation republished")
				default:
				}
			}
		})
	}
}

// 自动重试写 Outbox 失败时，Attempt 结束、预算增长、可执行时间都必须回滚。
func TestDispatchRetryIntentFailureRollsBackBusinessState(t *testing.T) {
	environment := newTestEnvironment(t)
	accepted := createRelease(t, environment, "retry-outbox-failure")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	ctx := context.Background()
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	claim, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, releaseoperation.ClaimRequest{WorkerID: "atomic-retry", LeaseDuration: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE operation_dispatches ADD CONSTRAINT test_retry_failure CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	failure := releaseoperation.Failure{Code: "kubernetes_unavailable", Summary: "retryable", Disposition: releaseoperation.Retryable}
	if _, err := operations.Fail(ctx, claim.Lease, failure); err == nil {
		t.Fatal("retry outbox failure ignored")
	}
	current, err := operations.Get(ctx, uuid.MustParse(accepted.ReleaseOperation.ID))
	if err != nil || current.Status != releaseoperation.StatusRunning || current.AutomaticRetryCount != 0 || current.Attempts[0].Status != releaseoperation.AttemptRunning {
		t.Fatalf("partial retry: %+v %v", current, err)
	}
	if _, err := db.Exec(`ALTER TABLE operation_dispatches DROP CONSTRAINT test_retry_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Fail(ctx, claim.Lease, failure); err != nil {
		t.Fatal(err)
	}
	items, err = operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 || items[0].Sequence != 2 {
		t.Fatalf("retried intent: %+v %v", items, err)
	}
}
