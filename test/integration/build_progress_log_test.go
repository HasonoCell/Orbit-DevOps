package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildkube"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// 使用真实 Runner、Kubernetes HTTP 边界和授权查询，验证运行中而非终态才能看见源码日志。
func TestBuildWorkerPublishesRunningSourceLogThroughAuthorizedQuery(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "progress-log-project")
	application := createApplication(t, environment, project.ID, "progress-log-app")
	acceptance := createBuildForArtifact(t, environment, application.ID, "progress-log-build", "a")
	db := openTestDatabase(t, environment.databaseURL)
	operations := buildoperation.New(db, buildoperation.WithAutomaticRetryPolicy(0, nil))
	runner, fixture := newProgressLogRunner(t, db, operations, "progress-log-worker")
	ref := reserveBuildWorkerDispatch(t, operations)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.RunDispatch(ctx, ref)
		done <- err
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("progress-log worker did not stop")
		}
	}()
	operationID := uuid.MustParse(acceptance.BuildOperation.ID)
	deadline := time.Now().Add(9 * time.Second)
	firstLogVisible := false
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			current, _ := operations.Get(context.Background(), operationID)
			if len(current.Attempts) > 0 && current.Attempts[0].ErrorCode != nil {
				t.Fatalf("worker stopped before publishing progress log: %v, code=%s summary=%s", err,
					*current.Attempts[0].ErrorCode, *current.Attempts[0].ErrorSummary)
			}
			t.Fatalf("worker stopped before publishing progress log: %v, status=%s attempts=%#v", err, current.Status, current.Attempts)
		default:
		}
		current, err := operations.Get(context.Background(), operationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(current.Attempts) > 0 {
			logResponse := environment.get(t, "/api/v1/build-attempts/"+current.Attempts[0].ID.String()+"/log")
			var log struct{ Excerpt string }
			err := json.NewDecoder(logResponse.Body).Decode(&log)
			logResponse.Body.Close()
			if logResponse.StatusCode != http.StatusOK || err != nil {
				t.Fatal("read authorized progress log")
			}
			if strings.Contains(log.Excerpt, "Cloning repository") {
				if current.Status != buildoperation.StatusRunning || strings.Contains(log.Excerpt, "fixture-log-secret") ||
					!strings.Contains(log.Excerpt, "[REDACTED]") {
					t.Fatalf("running log is not readable and sanitized: status=%s", current.Status)
				}
				if strings.Contains(log.Excerpt, "Fetching pinned commit") {
					return
				}
				if !firstLogVisible {
					firstLogVisible = true
					fixture.mu.Lock()
					fixture.sourceLog += "Fetching pinned commit...\n"
					fixture.mu.Unlock()
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("running source log never became visible through authorized query")
}

// 超时/观察通道中断可能没有新日志；收束不能擦掉已查询得到的运行中证据。
func TestBuildProgressLogSurvivesTerminalFailureWithoutNewLog(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "retained-log-project")
	application := createApplication(t, environment, project.ID, "retained-log-app")
	createBuildForArtifact(t, environment, application.ID, "retained-log-build", "a")
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL))
	lease := claimBuildDispatch(t, operations, "retained-log-worker")
	if err := operations.RecordProgressLog(context.Background(), lease, "source cloned; waiting for compiler", true); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Fail(context.Background(), lease, buildoperation.Failure{
		Code: "build_timeout", Summary: "fixture timeout", Disposition: buildoperation.NonRetryable,
	}); err != nil {
		t.Fatal(err)
	}
	logResponse := environment.get(t, "/api/v1/build-attempts/"+lease.BuildAttemptID.String()+"/log")
	defer logResponse.Body.Close()
	var log struct {
		Excerpt   string
		Truncated bool
	}
	if logResponse.StatusCode != http.StatusOK || json.NewDecoder(logResponse.Body).Decode(&log) != nil ||
		log.Excerpt != "source cloned; waiting for compiler" || !log.Truncated {
		t.Fatal("terminal failure erased the saved running log")
	}
	if err := operations.RecordProgressLog(context.Background(), lease, "late write", false); !errors.Is(err, buildoperation.ErrLeaseLost) {
		t.Fatalf("write after terminal state = %v", err)
	}
}

