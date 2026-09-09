package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/google/uuid"
)

func TestReleaseOperationSchedulingSerializesEachTargetAndAllowsOtherTargets(t *testing.T) {
	environment := newTestEnvironment(t)
	firstTarget := createDeploymentTarget(t, environment)
	secondTarget := createDeploymentTargetWithSuffix(t, environment, "parallel")
	first := createReleaseForTarget(t, environment, firstTarget.ID, "target-first")
	second := createReleaseForTarget(t, environment, firstTarget.ID, "target-second")
	parallel := createReleaseForTarget(t, environment, secondTarget.ID, "target-parallel")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)

	firstLease := claimReleaseOperation(t, operations, "worker-first")
	if firstLease.ReleaseOperationID.String() != first.ReleaseOperation.ID {
		t.Fatalf("first claim = %s, want %s", firstLease.ReleaseOperationID, first.ReleaseOperation.ID)
	}
	parallelLease := claimReleaseOperation(t, operations, "worker-parallel")
	if parallelLease.ReleaseOperationID.String() != parallel.ReleaseOperation.ID {
		t.Fatalf("parallel claim = %s, want %s", parallelLease.ReleaseOperationID, parallel.ReleaseOperation.ID)
	}
	if parallelLease.DeploymentTargetID == firstLease.DeploymentTargetID {
		t.Fatal("scheduler claimed two active operations for the same deployment target")
	}

	if _, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: "worker-blocked", LeaseDuration: time.Second,
	}); err != nil {
		t.Fatalf("claim blocked target: %v", err)
	} else if claimed {
		t.Fatal("later operation was claimed while the target head was still running")
	}

	if err := operations.Succeed(context.Background(), firstLease); err != nil {
		t.Fatalf("complete target head: %v", err)
	}
	secondLease := claimReleaseOperation(t, operations, "worker-second")
	if secondLease.ReleaseOperationID.String() != second.ReleaseOperation.ID {
		t.Fatalf("second claim = %s, want %s", secondLease.ReleaseOperationID, second.ReleaseOperation.ID)
	}
}

func TestRetryableFailurePreservesQueuePositionAndWaitsForBackoff(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "automatic-retry")
	db := openTestDatabase(t, environment.databaseURL)
	now := time.Now().UTC().Add(time.Minute)
	operations := releaseoperation.New(
		db,
		releaseoperation.WithClock(func() time.Time { return now }),
		releaseoperation.WithAutomaticRetryPolicy(2, func(int) time.Duration { return time.Minute }),
	)
	before, err := operations.Get(context.Background(), mustReleaseOperationID(t, acceptance.ReleaseOperation.ID))
	if err != nil {
		t.Fatalf("get accepted operation: %v", err)
	}
	firstLease := claimReleaseOperation(t, operations, "worker-retry-one")
	result, err := operations.Fail(context.Background(), firstLease, releaseoperation.Failure{
		Code:        "kubernetes_unavailable",
		Summary:     "Kubernetes API is temporarily unavailable",
		Disposition: releaseoperation.Retryable,
	})
	if err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	if !result.RetryScheduled || result.Status != releaseoperation.StatusPending || result.AvailableAt == nil {
		t.Fatalf("failure result = %#v, want pending automatic retry", result)
	}

	duringBackoff, err := operations.Get(context.Background(), firstLease.ReleaseOperationID)
	if err != nil {
		t.Fatalf("get retrying operation: %v", err)
	}
	if duringBackoff.QueuedAt != before.QueuedAt {
		t.Errorf("queuedAt changed from %s to %s", before.QueuedAt, duringBackoff.QueuedAt)
	}
	if duringBackoff.AutomaticRetryCount != 1 || duringBackoff.AttemptCount != 1 {
		t.Errorf(
			"retry counters = automatic %d, attempts %d; want 1 and 1",
			duringBackoff.AutomaticRetryCount,
			duringBackoff.AttemptCount,
		)
	}
	if _, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: "worker-too-early", LeaseDuration: time.Second,
	}); err != nil {
		t.Fatalf("claim during backoff: %v", err)
	} else if claimed {
		t.Fatal("operation was claimed before its retry became available")
	}

	now = now.Add(time.Minute)
	secondLease := claimReleaseOperation(t, operations, "worker-retry-two")
	if secondLease.ReleaseOperationID != firstLease.ReleaseOperationID || secondLease.ReleaseAttemptNumber != 2 {
		t.Fatalf("retry lease = %#v, want same operation with attempt 2", secondLease)
	}
	if err := operations.Succeed(context.Background(), secondLease); err != nil {
		t.Fatalf("complete retry: %v", err)
	}
}

