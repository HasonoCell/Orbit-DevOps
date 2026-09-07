package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func TestRecoveryUsesKubernetesEvidenceBeforePublishing(t *testing.T) {
	testCases := []struct {
		name             string
		observation      worker.RecoveryObservation
		inspectError     error
		wantStatus       operation.OperationStatus
		wantErrorCode    string
		wantPublishCalls int
		wantRecoveryWait bool
	}{
		{
			name:        "target release is already ready",
			observation: worker.RecoveryObservation{Action: worker.RecoverySucceeded},
			wantStatus:  operation.StatusSucceeded,
		},
		{
			name:             "resources are absent and safe to apply",
			observation:      worker.RecoveryObservation{Action: worker.RecoveryApply},
			wantStatus:       operation.StatusSucceeded,
			wantPublishCalls: 1,
		},
		{
			name: "ownership conflict requires attention",
			observation: worker.RecoveryObservation{
				Action:       worker.RecoveryAttention,
				ErrorCode:    "ownership_conflict",
				ErrorSummary: "resource is not owned by this deployment target",
			},
			wantStatus:    operation.StatusAttentionRequired,
			wantErrorCode: "ownership_conflict",
		},
		{
			name: "temporary Kubernetes outage preserves recovery mode",
			inspectError: worker.NewUnknownOutcome(
				"kubernetes_unavailable",
				"Kubernetes API is temporarily unavailable",
				true,
			),
			wantStatus:       operation.StatusPending,
			wantRecoveryWait: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			createRelease(t, environment, "recovery-"+slugForTest(testCase.name))
			db := openTestDatabase(t, environment.databaseURL)
			now := time.Now().UTC().Add(time.Minute)
			operations := operation.New(
				db,
				operation.WithClock(func() time.Time { return now }),
				operation.WithAutomaticRetryPolicy(2, func(int) time.Duration { return time.Second }),
			)
			firstLease, claimed, err := operations.ClaimNext(
				context.Background(),
				operation.ClaimRequest{WorkerID: "worker-lost", LeaseDuration: time.Second},
			)
			if err != nil || !claimed {
				t.Fatalf("claim lost lease = %v, error = %v", claimed, err)
			}
			now = now.Add(2 * time.Second)
			publisher := &recoveryRecordingPublisher{
				observation:  testCase.observation,
				inspectError: testCase.inspectError,
			}
			runner := newRecoveryRunner(t, operations, db, publisher)
			processed, err := runner.RunOnce(context.Background())
			if err != nil || !processed {
				t.Fatalf("run recovery = %v, error = %v", processed, err)
			}
			current, err := operations.Get(context.Background(), firstLease.OperationID)
			if err != nil {
				t.Fatalf("get recovered operation: %v", err)
			}
			if current.Status != testCase.wantStatus {
				t.Errorf("status = %q, want %q", current.Status, testCase.wantStatus)
			}
			if len(current.Attempts) != 2 ||
				current.Attempts[0].Status != operation.AttemptOutcomeUnknown {
				t.Fatalf("recovery Attempts = %#v", current.Attempts)
			}
			if publisher.publishCalls != testCase.wantPublishCalls {
				t.Errorf("Publish calls = %d, want %d", publisher.publishCalls, testCase.wantPublishCalls)
			}
			if testCase.wantErrorCode != "" &&
				(current.ErrorCode == nil || *current.ErrorCode != testCase.wantErrorCode) {
				t.Errorf("errorCode = %v, want %q", current.ErrorCode, testCase.wantErrorCode)
			}
			if current.RecoveryRequired != testCase.wantRecoveryWait {
				t.Errorf("recoveryRequired = %v, want %v", current.RecoveryRequired, testCase.wantRecoveryWait)
			}
		})
	}
}