// 当前有效 Lease 才能更新摘录；从正常授权接口核验所有拒绝分支都不改变已保存的内容。
func TestBuildProgressLogFencesExpiredAndReplacedWorkers(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "fenced-log-project")
	application := createApplication(t, environment, project.ID, "fenced-log-app")
	acceptance := createBuildForArtifact(t, environment, application.ID, "fenced-log-build", "a")
	now := acceptance.Build.CreatedAt.Add(time.Second)
	operations := buildoperation.New(openTestDatabase(t, environment.databaseURL),
		buildoperation.WithClock(func() time.Time { return now }))
	first := claimBuildDispatch(t, operations, "fenced-log-worker")
	if err := operations.RecordProgressLog(context.Background(), first, "original source log", false); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"worker", "attempt-id", "attempt-number", "operation-id"} {
		t.Run(field, func(t *testing.T) {
			wrong := first
			switch field {
			case "worker":
				wrong.WorkerID = "other-worker"
			case "attempt-id":
				wrong.BuildAttemptID = uuid.New()
			case "attempt-number":
				wrong.BuildAttemptNumber++
			case "operation-id":
				wrong.BuildOperationID = uuid.New()
			}
			if err := operations.RecordProgressLog(context.Background(), wrong, "invalid overwrite", false); !errors.Is(err, buildoperation.ErrLeaseLost) {
				t.Fatalf("mismatched lease write = %v", err)
			}
		})
	}
	for _, invalid := range []string{strings.Repeat("x", 64*1024+1), string([]byte{0xff})} {
		if err := operations.RecordProgressLog(context.Background(), first, invalid, false); err == nil {
			t.Fatal("invalid excerpt was accepted")
		}
	}
	if err := operations.RecordProgressLog(context.Background(), first, "", true); err != nil {
		t.Fatal(err)
	}
	now = first.ExpiresAt
	if err := operations.RecordProgressLog(context.Background(), first, "expired overwrite", false); !errors.Is(err, buildoperation.ErrLeaseLost) {
		t.Fatalf("expired lease write = %v", err)
	}
	now = now.Add(time.Second)
	if repaired, err := operations.RepairDispatches(context.Background(), 10); err != nil || repaired != 1 {
		t.Fatalf("repair expired log worker = %d, %v", repaired, err)
	}
	second := claimBuildDispatch(t, operations, "fenced-log-recovery")
	if err := operations.RecordProgressLog(context.Background(), second, "recovery source log", true); err != nil {
		t.Fatal(err)
	}
	if err := operations.RecordProgressLog(context.Background(), first, "late overwrite", false); !errors.Is(err, buildoperation.ErrLeaseLost) {
		t.Fatalf("replaced worker write = %v", err)
	}
	for _, expected := range []struct {
		attemptID uuid.UUID
		excerpt   string
		truncated bool
	}{{first.BuildAttemptID, "original source log", false}, {second.BuildAttemptID, "recovery source log", true}} {
		logResponse := environment.get(t, "/api/v1/build-attempts/"+expected.attemptID.String()+"/log")
		var log struct {
			Excerpt   string
			Truncated bool
		}
		err := json.NewDecoder(logResponse.Body).Decode(&log)
		logResponse.Body.Close()
		if logResponse.StatusCode != http.StatusOK || err != nil || log.Excerpt != expected.excerpt || log.Truncated != expected.truncated {
			t.Fatal("invalid writer changed an authorized attempt log")
		}
	}
}

// PostgreSQL 日志列的非关键写入故障，不得提前结束正在执行的 Job 或停止租约续租。
func TestBuildWorkerContinuesWhenProgressLogPersistenceFails(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "unavailable-log-project")
	application := createApplication(t, environment, project.ID, "unavailable-log-app")
	acceptance := createBuildForArtifact(t, environment, application.ID, "unavailable-log-build", "a")
	db := openTestDatabase(t, environment.databaseURL)
	// 仅在外部数据库边界注入运行中日志写故障，其他业务/心跳 SQL 和终态写入照常执行。
	if _, err := db.Exec(`CREATE FUNCTION fixture_reject_progress_log() RETURNS trigger LANGUAGE plpgsql AS $$
	 BEGIN IF NEW.status='running' AND NEW.log_excerpt<>OLD.log_excerpt THEN
	 RAISE EXCEPTION 'fixture progress log unavailable'; END IF; RETURN NEW; END $$;
	 CREATE TRIGGER fixture_reject_progress_log BEFORE UPDATE OF log_excerpt ON build_attempts
	 FOR EACH ROW EXECUTE FUNCTION fixture_reject_progress_log()`); err != nil {
		t.Fatal(err)
	}
	operations := buildoperation.New(db, buildoperation.WithAutomaticRetryPolicy(0, nil))
	runner, fixture := newProgressLogRunner(t, db, operations, "unavailable-log-worker")
	ref := reserveBuildWorkerDispatch(t, operations)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := runner.RunDispatch(ctx, ref); done <- err; close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("progress-log worker did not stop")
		}
	}()
	select {
	case err := <-done:
		t.Fatalf("diagnostic persistence interrupted build execution: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	fixture.mu.Lock()
	fixture.failed = true
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	fixture.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not observe the real terminal state after diagnostic persistence failed")
	}
	current, err := operations.Get(context.Background(), uuid.MustParse(acceptance.BuildOperation.ID))
	if err != nil || current.Status != buildoperation.StatusFailed || len(current.Attempts) != 1 {
		t.Fatalf("diagnostic failure changed build outcome: status=%s error=%v", current.Status, err)
	}
	logResponse := environment.get(t, "/api/v1/build-attempts/"+current.Attempts[0].ID.String()+"/log")
	defer logResponse.Body.Close()
	var log struct{ Excerpt string }
	if json.NewDecoder(logResponse.Body).Decode(&log) != nil || !strings.Contains(log.Excerpt, "Cloning repository") {
		t.Fatal("terminal persistence did not recover log evidence")
	}
}

