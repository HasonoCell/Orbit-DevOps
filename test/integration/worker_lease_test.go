package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func TestReleaseOperationLeaseHasOneOwnerAndRecoversAfterExpiry(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "lease-recovery")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)

	firstLease, claimed, err := operations.ClaimNext(
		context.Background(),
		releaseoperation.ClaimRequest{
			WorkerID:      "worker-one",
			LeaseDuration: 100 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("first claim = false, want true")
	}
	if firstLease.ReleaseOperationID.String() != acceptance.ReleaseOperation.ID {
		t.Errorf(
			"first operation id = %s, want %s",
			firstLease.ReleaseOperationID,
			acceptance.ReleaseOperation.ID,
		)
	}
	if firstLease.ReleaseAttemptNumber != 1 {
		t.Errorf("first attempt number = %d, want 1", firstLease.ReleaseAttemptNumber)
	}

	_, claimed, err = operations.ClaimNext(
		context.Background(),
		releaseoperation.ClaimRequest{
			WorkerID:      "worker-two",
			LeaseDuration: time.Second,
		},
	)
	if err != nil {
		t.Fatalf("competing claim: %v", err)
	}
	if claimed {
		t.Fatal("competing claim = true before lease expiry")
	}

	time.Sleep(150 * time.Millisecond)
	secondLease, claimed, err := operations.ClaimNext(
		context.Background(),
		releaseoperation.ClaimRequest{
			WorkerID:      "worker-two",
			LeaseDuration: time.Second,
		},
	)
	if err != nil {
		t.Fatalf("recovery claim: %v", err)
	}
	if !claimed {
		t.Fatal("recovery claim = false, want true")
	}
	if secondLease.ReleaseOperationID != firstLease.ReleaseOperationID {
		t.Errorf("recovered operation id = %s, want %s", secondLease.ReleaseOperationID, firstLease.ReleaseOperationID)
	}
	if secondLease.ReleaseAttemptNumber != 2 {
		t.Errorf("recovery attempt number = %d, want 2", secondLease.ReleaseAttemptNumber)
	}
	if !secondLease.Recovery {
		t.Error("recovery lease was not marked as recovery")
	}
	if err := operations.Succeed(context.Background(), firstLease); !errors.Is(err, releaseoperation.ErrLeaseLost) {
		t.Errorf("stale worker completion error = %v, want ErrLeaseLost", err)
	}

	current, err := operations.Get(context.Background(), secondLease.ReleaseOperationID)
	if err != nil {
		t.Fatalf("get recovered operation: %v", err)
	}
	if current.Status != releaseoperation.StatusRunning {
		t.Errorf("status = %q, want %q", current.Status, releaseoperation.StatusRunning)
	}
	if len(current.Attempts) != 2 {
		t.Fatalf("attempt count = %d, want 2", len(current.Attempts))
	}
	if current.Attempts[0].Status != releaseoperation.AttemptOutcomeUnknown {
		t.Errorf("first attempt status = %q, want %q", current.Attempts[0].Status, releaseoperation.AttemptOutcomeUnknown)
	}
	if current.Attempts[0].ErrorCode == nil ||
		*current.Attempts[0].ErrorCode != releaseoperation.FailureLeaseExpired {
		t.Errorf("first attempt error code = %v, want lease expiry", current.Attempts[0].ErrorCode)
	}
	if current.Attempts[1].Status != releaseoperation.AttemptRunning {
		t.Errorf("second attempt status = %q, want %q", current.Attempts[1].Status, releaseoperation.AttemptRunning)
	}
}

func openTestDatabase(t *testing.T, databaseURL string) *sqlx.DB {
	t.Helper()

	db, err := sqlx.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	return db
}

func createRelease(
	t *testing.T,
	environment *testEnvironment,
	keySuffix string,
) releaseAcceptanceDocument {
	t.Helper()

	target := createDeploymentTarget(t, environment)
	response := environment.postJSON(
		t,
		"/api/v1/deployment-targets/"+target.ID+"/releases",
		"release-"+keySuffix,
		`{"imageReference":"registry.example/orbitops/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	)
	defer response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("create release status = %d, want 201", response.StatusCode)
	}

	return decodeReleaseAcceptance(t, response)
}
