package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseworker"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

func TestRecoveryUsesKubernetesEvidenceBeforePublishing(t *testing.T) {
	testCases := []struct {
		name             string
		observation      releaseworker.RecoveryObservation
		inspectError     error
		wantStatus       releaseoperation.ReleaseOperationStatus
		wantErrorCode    string
		wantPublishCalls int
		wantRecoveryWait bool
	}{
		{
			name:        "target release is already ready",
			observation: releaseworker.RecoveryObservation{Action: releaseworker.RecoverySucceeded},
			wantStatus:  releaseoperation.StatusSucceeded,
		},
		{
			name:             "resources are absent and safe to apply",
			observation:      releaseworker.RecoveryObservation{Action: releaseworker.RecoveryApply},
			wantStatus:       releaseoperation.StatusSucceeded,
			wantPublishCalls: 1,
		},
		{
			name: "ownership conflict requires attention",
			observation: releaseworker.RecoveryObservation{
				Action:       releaseworker.RecoveryAttention,
				ErrorCode:    "ownership_conflict",
				ErrorSummary: "resource is not owned by this deployment target",
			},
			wantStatus:    releaseoperation.StatusAttentionRequired,
			wantErrorCode: "ownership_conflict",
		},
		{
			name: "temporary Kubernetes outage preserves recovery mode",
			inspectError: releaseworker.NewUnknownOutcome(
				"kubernetes_unavailable",
				"Kubernetes API is temporarily unavailable",
				true,
			),
			wantStatus:       releaseoperation.StatusPending,
			wantRecoveryWait: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			createRelease(t, environment, "recovery-"+slugForTest(testCase.name))
			db := openTestDatabase(t, environment.databaseURL)
			now := time.Now().UTC().Add(time.Minute)
			operations := releaseoperation.New(
				db,
				releaseoperation.WithClock(func() time.Time { return now }),
				releaseoperation.WithAutomaticRetryPolicy(2, func(int) time.Duration { return time.Second }),
			)
			firstLease, claimed, err := operations.ClaimNext(
				context.Background(),
				releaseoperation.ClaimRequest{WorkerID: "worker-lost", LeaseDuration: time.Second},
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
			current, err := operations.Get(context.Background(), firstLease.ReleaseOperationID)
			if err != nil {
				t.Fatalf("get recovered operation: %v", err)
			}
			if current.Status != testCase.wantStatus {
				t.Errorf("status = %q, want %q", current.Status, testCase.wantStatus)
			}
			if len(current.Attempts) != 2 ||
				current.Attempts[0].Status != releaseoperation.AttemptOutcomeUnknown {
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
	operations := releaseoperation.New(db, releaseoperation.WithClock(func() time.Time { return now }))
	previousLease := claimReleaseOperation(t, operations, "worker-previous")
	if err := operations.Succeed(context.Background(), previousLease); err != nil {
		t.Fatalf("complete previous release: %v", err)
	}
	createReleaseForTarget(t, environment, target.ID, "recover-current")
	currentLease := claimReleaseOperation(t, operations, "worker-current-lost")
	now = now.Add(2 * time.Second)
	previousID := mustReleaseOperationID(t, previous.Release.ID)
	publisher := &recoveryRecordingPublisher{
		observation: releaseworker.RecoveryObservation{
			Action:            releaseworker.RecoveryReleaseObserved,
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
	knownCurrent, err := operations.Get(context.Background(), currentLease.ReleaseOperationID)
	if err != nil || knownCurrent.Status != releaseoperation.StatusSucceeded {
		t.Fatalf("known previous recovery = %#v, error = %v", knownCurrent, err)
	}

	unknownAcceptance := createReleaseForTarget(t, environment, target.ID, "recover-unknown")
	unknownLease := claimReleaseOperation(t, operations, "worker-unknown-lost")
	now = now.Add(2 * time.Second)
	unknownID := uuid.New()
	publisher.observation = releaseworker.RecoveryObservation{
		Action:            releaseworker.RecoveryReleaseObserved,
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
	unknownCurrent, err := operations.Get(context.Background(), unknownLease.ReleaseOperationID)
	if err != nil || unknownCurrent.Status != releaseoperation.StatusAttentionRequired ||
		unknownCurrent.ErrorCode == nil || *unknownCurrent.ErrorCode != "unexpected_release_observed" {
		t.Fatalf(
			"unknown release recovery for %s = %#v, error = %v",
			unknownAcceptance.ReleaseOperation.ID,
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
	operations := releaseoperation.New(db, releaseoperation.WithClock(func() time.Time { return now }))
	firstLease := claimReleaseOperation(t, operations, "worker-lost-no-inspection")
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
	current, err := operations.Get(context.Background(), firstLease.ReleaseOperationID)
	if err != nil || current.Status != releaseoperation.StatusAttentionRequired ||
		current.ErrorCode == nil || *current.ErrorCode != "recovery_inspection_unavailable" {
		t.Fatalf("unsupported recovery for %s = %#v, error = %v", acceptance.ReleaseOperation.ID, current, err)
	}
}

func TestManualReconciliationUsesReadOnlyEvidence(t *testing.T) {
	publisher := &recoveryRecordingPublisher{
		observation: releaseworker.RecoveryObservation{Action: releaseworker.RecoverySucceeded},
	}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		RecoveryPublisher: publisher,
	})
	acceptance := createRelease(t, environment, "manual-reconciliation")
	db := openTestDatabase(t, environment.databaseURL)
	forceUnknownAttention(t, db, acceptance.ReleaseOperation.ID)

	response := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/reconcile",
		"reconcile-ready-release",
		"",
	)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reconcile status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	reconciled := decodeReleaseOperation(t, response)
	if reconciled.Status != releaseoperation.StatusSucceeded || reconciled.ErrorCode != nil {
		t.Fatalf("reconciled operation = %#v", reconciled)
	}
	replay := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/reconcile",
		"reconcile-ready-release",
		"",
	)
	defer replay.Body.Close()
	if replay.StatusCode != http.StatusOK || decodeReleaseOperation(t, replay).Status != releaseoperation.StatusSucceeded {
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
	assertAuditCount(t, db, acceptance.ReleaseOperation.ID, "operation.reconcile", 1)
}

func TestDeveloperCanReconcileButOnlyOwnerCanForceFail(t *testing.T) {
	publisher := &recoveryRecordingPublisher{observation: releaseworker.RecoveryObservation{
		Action:       releaseworker.RecoveryAttention,
		ErrorCode:    "ownership_conflict",
		ErrorSummary: "resource ownership remains ambiguous",
	}}
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
		RecoveryPublisher: publisher,
	})
	acceptance := createRelease(t, environment, "manual-role-boundary")
	db := openTestDatabase(t, environment.databaseURL)
	forceUnknownAttention(t, db, acceptance.ReleaseOperation.ID)
	developerUser := environment.ensureActor(t, "recovery-developer")
	addMember(
		t,
		environment.server,
		acceptance.Release.TargetSnapshot.ProjectID,
		developerUser.ID.String(),
		"developer",
		"add-recovery-developer",
	)
	developerServer := environment.serverForActor(t, "recovery-developer")

	reconcileResponse := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/reconcile",
		"developer-reconcile-operation",
		"",
	)
	defer reconcileResponse.Body.Close()
	if reconcileResponse.StatusCode != http.StatusOK {
		t.Fatalf("developer reconcile status = %d, want %d", reconcileResponse.StatusCode, http.StatusOK)
	}
	reconciled := decodeReleaseOperation(t, reconcileResponse)
	if reconciled.Status != releaseoperation.StatusAttentionRequired || reconciled.ErrorCode == nil ||
		*reconciled.ErrorCode != "ownership_conflict" {
		t.Fatalf("developer reconciliation = %#v", reconciled)
	}
	developerRetry := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/retry",
		"developer-retry-unknown-operation",
		"",
	)
	defer developerRetry.Body.Close()
	assertError(t, developerRetry, http.StatusForbidden, "project_permission_denied")

	developerFail := requestJSON(
		t,
		developerServer,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/fail",
		"developer-force-fail",
		`{"reason":"developer should not resolve unknown state"}`,
	)
	defer developerFail.Body.Close()
	assertError(t, developerFail, http.StatusForbidden, "project_permission_denied")

	ownerFail := requestJSON(
		t,
		environment.server,
		http.MethodPost,
		"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/fail",
		"owner-force-fail",
		`{"reason":"owner confirmed the release did not complete"}`,
	)
	defer ownerFail.Body.Close()
	if ownerFail.StatusCode != http.StatusOK {
		t.Fatalf("owner force-fail status = %d, want %d", ownerFail.StatusCode, http.StatusOK)
	}
	failed := decodeReleaseOperation(t, ownerFail)
	if failed.Status != releaseoperation.StatusFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != "manual_resolution" {
		t.Fatalf("owner force-failed operation = %#v", failed)
	}
}

func TestAttentionRetryRequiresSafeExternalEvidence(t *testing.T) {
	testCases := []struct {
		name       string
		action     releaseworker.RecoveryAction
		wantStatus int
		wantState  releaseoperation.ReleaseOperationStatus
	}{
		{name: "absent resources are safe", action: releaseworker.RecoveryApply, wantStatus: http.StatusOK, wantState: releaseoperation.StatusPending},
		{name: "ambiguous resources remain blocked", action: releaseworker.RecoveryAttention, wantStatus: http.StatusConflict, wantState: releaseoperation.StatusAttentionRequired},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			publisher := &recoveryRecordingPublisher{observation: releaseworker.RecoveryObservation{
				Action:       testCase.action,
				ErrorCode:    "ambiguous_state",
				ErrorSummary: "external state is not safe to overwrite",
			}}
			environment := newTestEnvironmentWithDependencies(t, app.Dependencies{
				RecoveryPublisher: publisher,
			})
			acceptance := createRelease(t, environment, "attention-retry-"+slugForTest(testCase.name))
			db := openTestDatabase(t, environment.databaseURL)
			forceUnknownAttention(t, db, acceptance.ReleaseOperation.ID)

			response := requestJSON(
				t,
				environment.server,
				http.MethodPost,
				"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/retry",
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
					"/api/v1/release-operations/"+acceptance.ReleaseOperation.ID+"/retry",
					"retry-attention-operation",
					"",
				)
				defer replay.Body.Close()
				if replay.StatusCode != http.StatusOK || decodeReleaseOperation(t, replay).Status != releaseoperation.StatusPending {
					t.Fatalf("attention retry replay status = %d", replay.StatusCode)
				}
			}
			current, err := releaseoperation.New(db).Get(
				context.Background(),
				mustReleaseOperationID(t, acceptance.ReleaseOperation.ID),
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
	observation  releaseworker.RecoveryObservation
	inspectError error
	inspectCalls int
	publishCalls int
	observeCalls int
}

func (p *recoveryRecordingPublisher) Publish(
	_ context.Context,
	_ releaseworker.PublishRequest,
) error {
	p.publishCalls++
	return nil
}

func (p *recoveryRecordingPublisher) InspectRecovery(
	_ context.Context,
	_ releaseworker.PublishRequest,
) (releaseworker.RecoveryObservation, error) {
	p.inspectCalls++
	return p.observation, p.inspectError
}

func (p *recoveryRecordingPublisher) ObserveRecovery(
	_ context.Context,
	_ releaseworker.PublishRequest,
) error {
	p.observeCalls++
	return nil
}

func newRecoveryRunner(
	t *testing.T,
	operations *releaseoperation.Module,
	db *sqlx.DB,
	publisher releaseworker.Publisher,
) *releaseworker.Runner {
	t.Helper()
	releases := delivery.New(db, operations, projectauth.New(db, nil))
	runner, err := releaseworker.New(releaseworker.Config{
		WorkerID: "worker-recovery", LeaseDuration: time.Second, ReleaseOperationTimeout: time.Second,
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
func forceUnknownAttention(t *testing.T, db *sqlx.DB, releaseOperationID string) {
	t.Helper()
	operations := releaseoperation.New(db)
	lease := claimReleaseOperation(t, operations, "worker-manual-recovery")
	if lease.ReleaseOperationID.String() != releaseOperationID {
		t.Fatalf("claimed operation = %s, want %s", lease.ReleaseOperationID, releaseOperationID)
	}
	if _, err := operations.HandleUnknownOutcome(context.Background(), lease, releaseoperation.Failure{
		Code:             "delivery_outcome_unknown",
		Summary:          "delivery result requires manual reconciliation",
		Disposition:      releaseoperation.UnknownOutcome,
		RetryRecommended: false,
	}, false); err != nil {
		t.Fatalf("move operation to attention_required: %v", err)
	}
}