func TestRetryableFailureStopsAfterAutomaticRetryBudget(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "retry-budget")
	db := openTestDatabase(t, environment.databaseURL)
	now := time.Now().UTC().Add(time.Minute)
	operations := releaseoperation.New(
		db,
		releaseoperation.WithClock(func() time.Time { return now }),
		releaseoperation.WithAutomaticRetryPolicy(1, func(int) time.Duration { return 0 }),
	)
	failure := releaseoperation.Failure{
		Code:        "kubernetes_unavailable",
		Summary:     "Kubernetes API is temporarily unavailable",
		Disposition: releaseoperation.Retryable,
	}
	firstLease := claimReleaseOperation(t, operations, "worker-budget-one")
	firstResult, err := operations.Fail(context.Background(), firstLease, failure)
	if err != nil || !firstResult.RetryScheduled {
		t.Fatalf("first failure result = %#v, error = %v; want retry", firstResult, err)
	}
	secondLease := claimReleaseOperation(t, operations, "worker-budget-two")
	secondResult, err := operations.Fail(context.Background(), secondLease, failure)
	if err != nil {
		t.Fatalf("exhaust retry budget: %v", err)
	}
	if secondResult.RetryScheduled || secondResult.Status != releaseoperation.StatusFailed {
		t.Fatalf("second failure result = %#v, want terminal failure", secondResult)
	}

	current, err := operations.Get(context.Background(), mustReleaseOperationID(t, acceptance.ReleaseOperation.ID))
	if err != nil {
		t.Fatalf("get exhausted operation: %v", err)
	}
	if current.AutomaticRetryCount != 1 || current.AttemptCount != 2 ||
		current.RetryDisposition == nil || *current.RetryDisposition != releaseoperation.Retryable {
		t.Errorf("exhausted operation = %#v", current)
	}
}

func claimReleaseOperation(t *testing.T, operations *releaseoperation.Module, workerID string) releaseoperation.Lease {
	t.Helper()
	lease, claimed, err := operations.ClaimNext(context.Background(), releaseoperation.ClaimRequest{
		WorkerID: workerID, LeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatalf("claim operation for %s: %v", workerID, err)
	}
	if !claimed {
		t.Fatalf("claim operation for %s = false, want true", workerID)
	}
	return lease
}

func mustReleaseOperationID(t *testing.T, value string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("parse operation ID %q: %v", value, err)
	}
	return id
}

func createReleaseForTarget(
	t *testing.T,
	environment *testEnvironment,
	targetID string,
	suffix string,
) releaseAcceptanceDocument {
	t.Helper()
	response := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+targetID+"/releases",
		"release-"+suffix,
		`{"imageReference":"registry.example/orbitops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create release %s status = %d, want %d", suffix, response.StatusCode, http.StatusCreated)
	}
	return decodeReleaseAcceptance(t, response)
}

func createDeploymentTargetWithSuffix(
	t *testing.T,
	environment *testEnvironment,
	suffix string,
) deploymentTargetDocument {
	t.Helper()
	projectResponse := environment.postProject(
		t,
		"project-"+suffix,
		fmt.Sprintf(`{"name":"Project %s","slug":"project-%s"}`, suffix, suffix),
	)
	defer projectResponse.Body.Close()
	if projectResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create project %s status = %d", suffix, projectResponse.StatusCode)
	}
	project := decodeProject(t, projectResponse)

	applicationResponse := environment.postJSON(
		t,
		"/api/v1/projects/"+project.ID+"/applications",
		"application-"+suffix,
		fmt.Sprintf(`{"name":"Application %s","slug":"application-%s"}`, suffix, suffix),
	)
	defer applicationResponse.Body.Close()
	if applicationResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create application %s status = %d", suffix, applicationResponse.StatusCode)
	}
	application := decodeApplication(t, applicationResponse)

	targetResponse := environment.postJSON(
		t,
		"/api/v1/applications/"+application.ID+"/deployment-targets",
		"target-"+suffix,
		`{"stage":"development","replicas":1,"containerPort":8080}`,
	)
	defer targetResponse.Body.Close()
	if targetResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create target %s status = %d", suffix, targetResponse.StatusCode)
	}
	target := decodeDeploymentTarget(t, targetResponse)
	target.ProjectID = project.ID
	return target
}
