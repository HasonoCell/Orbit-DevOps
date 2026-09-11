package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
)

// 离线准备按批次重入，回退后再次准备必须使旧消息失效且不改写业务历史。
func TestDispatchPreparationIsAtomicAndReentrant(t *testing.T) {
	environment := newTestEnvironment(t)
	accepted := createRelease(t, environment, "prepare-dispatch")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	ctx := context.Background()
	old, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(old) != 1 {
		t.Fatalf("reserve: %v %v", old, err)
	}
	before, _ := operations.Get(ctx, old[0].ReleaseOperationID)
	batch := uuid.New()
	if _, err := db.Exec(`ALTER TABLE operation_dispatches ADD CONSTRAINT test_prepare_failure CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.PrepareDispatches(ctx, batch); err == nil {
		t.Fatal("preparation failure not propagated")
	}
	if _, err := db.Exec(`ALTER TABLE operation_dispatches DROP CONSTRAINT test_prepare_failure`); err != nil {
		t.Fatal(err)
	}
	first, err := operations.PrepareDispatches(ctx, batch)
	if err != nil || first.Replayed || first.Scheduled != 1 {
		t.Fatalf("first prepare: %+v %v", first, err)
	}
	again, err := operations.PrepareDispatches(ctx, batch)
	if err != nil || !again.Replayed || again.Scheduled != 1 {
		t.Fatalf("repeated prepare: %+v %v", again, err)
	}
	claim, err := operations.ClaimDispatch(ctx, old[0].DispatchRef, releaseoperation.ClaimRequest{WorkerID: "stale", LeaseDuration: time.Second})
	if err != nil || claim.Outcome != releaseoperation.ClaimOutcomeIgnored {
		t.Fatalf("old message: %+v %v", claim, err)
	}
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 || items[0].Sequence != 2 {
		t.Fatalf("prepared intents: %+v %v", items, err)
	}
	after, err := operations.Get(ctx, mustReleaseOperationID(t, accepted.ReleaseOperation.ID))
	if err != nil || after.AttemptCount != 0 || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("preparation rewrote history: %+v %v", after, err)
	}
	if _, err := operations.PrepareDispatches(ctx, uuid.New()); err != nil {
		t.Fatal(err)
	}
	items, err = operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 || items[0].Sequence != 3 {
		t.Fatalf("second cutover: %+v %v", items, err)
	}
}

// 只按数据库自身协议隔离，不因外部坏消息撤销合法工作；离线准备是受控修复出口。
func TestUnsupportedStoredDispatchIsQuarantinedAndCanBePrepared(t *testing.T) {
	environment := newTestEnvironment(t)
	createRelease(t, environment, "unsupported-dispatch")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	ctx := context.Background()
	if _, err := db.Exec(`UPDATE operation_dispatches SET protocol_version = 99`); err != nil {
		t.Fatal(err)
	}
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 0 {
		t.Fatalf("unsupported protocol was sent: %+v %v", items, err)
	}
	var count int
	if err := db.Get(&count, `SELECT count(*) FROM operation_dispatches WHERE state='quarantined'`); err != nil || count != 1 {
		t.Fatalf("quarantine: %d %v", count, err)
	}
	if _, err := operations.PrepareDispatches(ctx, uuid.New()); err != nil {
		t.Fatal(err)
	}
	items, err = operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 1 || items[0].ProtocolVersion != 1 {
		t.Fatalf("protocol repair: %+v %v", items, err)
	}
}

// 混合历史工作准备时保留退避与有效租约，只为过期执行登记恢复，不重写 Attempt。
func TestDispatchPreparationPreservesBackoffAndActiveLeases(t *testing.T) {
	environment := newTestEnvironment(t)
	ids := map[string]uuid.UUID{}
	for _, name := range []string{"delayed", "active", "expired", "attention"} {
		target := createDeploymentTargetWithSuffix(t, environment, "prepare-"+name)
		accepted := createReleaseForTarget(t, environment, target.ID, "prepare-"+name)
		ids[name] = uuid.MustParse(accepted.ReleaseOperation.ID)
	}
	now := time.Now().Add(time.Minute)
	operations := releaseoperation.New(openTestDatabase(t, environment.databaseURL), releaseoperation.WithClock(func() time.Time { return now }), releaseoperation.WithAutomaticRetryPolicy(2, func(int) time.Duration { return time.Minute }))
	ctx := context.Background()
	items, err := operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 4 {
		t.Fatalf("mixed reserve: %+v %v", items, err)
	}
	for _, item := range items {
		duration := 5 * time.Minute
		if item.ReleaseOperationID == ids["expired"] {
			duration = time.Second
		}
		claim, err := operations.ClaimDispatch(ctx, item.DispatchRef, releaseoperation.ClaimRequest{WorkerID: "legacy-active", LeaseDuration: duration})
		if err != nil {
			t.Fatal(err)
		}
		switch item.ReleaseOperationID {
		case ids["delayed"]:
			if _, err := operations.Fail(ctx, claim.Lease, releaseoperation.Failure{Code: "kubernetes_unavailable", Summary: "退避中", Disposition: releaseoperation.Retryable}); err != nil {
				t.Fatal(err)
			}
		case ids["attention"]:
			if _, err := operations.HandleUnknownOutcome(ctx, claim.Lease, releaseoperation.Failure{Code: "delivery_outcome_unknown", Summary: "需人工确认", Disposition: releaseoperation.UnknownOutcome}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	delayed, _ := operations.Get(ctx, ids["delayed"])
	now = now.Add(2 * time.Second)
	prepared, err := operations.PrepareDispatches(ctx, uuid.New())
	if err != nil || prepared.Scheduled != 2 {
		t.Fatalf("mixed preparation: %+v %v", prepared, err)
	}
	items, err = operations.ReserveDispatches(ctx, 10, time.Second)
	if err != nil || len(items) != 2 {
		t.Fatalf("mixed prepared intents: %+v %v", items, err)
	}
	for _, item := range items {
		claim, err := operations.ClaimDispatch(ctx, item.DispatchRef, releaseoperation.ClaimRequest{WorkerID: "q1-active", LeaseDuration: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if item.ReleaseOperationID == ids["delayed"] && claim.Outcome != releaseoperation.ClaimOutcomeDeferred {
			t.Fatal("backoff executed early")
		}
		if item.ReleaseOperationID == ids["expired"] && (!claim.Lease.Recovery || claim.Lease.ReleaseAttemptNumber != 2) {
			t.Fatalf("expired recovery: %+v", claim)
		}
	}
	after, _ := operations.Get(ctx, ids["delayed"])
	active, _ := operations.Get(ctx, ids["active"])
	attention, _ := operations.Get(ctx, ids["attention"])
	if !after.AvailableAt.Equal(delayed.AvailableAt) || after.AttemptCount != 1 || active.AttemptCount != 1 || active.Status != releaseoperation.StatusRunning || attention.Status != releaseoperation.StatusAttentionRequired {
		t.Fatal("preparation rewrote backoff, lease or attention state")
	}
}
