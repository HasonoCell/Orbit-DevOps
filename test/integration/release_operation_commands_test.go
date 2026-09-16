package integration_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/jmoiron/sqlx"
)

func TestRetryReleaseOperationReusesReleaseOperationAndMovesItToTargetTail(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	failed := createReleaseForTarget(t, environment, target.ID, "manual-retry-failed")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	failedLease := claimReleaseOperation(t, operations, "worker-manual-retry")
	if _, err := operations.Fail(context.Background(), failedLease, releaseoperation.Failure{
		Code: "image_pull_failed", Summary: "image unavailable", Disposition: releaseoperation.NonRetryable,
	}); err != nil {
		t.Fatalf("fail operation before retry: %v", err)
	}
	later := createReleaseForTarget(t, environment, target.ID, "manual-retry-later")

	retryResponse := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+failed.ReleaseOperation.ID+"/retry",
		"retry-failed-operation",
		"",
	)
	defer retryResponse.Body.Close()
	if retryResponse.StatusCode != http.StatusOK {
		t.Fatalf("retry status = %d, want %d", retryResponse.StatusCode, http.StatusOK)
	}
	retried := decodeReleaseOperation(t, retryResponse)
	if retried.ID != failed.ReleaseOperation.ID || retried.Status != releaseoperation.StatusPending {
		t.Fatalf("retried operation = %#v", retried)
	}
	if retried.AttemptCount != 1 || len(retried.Attempts) != 1 ||
		retried.AutomaticRetryCount != 0 {
		t.Errorf("retry changed Attempt history or did not reset automatic budget: %#v", retried)
	}
	if !retried.QueuedAt.After(later.ReleaseOperation.QueuedAt) ||
		!retried.AvailableAt.Equal(retried.QueuedAt) {
		t.Errorf(
			"retry scheduling = queued %s, available %s; later operation queued %s",
			retried.QueuedAt,
			retried.AvailableAt,
			later.ReleaseOperation.QueuedAt,
		)
	}

	replayResponse := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+failed.ReleaseOperation.ID+"/retry",
		"retry-failed-operation",
		"",
	)
	defer replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusOK {
		t.Fatalf("retry replay status = %d, want %d", replayResponse.StatusCode, http.StatusOK)
	}
	replayed := decodeReleaseOperation(t, replayResponse)
	if replayed.ID != retried.ID || !replayed.QueuedAt.Equal(retried.QueuedAt) {
		t.Errorf("retry replay = %#v, want original response %#v", replayed, retried)
	}
	assertAuditCount(t, db, failed.ReleaseOperation.ID, "operation.retry", 1)

	stateConflict := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+failed.ReleaseOperation.ID+"/retry",
		"retry-pending-operation",
		"",
	)
	defer stateConflict.Body.Close()
	assertError(t, stateConflict, http.StatusConflict, "release_operation_state_conflict")

	laterLease := claimReleaseOperation(t, operations, "worker-target-head")
	if laterLease.ReleaseOperationID.String() != later.ReleaseOperation.ID {
		t.Fatalf("target head = %s, want later release %s", laterLease.ReleaseOperationID, later.ReleaseOperation.ID)
	}
	if err := operations.Succeed(context.Background(), laterLease); err != nil {
		t.Fatalf("complete target head: %v", err)
	}
	retryLease := claimReleaseOperation(t, operations, "worker-retried-tail")
	if retryLease.ReleaseOperationID.String() != failed.ReleaseOperation.ID || retryLease.ReleaseAttemptNumber != 2 {
		t.Fatalf("retried tail lease = %#v", retryLease)
	}
}

func TestCancelPendingReleaseOperationIsImmediateAndIdempotent(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "cancel-pending")
	db := openTestDatabase(t, environment.databaseURL)

	cancel := func(key string) operationDocument {
		response := requestJSON(
			t,
			environment.server,
			http.MethodPost,
			"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
			key,
			"",
		)
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("cancel status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		return decodeReleaseOperation(t, response)
	}
	first := cancel("cancel-pending-operation")
	replayed := cancel("cancel-pending-operation")
	if first.Status != releaseoperation.StatusCanceled || first.FinishedAt == nil ||
		first.AttemptCount != 0 || len(first.Attempts) != 0 {
		t.Errorf("canceled pending operation = %#v", first)
	}
	if replayed.Status != first.Status || replayed.FinishedAt == nil ||
		!replayed.FinishedAt.Equal(*first.FinishedAt) {
		t.Errorf("cancel replay = %#v, want %#v", replayed, first)
	}
	assertAuditCount(t, db, acceptance.ReleaseOperation.ID, "operation.cancel", 1)

	operations := releaseoperation.New(db)
	if _, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: "worker-after-cancel", LeaseDuration: time.Second,
	}); err != nil {
		t.Fatalf("claim after cancel: %v", err)
	} else if claimed {
		t.Fatal("canceled pending operation was claimed")
	}
}

