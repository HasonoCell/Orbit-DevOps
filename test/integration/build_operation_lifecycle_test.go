package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/google/uuid"
)

// 可恢复失败会创建新的业务调度代次；只有新的领取才消耗重试预算并产生 Attempt。
func TestBuildOperationRetriesAndAtomicallyCreatesArtifact(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-lifecycle-project")
	application := createApplication(t, environment, project.ID, "build-lifecycle-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-lifecycle",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("d", 40)+`"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create build status = %d", response.StatusCode)
	}
	var acceptance buildAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatal(err)
	}

	now := acceptance.Build.CreatedAt.Add(time.Second)
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL),
		buildoperation.WithClock(func() time.Time { return now }),
		buildoperation.WithAutomaticRetryPolicy(1, func(int) time.Duration { return 100 * time.Millisecond }))
	first := claimBuildDispatch(t, operations, "build-worker-first")
	if err := operations.RecordExecutorIdentity(context.Background(), first, buildoperation.ExecutorIdentity{
		Name: "orbit-devops-build-" + first.BuildAttemptID.String(), UID: "job-uid-first",
	}); err != nil {
		t.Fatal(err)
	}
	failure, err := operations.Fail(context.Background(), first, buildoperation.Failure{
		Code: "registry_unavailable", Summary: "registry temporarily unavailable", Disposition: buildoperation.Retryable,
		LogExcerpt: "bounded failure log",
	})
	if err != nil || !failure.RetryScheduled || failure.Status != buildoperation.StatusPending {
		t.Fatalf("retryable failure = %#v, %v", failure, err)
	}

	now = now.Add(time.Second)
	second := claimBuildDispatch(t, operations, "build-worker-second")
	if second.BuildAttemptNumber != 2 {
		t.Fatalf("second attempt number = %d", second.BuildAttemptNumber)
	}
	if _, err := operations.Fail(context.Background(), first, buildoperation.Failure{
		Code: "late_worker", Summary: "late result", Disposition: buildoperation.NonRetryable,
	}); !errors.Is(err, buildoperation.ErrLeaseLost) {
		t.Fatalf("late worker failure = %v, want lease lost", err)
	}

	digest := "sha256:" + strings.Repeat("e", 64)
	artifact, err := operations.Succeed(context.Background(), second, buildoperation.ArtifactResult{
		Repository: acceptance.Build.DestinationRepository, Digest: digest, LogExcerpt: "build completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.BuildID.String() != acceptance.Build.ID || artifact.ImageReference != acceptance.Build.DestinationRepository+"@"+digest {
		t.Fatalf("artifact = %#v", artifact)
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != buildoperation.StatusSucceeded || current.AttemptCount != 2 ||
		len(current.Attempts) != 2 || current.Attempts[0].Status != buildoperation.AttemptFailed ||
		current.Attempts[1].Status != buildoperation.AttemptSucceeded {
		t.Fatalf("completed build operation = %#v", current)
	}
	// 前端授权查询也必须按执行次序保留重试历史，而不是只返回最终状态和次数。
	detail := environment.get(t, "/api/v1/build-operations/"+acceptance.BuildOperation.ID)
	defer detail.Body.Close()
	var history struct {
		AttemptCount int `json:"attemptCount"`
		Attempts     []struct {
			ID     string `json:"id"`
			Number int    `json:"number"`
			Status string `json:"status"`
		} `json:"attempts"`
	}
	if detail.StatusCode != http.StatusOK || json.NewDecoder(detail.Body).Decode(&history) != nil ||
		history.AttemptCount != 2 || len(history.Attempts) != 2 {
		t.Fatalf("authorized build history: status=%d document=%#v", detail.StatusCode, history)
	}
	if history.Attempts[0].ID != first.BuildAttemptID.String() || history.Attempts[0].Number != 1 || history.Attempts[0].Status != "failed" ||
		history.Attempts[1].ID != second.BuildAttemptID.String() || history.Attempts[1].Number != 2 || history.Attempts[1].Status != "succeeded" {
		t.Fatalf("authorized build attempts are not in execution order: %#v", history.Attempts)
	}
}

// Lease 过期本身不改写历史；只有接管者取得新 Lease 时才把旧 Attempt 记为结果未知。
func TestBuildOperationLeaseRecoveryObservesPreviousAttempt(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "build-recovery-project")
	application := createApplication(t, environment, project.ID, "build-recovery-application")
	response := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/builds", "build-recovery",
		`{"repositoryUrl":"https://github.com/example/demo.git","sourceCommit":"`+strings.Repeat("f", 40)+`"}`)
	defer response.Body.Close()
	var acceptance buildAcceptanceDocument
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&acceptance) != nil {
		t.Fatalf("create recovery build status = %d", response.StatusCode)
	}

	now := acceptance.Build.CreatedAt.Add(time.Second)
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL),
		buildoperation.WithClock(func() time.Time { return now }))
	first := claimBuildDispatch(t, operations, "build-worker-lost")
	now = first.ExpiresAt.Add(time.Second)
	repaired, err := operations.RepairDispatches(context.Background(), 10)
	if err != nil || repaired != 1 {
		t.Fatalf("repair expired build = %d, %v", repaired, err)
	}
	second := claimBuildDispatch(t, operations, "build-worker-recovery")
	if !second.Recovery || second.BuildAttemptNumber != 2 {
		t.Fatalf("recovery lease = %#v", second)
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempts[0].Status != buildoperation.AttemptOutcomeUnknown ||
		current.Attempts[1].RecoveredFromAttemptID == nil || *current.Attempts[1].RecoveredFromAttemptID != first.BuildAttemptID {
		t.Fatalf("recovery attempts = %#v", current.Attempts)
	}
}

func claimBuildDispatch(t *testing.T, operations *buildoperation.Module, workerID string) buildoperation.Lease {
	t.Helper()
	items, err := operations.ReserveDispatches(context.Background(), 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("reserve build dispatch = %d, %v", len(items), err)
	}
	if err := operations.ConfirmDispatch(context.Background(), items[0], "", time.Second); err != nil {
		t.Fatal(err)
	}
	claim, err := operations.ClaimDispatch(context.Background(), items[0].DispatchRef, buildoperation.ClaimRequest{
		WorkerID: workerID, LeaseDuration: time.Minute,
	})
	if err != nil || claim.Outcome != buildoperation.ClaimOutcomeClaimed {
		t.Fatalf("claim build dispatch = %#v, %v", claim, err)
	}
	return claim.Lease
}