func TestRecoveryCanReplaceKnownPreviousReleaseButRejectsUnknownRelease(t *testing.T) {
	environment := newTestEnvironment(t)
	target := createDeploymentTarget(t, environment)
	previous := createReleaseForTarget(t, environment, target.ID, "known-previous")
	db := openTestDatabase(t, environment.databaseURL)
	now := time.Now().UTC().Add(time.Minute)
	operations := operation.New(db, operation.WithClock(func() time.Time { return now }))
	previousLease := claimOperation(t, operations, "worker-previous")
	if err := operations.Succeed(context.Background(), previousLease); err != nil {
		t.Fatalf("complete previous release: %v", err)
	}
	createReleaseForTarget(t, environment, target.ID, "recover-current")
	currentLease := claimOperation(t, operations, "worker-current-lost")
	now = now.Add(2 * time.Second)
	previousID := mustOperationID(t, previous.Release.ID)
	publisher := &recoveryRecordingPublisher{
		observation: worker.RecoveryObservation{
			Action:            worker.RecoveryReleaseObserved,
			ObservedReleaseID: &previousID,
		},
	}
	runner := newRecoveryRunner(t, operations, db, publisher)
	processed, err := runner.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("recover over known previous release = %v, error = %v", processed, err)
	}
	if publisher.publishCalls != 1 {
		t.Fatalf("known previous release Publish calls = %d, want 1", publisher.publishCalls)
	}
	knownCurrent, err := operations.Get(context.Background(), currentLease.OperationID)
	if err != nil || knownCurrent.Status != operation.StatusSucceeded {
		t.Fatalf("known previous recovery = %#v, error = %v", knownCurrent, err)
	}

	unknownAcceptance := createReleaseForTarget(t, environment, target.ID, "recover-unknown")
	unknownLease := claimOperation(t, operations, "worker-unknown-lost")
	now = now.Add(2 * time.Second)
	unknownID := uuid.New()
	publisher.observation = worker.RecoveryObservation{
		Action:            worker.RecoveryReleaseObserved,
		ObservedReleaseID: &unknownID,
	}
	publisher.publishCalls = 0
	processed, err = runner.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("recover over unknown release = %v, error = %v", processed, err)
	}
	if publisher.publishCalls != 0 {
		t.Fatal("unknown release was overwritten during recovery")
	}
	unknownCurrent, err := operations.Get(context.Background(), unknownLease.OperationID)
	if err != nil || unknownCurrent.Status != operation.StatusAttentionRequired ||
		unknownCurrent.ErrorCode == nil || *unknownCurrent.ErrorCode != "unexpected_release_observed" {
		t.Fatalf(
			"unknown release recovery for %s = %#v, error = %v",
			unknownAcceptance.Operation.ID,
			unknownCurrent,
			err,
		)
	}
}

func TestRecoveryWithoutInspectionNeverPublishesBlindly(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "recovery-without-inspection")
	db := openTestDatabase(t, environment.databaseURL)
	now := time.Now().UTC().Add(time.Minute)
	operations := operation.New(db, operation.WithClock(func() time.Time { return now }))
	firstLease := claimOperation(t, operations, "worker-lost-no-inspection")
	now = now.Add(2 * time.Second)
	publisher := &recordingPublisher{}
	runner := newRecoveryRunner(t, operations, db, publisher)
	processed, err := runner.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("run unsupported recovery = %v, error = %v", processed, err)
	}
	if len(publisher.requests) != 0 {
		t.Fatal("publisher was called before recovery inspection")
	}
	current, err := operations.Get(context.Background(), firstLease.OperationID)
	if err != nil || current.Status != operation.StatusAttentionRequired ||
		current.ErrorCode == nil || *current.ErrorCode != "recovery_inspection_unavailable" {
		t.Fatalf("unsupported recovery for %s = %#v, error = %v", acceptance.Operation.ID, current, err)
	}
}