func TestWorkerAcknowledgesRunningReleaseOperationCancellation(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "cancel-running")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	releases := delivery.New(db, operations, projectauth.New(db, nil))
	publisher := &blockingPublisher{started: make(chan struct{}), release: make(chan struct{})}
	runner, err := releaseworker.New(releaseworker.Config{
		WorkerID: "worker-cancel", LeaseDuration: 150 * time.Millisecond,
		ReleaseOperationTimeout: 3 * time.Second,
	}, operations, releases, publisher)
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	workerResult := make(chan error, 1)
	go func() {
		processed, runErr := runner.RunOnce(context.Background())
		if runErr == nil && !processed {
			runErr = errors.New("worker did not process operation")
		}
		workerResult <- runErr
	}()
	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not start")
	}

	cancelResponse := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
		"cancel-running-operation",
		"",
	)
	defer cancelResponse.Body.Close()
	if cancelResponse.StatusCode != http.StatusOK {
		t.Fatalf("request cancellation status = %d, want %d", cancelResponse.StatusCode, http.StatusOK)
	}
	requested := decodeReleaseOperation(t, cancelResponse)
	if requested.Status != releaseoperation.StatusCancelRequested {
		t.Fatalf("requested status = %q, want cancel_requested", requested.Status)
	}

	select {
	case err := <-workerResult:
		if err != nil {
			t.Fatalf("worker cancellation result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not acknowledge cancellation")
	}
	current, err := operations.Get(context.Background(), mustReleaseOperationID(t, acceptance.ReleaseOperation.ID))
	if err != nil {
		t.Fatalf("get canceled operation: %v", err)
	}
	if current.Status != releaseoperation.StatusCanceled || current.FinishedAt == nil ||
		len(current.Attempts) != 1 || current.Attempts[0].Status != releaseoperation.AttemptCanceled {
		t.Errorf("worker-canceled operation = %#v", current)
	}
	assertAuditCount(t, db, acceptance.ReleaseOperation.ID, "operation.cancel", 1)
	assertAuditCount(t, db, acceptance.ReleaseOperation.ID, "operation.canceled", 1)
	var actorID, actorKind string
	if err := db.QueryRowContext(
		context.Background(),
		`SELECT actor_id, actor_kind FROM audit_records
		 WHERE target_type = 'operation' AND target_id = $1 AND action = 'operation.canceled'`,
		acceptance.ReleaseOperation.ID,
	).Scan(&actorID, &actorKind); err != nil {
		t.Fatalf("load Worker cancellation audit: %v", err)
	}
	if actorID != "worker-cancel" || actorKind != "system" {
		t.Errorf("Worker cancellation audit actor = %q/%q, want worker-cancel/system", actorID, actorKind)
	}
}

func TestExpiredCancellationRequiresAttentionInsteadOfBlockingForever(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "cancel-expired")
	db := openTestDatabase(t, environment.databaseURL)
	now := time.Now().UTC().Add(time.Minute)
	operations := releaseoperation.New(db, releaseoperation.WithClock(func() time.Time { return now }))
	lease, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: "worker-disappeared", LeaseDuration: 100 * time.Millisecond,
	})
	if err != nil || !claimed {
		t.Fatalf("claim disappearing worker lease = %v, error = %v", claimed, err)
	}
	cancelResponse := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/cancel",
		"cancel-expired-operation",
		"",
	)
	cancelResponse.Body.Close()
	if cancelResponse.StatusCode != http.StatusOK {
		t.Fatalf("request cancellation status = %d, want %d", cancelResponse.StatusCode, http.StatusOK)
	}
	now = now.Add(101 * time.Millisecond)
	if _, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: "worker-recovery", LeaseDuration: time.Second,
	}); err != nil {
		t.Fatalf("recover expired cancellation: %v", err)
	} else if claimed {
		t.Fatal("expired cancellation was incorrectly reclaimed for publishing")
	}
	current, err := operations.Get(context.Background(), lease.ReleaseOperationID)
	if err != nil {
		t.Fatalf("get unknown cancellation: %v", err)
	}
	if current.Status != releaseoperation.StatusAttentionRequired || current.ErrorCode == nil ||
		*current.ErrorCode != "cancellation_outcome_unknown" || len(current.Attempts) != 1 ||
		current.Attempts[0].Status != releaseoperation.AttemptOutcomeUnknown {
		t.Errorf("expired cancellation = %#v", current)
	}
}

func assertAuditCount(
	t *testing.T,
	database *sqlx.DB,
	releaseOperationID string,
	action string,
	want int,
) {
	t.Helper()
	var count int
	if err := database.QueryRowContext(
		context.Background(),
		`SELECT count(*) FROM audit_records
		 WHERE target_type = 'operation' AND target_id = $1 AND action = $2`,
		releaseOperationID,
		action,
	).Scan(&count); err != nil {
		t.Fatalf("count %s audit records: %v", action, err)
	}
	if count != want {
		t.Errorf("%s audit count = %d, want %d", action, count, want)
	}
}
