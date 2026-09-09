package integration_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/observability"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/HasonoCell/OrbitOps/internal/worker"
)

func TestWorkerPersistsSuccessfulAndFailedTerminalStates(t *testing.T) {
	testCases := []struct {
		name              string
		publishError      error
		waitForTimeout    bool
		operationTimeout  time.Duration
		wantStatus        releaseoperation.ReleaseOperationStatus
		wantAttemptStatus releaseoperation.AttemptStatus
		wantErrorCode     string
	}{
		{
			name:              "successful rollout",
			operationTimeout:  2 * time.Second,
			wantStatus:        releaseoperation.StatusSucceeded,
			wantAttemptStatus: releaseoperation.AttemptSucceeded,
		},
		{
			name:              "image pull failure",
			publishError:      worker.NewFailure("image_pull_failed", "container image could not be pulled"),
			operationTimeout:  2 * time.Second,
			wantStatus:        releaseoperation.StatusFailed,
			wantAttemptStatus: releaseoperation.AttemptFailed,
			wantErrorCode:     "image_pull_failed",
		},
		{
			name:              "rollout timeout",
			waitForTimeout:    true,
			operationTimeout:  100 * time.Millisecond,
			wantStatus:        releaseoperation.StatusFailed,
			wantAttemptStatus: releaseoperation.AttemptFailed,
			wantErrorCode:     "rollout_timeout",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			acceptance := createRelease(t, environment, "worker-terminal-state")
			db := openTestDatabase(t, environment.databaseURL)
			operations := releaseoperation.New(db)
			releases := delivery.New(db, operations, projectauth.New(db))
			publisher := &recordingPublisher{
				err:            testCase.publishError,
				waitForTimeout: testCase.waitForTimeout,
			}
			metrics := observability.NewMetrics(operations.CountPending)
			runner, err := worker.New(worker.Config{
				WorkerID:                "worker-terminal-test",
				LeaseDuration:           3 * time.Second,
				ReleaseOperationTimeout: testCase.operationTimeout,
				Recorder:                metrics,
			}, operations, releases, publisher)
			if err != nil {
				t.Fatalf("create worker: %v", err)
			}

			processed, err := runner.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("run worker once: %v", err)
			}
			if !processed {
				t.Fatal("processed = false, want true")
			}
			if len(publisher.requests) != 1 {
				t.Fatalf("publish requests = %d, want 1", len(publisher.requests))
			}
			publishRequest := publisher.requests[0]
			if publishRequest.ReleaseOperationID.String() != acceptance.ReleaseOperation.ID {
				t.Errorf(
					"publish operation id = %s, want %s",
					publishRequest.ReleaseOperationID,
					acceptance.ReleaseOperation.ID,
				)
			}
			if publishRequest.ReleaseID.String() != acceptance.Release.ID {
				t.Errorf(
					"publish release id = %s, want %s",
					publishRequest.ReleaseID,
					acceptance.Release.ID,
				)
			}
			if publishRequest.ProjectID.String() != acceptance.Release.TargetSnapshot.ProjectID {
				t.Errorf(
					"publish project id = %s, want %s",
					publishRequest.ProjectID,
					acceptance.Release.TargetSnapshot.ProjectID,
				)
			}
			if publishRequest.ImageReference != acceptance.Release.ImageReference {
				t.Errorf(
					"publish image = %q, want %q",
					publishRequest.ImageReference,
					acceptance.Release.ImageReference,
				)
			}

			response := environment.get(t, "/api/v1/release-operations/"+acceptance.ReleaseOperation.ID)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("get operation status = %d, want %d", response.StatusCode, http.StatusOK)
			}
			current := decodeReleaseOperation(t, response)
			if current.Status != testCase.wantStatus {
				t.Errorf("operation status = %q, want %q", current.Status, testCase.wantStatus)
			}
			if current.AttemptCount != 1 || len(current.Attempts) != 1 {
				t.Fatalf(
					"attemptCount = %d, attempts = %d, want 1 and 1",
					current.AttemptCount,
					len(current.Attempts),
				)
			}
			attempt := current.Attempts[0]
			if attempt.WorkerID != "worker-terminal-test" {
				t.Errorf("attempt workerId = %q, want worker-terminal-test", attempt.WorkerID)
			}
			if attempt.Status != testCase.wantAttemptStatus {
				t.Errorf("attempt status = %q, want %q", attempt.Status, testCase.wantAttemptStatus)
			}
			if current.FinishedAt == nil || attempt.FinishedAt == nil {
				t.Error("terminal operation or attempt has no finishedAt")
			}
			if testCase.wantErrorCode == "" {
				if current.ErrorCode != nil || attempt.ErrorCode != nil {
					t.Error("successful operation contains an error code")
				}
			} else {
				if current.ErrorCode == nil || *current.ErrorCode != testCase.wantErrorCode {
					t.Errorf("operation error code = %v, want %q", current.ErrorCode, testCase.wantErrorCode)
				}
				if attempt.ErrorCode == nil || *attempt.ErrorCode != testCase.wantErrorCode {
					t.Errorf("attempt error code = %v, want %q", attempt.ErrorCode, testCase.wantErrorCode)
				}
				if current.RetryDisposition == nil ||
					*current.RetryDisposition != releaseoperation.NonRetryable ||
					attempt.RetryDisposition == nil ||
					*attempt.RetryDisposition != releaseoperation.NonRetryable {
					t.Errorf(
						"retry dispositions = operation %v, attempt %v; want non_retryable",
						current.RetryDisposition,
						attempt.RetryDisposition,
					)
				}
			}

			metricResponse := httptest.NewRecorder()
			metrics.Handler().ServeHTTP(metricResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			metricPayload, err := io.ReadAll(metricResponse.Result().Body)
			if err != nil {
				t.Fatalf("read worker metrics: %v", err)
			}
			category := testCase.wantErrorCode
			if category == "" {
				category = "none"
			}
			wantMetric := `orbitops_operation_terminal_total{category="` + category +
				`",status="` + string(testCase.wantStatus) + `"} 1`
			if !strings.Contains(string(metricPayload), wantMetric) {
				t.Errorf("worker metrics do not contain %q", wantMetric)
			}
		})
	}
}