func TestManualReconciliationUsesReadOnlyEvidence(t *testing.T) {
	publisher := &recoveryRecordingPublisher{
		observation: worker.RecoveryObservation{Action: worker.RecoverySucceeded},
	}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		RecoveryPublisher: publisher,
	})
	acceptance := createRelease(t, environment, "manual-reconciliation")
	db := openTestDatabase(t, environment.databaseURL)
	forceUnknownAttention(t, db, acceptance.Operation.ID)

	response := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/reconcile",
		"reconcile-ready-release",
		"",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reconcile status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	reconciled := decodeOperation(t, response)
	if reconciled.Status != operation.StatusSucceeded || reconciled.ErrorCode != nil {
		t.Fatalf("reconciled operation = %#v", reconciled)
	}
	replay := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/reconcile",
		"reconcile-ready-release",
		"",
	)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusOK || decodeOperation(t, replay).Status != operation.StatusSucceeded {
		t.Fatalf("reconcile replay status = %d, want succeeded replay", replay.StatusCode)
	}
	if publisher.inspectCalls != 1 || publisher.publishCalls != 0 || publisher.observeCalls != 0 {
		t.Fatalf(
			"manual reconciliation calls = inspect %d, publish %d, observe %d",
			publisher.inspectCalls,
			publisher.publishCalls,
			publisher.observeCalls,
		)
	}
	assertAuditCount(t, db, acceptance.Operation.ID, "operation.reconcile", 1)
}

func TestDeveloperCanReconcileButOnlyOwnerCanForceFail(t *testing.T) {
	publisher := &recoveryRecordingPublisher{observation: worker.RecoveryObservation{
		Action:       worker.RecoveryAttention,
		ErrorCode:    "ownership_conflict",
		ErrorSummary: "resource ownership remains ambiguous",
	}}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		RecoveryPublisher: publisher,
	})
	acceptance := createRelease(t, environment, "manual-role-boundary")
	db := openTestDatabase(t, environment.databaseURL)
	forceUnknownAttention(t, db, acceptance.Operation.ID)
	addMember(
		t,
		environment.server,
		acceptance.Release.TargetSnapshot.ProjectID,
		"recovery-developer",
		"developer",
		"add-recovery-developer",
	)
	developerServer := environment.serverForActor(t, "recovery-developer")

	reconcileResponse := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/reconcile",
		"developer-reconcile-operation",
		"",
	)
	defer reconcileResponse.Body.Close()
	if reconcileResponse.StatusCode != http.StatusOK {
		t.Fatalf("developer reconcile status = %d, want %d", reconcileResponse.StatusCode, http.StatusOK)
	}
	reconciled := decodeOperation(t, reconcileResponse)
	if reconciled.Status != operation.StatusAttentionRequired || reconciled.ErrorCode == nil ||
		*reconciled.ErrorCode != "ownership_conflict" {
		t.Fatalf("developer reconciliation = %#v", reconciled)
	}
	developerRetry := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/retry",
		"developer-retry-unknown-operation",
		"",
	)
	defer developerRetry.Body.Close()
	assertError(t, developerRetry, http.StatusForbidden, "project_permission_denied")

	developerFail := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/fail",
		"developer-force-fail",
		`{"reason":"developer should not resolve unknown state"}`,
	)
	defer developerFail.Body.Close()
	assertError(t, developerFail, http.StatusForbidden, "project_permission_denied")

	ownerFail := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/operations/"+acceptance.Operation.ID+"/fail",
		"owner-force-fail",
		`{"reason":"owner confirmed the release did not complete"}`,
	)
	defer ownerFail.Body.Close()
	if ownerFail.StatusCode != http.StatusOK {
		t.Fatalf("owner force-fail status = %d, want %d", ownerFail.StatusCode, http.StatusOK)
	}
	failed := decodeOperation(t, ownerFail)
	if failed.Status != operation.StatusFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != "manual_resolution" {
		t.Fatalf("owner force-failed operation = %#v", failed)
	}
}

