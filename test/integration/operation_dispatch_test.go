package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/operation"
)

// 发布受理必须留下可投递的持久化意图，而不是依赖请求结束后的入队动作。
func TestAcceptedReleaseHasOneDurableDispatch(t *testing.T) {
	environment := newTestEnvironment(t)
	accepted := createRelease(t, environment, "durable-dispatch")
	operations := operation.New(openTestDatabase(t, environment.databaseURL))
	ctx := context.Background()
	dispatches, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatches) != 1 || dispatches[0].OperationID.String() != accepted.Operation.ID || dispatches[0].Generation != 1 {
		t.Fatalf("dispatches = %#v, want accepted operation at generation 1", dispatches)
	}
	if again, err := operations.ReserveDispatches(ctx, 10, time.Second); err != nil || len(again) != 0 {
		t.Fatalf("reserved twice: %#v, error %v", again, err)
	}
	current, err := operations.Get(ctx, mustOperationID(t, accepted.Operation.ID))
	if err != nil || current.Status != operation.StatusPending || current.AttemptCount != 0 {
		t.Fatalf("transport created an execution: %#v, error %v", current, err)
	}
}

// Outbox 写入失败必须连同发布受理一起回滚；测试故障只注入该测试独占的数据库。
func TestDispatchFailureRollsBackReleaseAcceptance(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	db := openTestDatabase(t, environment.databaseURL)
	if _, err := db.Exec(`ALTER TABLE operation_dispatches ADD CONSTRAINT test_reject_dispatch CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	response := environment.postJSON(t, "/api/v1/deployment-targets/"+target.ID+"/releases", "outbox-failure", `{"imageReference":"registry.example/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("response = %d", response.StatusCode)
	}
	history := environment.get(t, "/api/v1/deployment-targets/"+target.ID+"/releases")
	defer history.Body.Close()
	if page := decodeReleaseHistoryPage(t, history); len(page.Items) != 0 {
		t.Fatalf("partial release accepted: %#v", page)
	}
	if _, err := db.Exec(`ALTER TABLE operation_dispatches DROP CONSTRAINT test_reject_dispatch`); err != nil {
		t.Fatal(err)
	}
	retry := environment.postJSON(t, "/api/v1/deployment-targets/"+target.ID+"/releases", "outbox-failure", `{"imageReference":"registry.example/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	retry.Body.Close()
	if retry.StatusCode != http.StatusCreated {
		t.Fatalf("same idempotency key after rollback: %d", retry.StatusCode)
	}
	operations := operation.New(db)
	items, err := operations.ReserveDispatches(context.Background(), 10, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("recovered acceptance: %#v %v", items, err)
	}
}

// 入队后消息丢失时，宽限期结束补发同一代次；旧投递确认不能覆盖延期结果。
func TestPublishedDispatchIsRedeliveredWithoutNewBusinessAttempt(t *testing.T) {
	environment := newTestEnvironment(t)
	createRelease(t, environment, "dispatch-lost")
	now := time.Now().UTC().Add(time.Minute)
	operations := operation.New(openTestDatabase(t, environment.databaseURL), operation.WithClock(func() time.Time { return now }))
	ctx := context.Background()
	first, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("reserve: %#v %v", first, err)
	}
	if err := operations.ConfirmDispatch(ctx, first[0], "", time.Second); err != nil {
		t.Fatal(err)
	}
	if items, err := operations.ReserveDispatches(ctx, 10, time.Second); err != nil || len(items) != 0 {
		t.Fatalf("premature replay: %#v %v", items, err)
	}
	now = now.Add(2 * time.Second)
	second, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(second) != 1 || second[0].DispatchRef != first[0].DispatchRef || second[0].PublishToken == first[0].PublishToken {
		t.Fatalf("redelivery: %#v %v", second, err)
	}
	if err := operations.ConfirmDispatch(ctx, first[0], "queue_unavailable", time.Second); err != nil {
		t.Fatal(err)
	}
	claim, err := operations.ClaimDispatch(ctx, second[0].DispatchRef, operation.ClaimRequest{WorkerID: "recovered", LeaseDuration: time.Second})
	if err != nil || claim.Lease.AttemptNumber != 1 {
		t.Fatalf("recovered: %#v %v", claim, err)
	}
}

// 自动重试必须持久化新的意图；旧消息不能跨越业务重试代次。
func TestDispatchRetryKeepsBudgetAndRejectsOldGeneration(t *testing.T) {
	environment := newTestEnvironment(t)
	accepted := createRelease(t, environment, "dispatch-retry")
	now := time.Now().UTC().Add(time.Minute)
	operations := operation.New(openTestDatabase(t, environment.databaseURL), operation.WithClock(func() time.Time { return now }), operation.WithAutomaticRetryPolicy(1, func(int) time.Duration { return time.Minute }))
	ctx := context.Background()
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("reserve = %#v %v", items, err)
	}
	first, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, operation.ClaimRequest{WorkerID: "retry", LeaseDuration: time.Minute})
	if err != nil || first.Disposition != operation.DispatchClaimed {
		t.Fatalf("claim = %#v %v", first, err)
	}
	result, err := operations.Fail(ctx, first.Lease, operation.Failure{Code: "kubernetes_unavailable", Summary: "temporarily unavailable", Disposition: operation.Retryable})
	if err != nil || !result.RetryScheduled {
		t.Fatalf("retry = %#v %v", result, err)
	}
	next, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(next) != 1 || next[0].Generation != 2 {
		t.Fatalf("retry dispatch = %#v %v", next, err)
	}
	old, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, operation.ClaimRequest{WorkerID: "old", LeaseDuration: time.Minute})
	if err != nil || old.Disposition != operation.DispatchIgnored {
		t.Fatalf("old = %#v %v", old, err)
	}
	early, err := operations.ClaimDispatch(ctx, next[0].DispatchRef, operation.ClaimRequest{WorkerID: "early", LeaseDuration: time.Minute})
	if err != nil || early.Disposition != operation.DispatchDeferred {
		t.Fatalf("early = %#v %v", early, err)
	}
	// 延期先于旧发送者确认，迟到确认不能把下一次可调度时间推到一个小时之后。
	if err := operations.ConfirmDispatch(ctx, next[0], "", time.Hour); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if ready, err := operations.ReserveDispatches(ctx, 10, time.Second); err != nil || len(ready) != 1 || ready[0].Generation != 2 {
		t.Fatalf("late confirmation overwrote deferred schedule: %+v %v", ready, err)
	}
	second, err := operations.ClaimDispatch(ctx, next[0].DispatchRef, operation.ClaimRequest{WorkerID: "second", LeaseDuration: time.Minute})
	if err != nil || second.Lease.AttemptNumber != 2 {
		t.Fatalf("second = %#v %v", second, err)
	}
	if _, err := operations.Fail(ctx, second.Lease, operation.Failure{Code: "kubernetes_unavailable", Summary: "temporarily unavailable", Disposition: operation.Retryable}); err != nil {
		t.Fatal(err)
	}
	current, err := operations.Get(ctx, mustOperationID(t, accepted.Operation.ID))
	if err != nil || current.AutomaticRetryCount != 1 || current.AttemptCount != 2 || current.Status != operation.StatusFailed {
		t.Fatalf("budget = %#v %v", current, err)
	}
}

// 消息只能领取自身意图，不能借另一条消息执行全局队列中的其他任务。
func TestDispatchClaimsOnlyItsOperationAndFencesDuplicateDelivery(t *testing.T) {
	environment := newTestEnvironment(t)
	first := createRelease(t, environment, "dispatch-first")
	secondTarget := createDeploymentTargetWithSuffix(t, environment, "dispatch-other")
	second := createReleaseForTarget(t, environment, secondTarget.ID, "dispatch-second")
	operations := operation.New(openTestDatabase(t, environment.databaseURL))
	ctx := context.Background()
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 2 {
		t.Fatalf("reserve = %#v, %v", items, err)
	}
	var dispatch operation.Dispatch
	for _, item := range items {
		if item.OperationID.String() == second.Operation.ID {
			dispatch = item
		}
	}
	claim, err := operations.ClaimDispatch(ctx, dispatch.DispatchRef, operation.ClaimRequest{WorkerID: "queue-worker", LeaseDuration: time.Second})
	if err != nil || claim.Disposition != operation.DispatchClaimed || claim.Lease.OperationID.String() != second.Operation.ID {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	duplicate, err := operations.ClaimDispatch(ctx, dispatch.DispatchRef, operation.ClaimRequest{WorkerID: "duplicate", LeaseDuration: time.Second})
	if err != nil || duplicate.Disposition != operation.DispatchIgnored {
		t.Fatalf("duplicate = %#v, %v", duplicate, err)
	}
	current, err := operations.Get(ctx, mustOperationID(t, first.Operation.ID))
	if err != nil || current.AttemptCount != 0 {
		t.Fatalf("unrelated work was claimed: %#v, %v", current, err)
	}
	if err := operations.Succeed(ctx, claim.Lease); err != nil {
		t.Fatal(err)
	}
	if err := operations.ConfirmDispatch(ctx, dispatch, "", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.ClaimDispatch(ctx, dispatch.DispatchRef, operation.ClaimRequest{WorkerID: "late", LeaseDuration: time.Second}); err != nil {
		t.Fatal(err)
	}
	finished, err := operations.Get(ctx, dispatch.OperationID)
	if err != nil || finished.Status != operation.StatusSucceeded || finished.AttemptCount != 1 {
		t.Fatalf("terminal = %#v, %v", finished, err)
	}
}

// 运输消息重投不能接管已消费代次；真实租约过期由独立恢复检查登记新意图。
func TestExpiredDispatchCreatesOneRecoveryIntentAndFencesOldWorker(t *testing.T) {
	environment := newTestEnvironment(t)
	createRelease(t, environment, "dispatch-recovery")
	now := time.Now().UTC().Add(time.Minute)
	operations := operation.New(openTestDatabase(t, environment.databaseURL), operation.WithClock(func() time.Time { return now }))
	ctx := context.Background()
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatalf("reserve: %#v %v", items, err)
	}
	first, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, operation.ClaimRequest{WorkerID: "old", LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	old, err := operations.ClaimDispatch(ctx, items[0].DispatchRef, operation.ClaimRequest{WorkerID: "redelivery", LeaseDuration: time.Second})
	if err != nil || old.Disposition != operation.DispatchIgnored {
		t.Fatalf("old: %#v %v", old, err)
	}
	if count, err := operations.RepairDispatches(ctx, 100); err != nil || count != 1 {
		t.Fatalf("repair: %d %v", count, err)
	}
	if count, err := operations.RepairDispatches(ctx, 100); err != nil || count != 0 {
		t.Fatalf("repeat repair: %d %v", count, err)
	}
	recovered, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(recovered) != 1 || recovered[0].Generation != 2 {
		t.Fatalf("recovery: %#v %v", recovered, err)
	}
	second, err := operations.ClaimDispatch(ctx, recovered[0].DispatchRef, operation.ClaimRequest{WorkerID: "new", LeaseDuration: time.Second})
	if err != nil || !second.Lease.Recovery || second.Lease.AttemptNumber != 2 {
		t.Fatalf("reclaim: %#v %v", second, err)
	}
	if err := operations.Succeed(ctx, first.Lease); err != operation.ErrLeaseLost {
		t.Fatalf("old completion: %v", err)
	}
	if err := operations.Succeed(ctx, second.Lease); err != nil {
		t.Fatal(err)
	}
	current, err := operations.Get(ctx, second.Lease.OperationID)
	if err != nil || current.Attempts[0].Status != operation.AttemptOutcomeUnknown || current.AutomaticRetryCount != 1 {
		t.Fatalf("history: %#v %v", current, err)
	}
}
