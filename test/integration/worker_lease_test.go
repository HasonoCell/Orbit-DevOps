package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/operation"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func TestOperationLeaseHasOneOwnerAndRecoversAfterExpiry(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "lease-recovery")
	db := openTestDatabase(t, environment.databaseURL)
	operations := operation.New(db)

	firstLease, claimed, err := operations.ClaimNext(
		context.Background(),
		operation.ClaimRequest{
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
	if firstLease.OperationID.String() != acceptance.Operation.ID {
		t.Errorf(
			"first operation id = %s, want %s",
			firstLease.OperationID,
			acceptance.Operation.ID,
		)
	}
	if firstLease.AttemptNumber != 1 {
		t.Errorf("first attempt number = %d, want 1", firstLease.AttemptNumber)
	}

	_, claimed, err = operations.ClaimNext(
		context.Background(),
		operation.ClaimRequest{
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
		operation.ClaimRequest{
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
	if secondLease.OperationID != firstLease.OperationID {
		t.Errorf("recovered operation id = %s, want %s", secondLease.OperationID, firstLease.OperationID)
	}
	if secondLease.AttemptNumber != 2 {
		t.Errorf("recovery attempt number = %d, want 2", secondLease.AttemptNumber)
	}
	if !secondLease.Recovery {
		t.Error("recovery lease was not marked as recovery")
	}
	if err := operations.Succeed(context.Background(), firstLease); !errors.Is(err, operation.ErrLeaseLost) {
		t.Errorf("stale worker completion error = %v, want ErrLeaseLost", err)
	}

	current, err := operations.Get(context.Background(), secondLease.OperationID)
	if err != nil {
		t.Fatalf("get recovered operation: %v", err)
	}
	if current.Status != operation.StatusRunning {
		t.Errorf("status = %q, want %q", current.Status, operation.StatusRunning)
	}
	if len(current.Attempts) != 2 {
		t.Fatalf("attempt count = %d, want 2", len(current.Attempts))
	}
	if current.Attempts[0].Status != operation.AttemptOutcomeUnknown {
		t.Errorf("first attempt status = %q, want %q", current.Attempts[0].Status, operation.AttemptOutcomeUnknown)
	}
	if current.Attempts[0].ErrorCode == nil ||
		*current.Attempts[0].ErrorCode != operation.FailureLeaseExpired {
		t.Errorf("first attempt error code = %v, want lease expiry", current.Attempts[0].ErrorCode)
	}
	if current.Attempts[1].Status != operation.AttemptRunning {
		t.Errorf("second attempt status = %q, want %q", current.Attempts[1].Status, operation.AttemptRunning)
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