func TestAttentionRetryRequiresSafeExternalEvidence(t *testing.T) {
	testCases := []struct {
		name       string
		action     worker.RecoveryAction
		wantStatus int
		wantState  operation.OperationStatus
	}{
		{name: "absent resources are safe", action: worker.RecoveryApply, wantStatus: http.StatusOK, wantState: operation.StatusPending},
		{name: "ambiguous resources remain blocked", action: worker.RecoveryAttention, wantStatus: http.StatusConflict, wantState: operation.StatusAttentionRequired},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			publisher := &recoveryRecordingPublisher{observation: worker.RecoveryObservation{
				Action:       testCase.action,
				ErrorCode:    "ambiguous_state",
				ErrorSummary: "external state is not safe to overwrite",
			}}
			environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
				RecoveryPublisher: publisher,
			})
			acceptance := createRelease(t, environment, "attention-retry-"+slugForTest(testCase.name))
			db := openTestDatabase(t, environment.databaseURL)
			forceUnknownAttention(t, db, acceptance.Operation.ID)

			response := requestJSON(
				t,
				environment.server,
				http.MethodPost,
				"/api/v1/operations/"+acceptance.Operation.ID+"/retry",
				"retry-attention-operation",
				"",
			)
			defer response.Body.Close()
			if response.StatusCode != testCase.wantStatus {
				t.Fatalf("attention retry status = %d, want %d", response.StatusCode, testCase.wantStatus)
			}
			if testCase.wantStatus == http.StatusOK {
				replay := requestJSON(
					t,
					environment.server,
					http.MethodPost,
					"/api/v1/operations/"+acceptance.Operation.ID+"/retry",
					"retry-attention-operation",
					"",
				)
				defer replay.Body.Close()
				if replay.StatusCode != http.StatusOK || decodeOperation(t, replay).Status != operation.StatusPending {
					t.Fatalf("attention retry replay status = %d", replay.StatusCode)
				}
			}
			current, err := operation.New(db).Get(
				context.Background(),
				mustOperationID(t, acceptance.Operation.ID),
			)
			if err != nil || current.Status != testCase.wantState {
				t.Fatalf("attention retry operation = %#v, error = %v", current, err)
			}
			if publisher.inspectCalls != 1 || publisher.publishCalls != 0 {
				t.Fatalf("attention retry calls = inspect %d, publish %d", publisher.inspectCalls, publisher.publishCalls)
			}
		})
	}
}

type recoveryRecordingPublisher struct {
	observation  worker.RecoveryObservation
	inspectError error
	inspectCalls int
	publishCalls int
	observeCalls int
}

func (p *recoveryRecordingPublisher) Publish(
	_ context.Context,
	_ worker.PublishRequest,
) error {
	p.publishCalls++
	return nil
}

func (p *recoveryRecordingPublisher) InspectRecovery(
	_ context.Context,
	_ worker.PublishRequest,
) (worker.RecoveryObservation, error) {
	p.inspectCalls++
	return p.observation, p.inspectError
}

func (p *recoveryRecordingPublisher) ObserveRecovery(
	_ context.Context,
	_ worker.PublishRequest,
) error {
	p.observeCalls++
	return nil
}

func newRecoveryRunner(
	t *testing.T,
	operations *operation.Module,
	db *sqlx.DB,
	publisher worker.Publisher,
) *worker.Runner {
	t.Helper()
	releases := delivery.New(db, operations, projectauth.New(db))
	runner, err := worker.New(worker.Config{
		WorkerID: "worker-recovery", LeaseDuration: time.Second, OperationTimeout: time.Second,
	}, operations, releases, publisher)
	if err != nil {
		t.Fatalf("create recovery Worker: %v", err)
	}
	return runner
}

func slugForTest(value string) string {
	result := make([]byte, 0, len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character >= 'a' && character <= 'z' {
			result = append(result, character)
		} else if len(result) == 0 || result[len(result)-1] != '-' {
			result = append(result, '-')
		}
	}
	return string(result)
}

// forceUnknownAttention 构造稳定的未知结果，供人工恢复入口测试复用。
func forceUnknownAttention(t *testing.T, db *sqlx.DB, operationID string) {
	t.Helper()
	operations := operation.New(db)
	lease := claimOperation(t, operations, "worker-manual-recovery")
	if lease.OperationID.String() != operationID {
		t.Fatalf("claimed operation = %s, want %s", lease.OperationID, operationID)
	}
	if _, err := operations.HandleUnknownOutcome(context.Background(), lease, operation.Failure{
		Code:             "delivery_outcome_unknown",
		Summary:          "delivery result requires manual reconciliation",
		Disposition:      operation.UnknownOutcome,
		RetryRecommended: false,
	}, false); err != nil {
		t.Fatalf("move operation to attention_required: %v", err)
	}
}
