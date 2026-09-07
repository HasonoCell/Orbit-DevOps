package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/jmoiron/sqlx"
)

const (
	workerInterruptionModeEnv   = "ORBITOPS_TEST_WORKER_INTERRUPTION_MODE"
	workerInterruptionDBEnv     = "ORBITOPS_TEST_WORKER_INTERRUPTION_DATABASE_URL"
	workerInterruptionMarkerEnv = "ORBITOPS_TEST_WORKER_INTERRUPTION_MARKER"
)

func TestWorkerRecoversAcrossRealProcessInterruptions(t *testing.T) {
	testCases := []struct {
		name             string
		mode             string
		recoveryAction   worker.RecoveryAction
		wantMarker       bool
		wantPublishCalls int
		wantObserveCalls int
	}{
		{
			name:             "before Apply",
			mode:             "before_publish",
			recoveryAction:   worker.RecoveryApply,
			wantPublishCalls: 1,
		},
		{
			name:           "after Apply before database commit",
			mode:           "before_commit",
			recoveryAction: worker.RecoverySucceeded,
			wantMarker:     true,
		},
		{
			name:             "during Rollout observation",
			mode:             "during_rollout",
			recoveryAction:   worker.RecoveryObserve,
			wantMarker:       true,
			wantObserveCalls: 1,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := newTestEnvironment(t)
			acceptance := createRelease(t, environment, "process-interruption-"+slugForTest(testCase.name))
			marker := filepath.Join(t.TempDir(), "external-apply-completed")

			runInterruptedWorkerProcess(t, environment.databaseURL, testCase.mode, marker)
			_, markerErr := os.Stat(marker)
			if testCase.wantMarker && markerErr != nil {
				t.Fatalf("external Apply marker is unavailable: %v", markerErr)
			}
			if !testCase.wantMarker && !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("external Apply unexpectedly ran before interruption: %v", markerErr)
			}

			// 子进程被终止后必须等租约自然过期，新的 Worker 才能通过数据库围栏接管。
			time.Sleep(350 * time.Millisecond)
			db := openTestDatabase(t, environment.databaseURL)
			operations := operation.New(db)
			publisher := &recoveryRecordingPublisher{
				observation: worker.RecoveryObservation{Action: testCase.recoveryAction},
			}
			runner := newRecoveryRunner(t, operations, db, publisher)
			processed, err := runner.RunOnce(context.Background())
			if err != nil || !processed {
				t.Fatalf("recover interrupted Worker = %v, error = %v", processed, err)
			}

			current, err := operations.Get(context.Background(), mustOperationID(t, acceptance.Operation.ID))
			if err != nil {
				t.Fatalf("get recovered operation: %v", err)
			}
			if current.Status != operation.StatusSucceeded || current.AttemptCount != 2 {
				t.Fatalf("recovered operation = %#v, want succeeded with two Attempts", current)
			}
			if current.Attempts[0].Status != operation.AttemptOutcomeUnknown ||
				current.Attempts[1].Status != operation.AttemptSucceeded {
				t.Fatalf("interruption attempt history = %#v", current.Attempts)
			}
			if publisher.inspectCalls != 1 || publisher.publishCalls != testCase.wantPublishCalls ||
				publisher.observeCalls != testCase.wantObserveCalls {
				t.Errorf(
					"recovery calls = inspect %d, publish %d, observe %d; want 1, %d, %d",
					publisher.inspectCalls,
					publisher.publishCalls,
					publisher.observeCalls,
					testCase.wantPublishCalls,
					testCase.wantObserveCalls,
				)
			}
			snapshot, err := operations.ReadMetricsSnapshot(context.Background())
			if err != nil {
				t.Fatalf("read recovery metrics snapshot: %v", err)
			}
			if got := labeledCount(snapshot.Events, "operation.reclaimed"); got != 1 {
				t.Errorf("operation.reclaimed metric = %d, want 1", got)
			}
		})
	}
}

// TestWorkerInterruptionHelper 在独立测试进程中运行真实 Runner；父进程用退出或 SIGKILL 制造崩溃窗口。
func TestWorkerInterruptionHelper(t *testing.T) {
	mode := os.Getenv(workerInterruptionModeEnv)
	if mode == "" {
		return
	}
	databaseURL := os.Getenv(workerInterruptionDBEnv)
	marker := os.Getenv(workerInterruptionMarkerEnv)
	db, err := sqlx.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open helper database: %v", err)
	}
	defer db.Close()

	operations := operation.New(db)
	releases := delivery.New(db, operations, projectauth.New(db))
	publisher := processInterruptionPublisher{mode: mode, marker: marker}
	runner, err := worker.New(worker.Config{
		WorkerID:         "worker-interruption-child",
		LeaseDuration:    200 * time.Millisecond,
		OperationTimeout: 30 * time.Second,
		DeliveryHook: func(checkpoint worker.DeliveryCheckpoint, _ worker.PublishRequest) {
			if (mode == "before_publish" && checkpoint == worker.DeliveryBeforePublish) ||
				(mode == "before_commit" && checkpoint == worker.DeliveryBeforeCommit) {
				os.Exit(91)
			}
		},
	}, operations, releases, publisher)
	if err != nil {
		t.Fatalf("create helper Worker: %v", err)
	}
	if address := os.Getenv("ORBITOPS_TEST_INTERRUPTION_REDIS_ADDRESS"); address != "" {
		config := queueConfig(address)
		config.AfterEnqueue = func(operation.Dispatch) {
			if mode == "after_enqueue" {
				os.Exit(91)
			}
		}
		service, err := dispatch.New(config, operations, runner)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "after_enqueue" {
			t.Fatal(service.PublishOnce(context.Background()))
		}
		t.Fatal(service.Run(context.Background()))
	}
	processed, err := runner.RunOnce(context.Background())
	t.Fatalf("helper Worker unexpectedly returned: processed=%v error=%v", processed, err)
}

func runInterruptedWorkerProcess(t *testing.T, databaseURL string, mode string, marker string, queueAddress ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerInterruptionHelper$")
	command.Env = append(os.Environ(),
		workerInterruptionModeEnv+"="+mode,
		workerInterruptionDBEnv+"="+databaseURL,
		workerInterruptionMarkerEnv+"="+marker,
	)
	if len(queueAddress) > 0 {
		command.Env = append(command.Env, "ORBITOPS_TEST_INTERRUPTION_REDIS_ADDRESS="+queueAddress[0])
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start interrupted Worker process: %v", err)
	}

	if mode == "during_rollout" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = command.Process.Kill()
				_ = command.Wait()
				t.Fatalf("Worker did not enter Rollout observation:\n%s", output.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := command.Process.Kill(); err != nil {
			t.Fatalf("kill Worker during Rollout observation: %v", err)
		}
	}

	err := command.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("interrupted Worker did not exit abnormally: %v\n%s", err, output.String())
	}
	if mode != "during_rollout" && exitError.ExitCode() != 91 {
		t.Fatalf("controlled Worker exit code = %d, want 91\n%s", exitError.ExitCode(), output.String())
	}
}

type processInterruptionPublisher struct {
	mode   string
	marker string
}

func (p processInterruptionPublisher) Publish(ctx context.Context, _ worker.PublishRequest) error {
	if err := os.WriteFile(p.marker, []byte("applied"), 0o600); err != nil {
		return fmt.Errorf("write external Apply marker: %w", err)
	}
	if p.mode == "during_rollout" {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func labeledCount(counts []operation.LabeledCount, label string) int {
	for _, count := range counts {
		if count.Label == label {
			return count.Count
		}
	}
	return 0
}