func TestWorkerRenewsLeaseDuringDelivery(t *testing.T) {
	environment := newTestEnvironment(t)
	acceptance := createRelease(t, environment, "worker-heartbeat")
	db := openTestDatabase(t, environment.databaseURL)
	operations := releaseoperation.New(db)
	releases := delivery.New(db, operations, projectauth.New(db))
	publisher := &blockingPublisher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	primary, err := worker.New(worker.Config{
		WorkerID:                "worker-heartbeat-primary",
		LeaseDuration:           300 * time.Millisecond,
		ReleaseOperationTimeout: 2 * time.Second,
	}, operations, releases, publisher)
	if err != nil {
		t.Fatalf("create primary worker: %v", err)
	}

	primaryResult := make(chan error, 1)
	go func() {
		processed, runErr := primary.RunOnce(context.Background())
		if runErr == nil && !processed {
			runErr = errors.New("primary worker did not process an operation")
		}
		primaryResult <- runErr
	}()

	select {
	case <-publisher.started:
	case <-time.After(time.Second):
		t.Fatal("primary publisher did not start")
	}
	time.Sleep(450 * time.Millisecond)

	competingPublisher := &recordingPublisher{}
	competing, err := worker.New(worker.Config{
		WorkerID:                "worker-heartbeat-competing",
		LeaseDuration:           time.Second,
		ReleaseOperationTimeout: time.Second,
	}, operations, releases, competingPublisher)
	if err != nil {
		t.Fatalf("create competing worker: %v", err)
	}
	processed, err := competing.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run competing worker: %v", err)
	}
	if processed {
		t.Fatal("competing worker processed an operation while primary lease was renewed")
	}
	if len(competingPublisher.requests) != 0 {
		t.Fatalf("competing publish requests = %d, want 0", len(competingPublisher.requests))
	}

	close(publisher.release)
	select {
	case err := <-primaryResult:
		if err != nil {
			t.Fatalf("primary worker result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("primary worker did not finish")
	}

	response := environment.get(t, "/api/v1/release-operations/"+acceptance.ReleaseOperation.ID)
	defer response.Body.Close()
	current := decodeReleaseOperation(t, response)
	if current.Status != releaseoperation.StatusSucceeded {
		t.Errorf("operation status = %q, want %q", current.Status, releaseoperation.StatusSucceeded)
	}
	if current.AttemptCount != 1 {
		t.Errorf("attemptCount = %d, want 1", current.AttemptCount)
	}
}

type recordingPublisher struct {
	requests       []worker.PublishRequest
	err            error
	waitForTimeout bool
}

func (p *recordingPublisher) Publish(ctx context.Context, request worker.PublishRequest) error {
	p.requests = append(p.requests, request)
	if p.waitForTimeout {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.err
}

type blockingPublisher struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingPublisher) Publish(ctx context.Context, _ worker.PublishRequest) error {
	close(p.started)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