// 两个 Worker 场景共用真实构造入口，差别只在外部数据库和 Kubernetes 的响应。
func newProgressLogRunner(t *testing.T, db *sqlx.DB, operations *buildoperation.Module, workerID string) (*buildworker.Runner, *progressLogKubernetes) {
	t.Helper()
	fixture := newProgressLogKubernetes(t)
	// HTTP fixture 实现 Kubernetes 的 JSON 协议，不接受 client-go 默认优先发送的 Protobuf。
	client, err := kubernetes.NewForConfig(&rest.Config{Host: fixture.server.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := buildkube.New(client, buildkube.Config{
		Namespace: "orbit-build-log-fixture", FieldManager: "progress-log-test",
		GitImage: "git:fixture@sha256:" + strings.Repeat("a", 64), BuildkitImage: "buildkit:fixture@sha256:" + strings.Repeat("b", 64),
		ActiveDeadline: time.Minute, TTL: time.Hour, CPU: "1", Memory: "1Gi", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := buildworker.New(buildworker.Config{WorkerID: workerID, LeaseDuration: time.Second,
		BuildTimeout: 15 * time.Second, PollInterval: 20 * time.Millisecond}, operations,
		build.New(db, build.Config{}, operations, projectauth.New(db, nil)), adapter)
	if err != nil {
		t.Fatal(err)
	}
	return runner, fixture
}

type progressLogKubernetes struct {
	server    *httptest.Server
	mu        sync.Mutex
	job       *batchv1.Job
	sourceLog string
	failed    bool
}

func newProgressLogKubernetes(t *testing.T) *progressLogKubernetes {
	t.Helper()
	fixture := &progressLogKubernetes{sourceLog: "Cloning repository...\npassword=fixture-log-secret\n"}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			if r.URL.Query().Get("container") != "source" {
				http.Error(w, "fixture container has not started", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(fixture.sourceLog))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
			fixture.job = &batchv1.Job{}
			if json.NewDecoder(r.Body).Decode(fixture.job) != nil {
				http.Error(w, "fixture invalid job", http.StatusBadRequest)
				return
			}
			fixture.job.UID = "progress-log-job"
			writeProgressLogJSON(w, fixture.job)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			if fixture.job == nil {
				writeProgressLogJSON(w, &corev1.PodList{})
				return
			}
			controller := true
			labels := map[string]string{"job-name": fixture.job.Name}
			for key, value := range fixture.job.Labels {
				labels[key] = value
			}
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "progress-log-pod", Namespace: fixture.job.Namespace,
				UID: "progress-log-pod-uid", Labels: labels, Annotations: fixture.job.Spec.Template.Annotations,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1",
					Kind: "Job", Name: fixture.job.Name, UID: fixture.job.UID, Controller: &controller}}},
				Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{{Name: "source",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
			if fixture.failed {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128}}
			}
			writeProgressLogJSON(w, &corev1.PodList{Items: []corev1.Pod{pod}})
		case strings.Contains(r.URL.Path, "/jobs/"):
			if fixture.job == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				writeProgressLogJSON(w, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
					Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
				return
			}
			writeProgressLogJSON(w, fixture.job)
		default:
			http.Error(w, "unexpected fixture endpoint", http.StatusNotFound)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func writeProgressLogJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
