package buildkube_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/HasonoCell/Orbit-DevOps/internal/buildkube"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestBuildAdapterSourceFailureIncludesSourceLogWhenBuildkitNeverStarted(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 128},
	}}}
	fixture.pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "buildkit", State: corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"},
	}}}
	fixture.logs["source"] = logResponse{body: "fatal: unable to access repository: Failed to connect to github.com port 443\n"}
	observation, err := fixture.observe(context.Background())
	if err != nil || observation.Phase != buildworker.PhaseFailed || observation.ErrorCode != "source_fetch_failed" ||
		observation.Disposition != buildoperation.Retryable {
		t.Fatalf("source failure observation = %#v, %v", observation, err)
	}
	if !strings.Contains(observation.LogExcerpt, "[source]") || !strings.Contains(observation.LogExcerpt, "Failed to connect to github.com port 443") {
		t.Fatalf("source error missing from excerpt: %q", observation.LogExcerpt)
	}
	if strings.Contains(observation.LogExcerpt, "[buildkit]") || fixture.readLogRequests()["buildkit"] != 0 {
		t.Fatalf("unstarted BuildKit was read: excerpt=%q requests=%v", observation.LogExcerpt, fixture.readLogRequests())
	}
}

func TestBuildAdapterRunningObservationIncludesBothStartedStages(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
	}}}
	fixture.pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "buildkit", State: corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	}}}
	fixture.logs["source"] = logResponse{body: "checked out accepted commit\n"}
	fixture.logs["buildkit"] = logResponse{body: "#7 compiling application\n"}
	observation, err := fixture.observe(context.Background())
	if err != nil || observation.Phase != buildworker.PhaseRunning ||
		!strings.Contains(observation.LogExcerpt, "[source]\nchecked out accepted commit") ||
		!strings.Contains(observation.LogExcerpt, "[buildkit]\n#7 compiling application") {
		t.Fatalf("running stages observation = %#v, %v", observation, err)
	}
}

func TestBuildAdapterLogReadFailureHasSafeMarkerWithoutChangingBuildFailure(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: 128},
	}}}
	fixture.logs["source"] = logResponse{status: http.StatusForbidden, body: "forbidden raw upstream credential must never escape"}
	observation, err := fixture.observe(context.Background())
	if err != nil || observation.Phase != buildworker.PhaseFailed || observation.ErrorCode != "source_fetch_failed" ||
		!strings.Contains(observation.LogExcerpt, "[source]\n[log unavailable]") {
		t.Fatalf("safe log failure = %#v, %v", observation, err)
	}
	if strings.Contains(observation.LogExcerpt, "credential") || strings.Contains(observation.LogExcerpt, "forbidden raw") {
		t.Fatalf("upstream error escaped in excerpt: %q", observation.LogExcerpt)
	}
}

func TestBuildAdapterDoesNotReadLogsFromPodsWithDifferentAttemptOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{name: "different controller UID", mutate: func(pod *corev1.Pod) { pod.OwnerReferences[0].UID = "replacement-job-uid" }},
		{name: "different attempt", mutate: func(pod *corev1.Pod) { pod.Labels[buildkube.BuildAttemptIDLabel] = "another-attempt" }},
		{name: "different input", mutate: func(pod *corev1.Pod) { pod.Annotations[buildkube.InputDigestAnnotation] = "another-input" }},
		{name: "missing controller", mutate: func(pod *corev1.Pod) { pod.OwnerReferences = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBuildLogFixture(t)
			fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			fixture.pods[0].Status.Message = "foreign diagnostic belongs to another attempt"
			fixture.logs["source"] = logResponse{body: "foreign source output"}
			test.mutate(&fixture.pods[0])
			observation, err := fixture.observe(context.Background())
			if err != nil || observation.LogExcerpt != "" || len(fixture.readLogRequests()) != 0 {
				t.Fatalf("foreign pod output escaped: %#v, %v, log requests=%v", observation, err, fixture.readLogRequests())
			}
		})
	}
}

func TestBuildAdapterSlowLogEndpointDoesNotBlockStateObservation(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	fixture.logs["source"] = logResponse{delay: 3 * time.Second, body: "late output"}
	started := time.Now()
	observation, err := fixture.observe(context.Background())
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("log endpoint blocked Observe for %s", elapsed)
	}
	if err != nil || observation.Phase != buildworker.PhaseRunning || !strings.Contains(observation.LogExcerpt, "[log unavailable]") {
		t.Fatalf("bounded log observation = %#v, %v", observation, err)
	}
}

func TestBuildAdapterFrequentPollingRetainsSnapshotAndTerminalForcesFreshLogs(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	fixture.logs["source"] = logResponse{body: "git fetch is running\n"}
	for index := 0; index < 6; index++ {
		observation, err := fixture.observe(context.Background())
		if err != nil || !strings.Contains(observation.LogExcerpt, "git fetch is running") {
			t.Fatalf("running snapshot was lost: %#v, %v", observation, err)
		}
	}
	if got := fixture.readLogRequests()["source"]; got != 1 {
		t.Fatalf("frequent state polling fetched %d log snapshots", got)
	}
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	fixture.pods[0].Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128}}
	fixture.logs["source"] = logResponse{body: "fatal: git fetch failed\n"}
	observation, err := fixture.observe(context.Background())
	if err != nil || observation.Phase != buildworker.PhaseFailed || !strings.Contains(observation.LogExcerpt, "fatal: git fetch failed") {
		t.Fatalf("terminal snapshot was not refreshed: %#v, %v", observation, err)
	}
}

func TestBuildAdapterRetainsLastReadableLogWhenTerminalReadFails(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	fixture.logs["source"] = logResponse{body: "fatal: useful source error\n"}
	if _, err := fixture.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	fixture.pods[0].Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 128}}
	fixture.logs["source"] = logResponse{status: http.StatusForbidden, body: "sensitive upstream error"}
	observation, err := fixture.observe(context.Background())
	if err != nil || !strings.Contains(observation.LogExcerpt, "fatal: useful source error") || !strings.Contains(observation.LogExcerpt, "[log unavailable]") {
		t.Fatalf("known log was replaced by a read failure: %#v, %v", observation, err)
	}
}

func TestBuildAdapterOversizedSourceLogCannotHideBuildkitFailureAndSecrets(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	fixture.pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "buildkit", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
	fixture.logs["source"] = logResponse{body: "token=source-log-secret\n" + strings.Repeat("源码输出", 10000)}
	fixture.logs["buildkit"] = logResponse{body: "password=buildkit-log-secret\nERROR: Dockerfile build failed\n"}
	observation, err := fixture.observe(context.Background())
	if err != nil || !observation.LogTruncated || len(observation.LogExcerpt) > 64*1024 || !utf8.ValidString(observation.LogExcerpt) ||
		!strings.Contains(observation.LogExcerpt, "ERROR: Dockerfile build failed") {
		t.Fatalf("bounded multi-stage excerpt = length %d, truncated %v, BuildKit failure present %v, %v", len(observation.LogExcerpt),
			observation.LogTruncated, strings.Contains(observation.LogExcerpt, "ERROR: Dockerfile build failed"), err)
	}
	if strings.Contains(observation.LogExcerpt, "source-log-secret") || strings.Contains(observation.LogExcerpt, "buildkit-log-secret") ||
		!strings.Contains(observation.LogExcerpt, "[REDACTED]") {
		t.Fatal("credential redaction was lost in the bounded log excerpt")
	}
}

func TestBuildAdapterSanitizesAndBoundsPodDiagnosticWhenContainerHasNoLog(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.Message = "token=pod-diagnostic-secret\n" + strings.Repeat("调度错误", 10000)
	observation, err := fixture.observe(context.Background())
	if err != nil || len(observation.LogExcerpt) > 64*1024 || !observation.LogTruncated || !utf8.ValidString(observation.LogExcerpt) ||
		strings.Contains(observation.LogExcerpt, "pod-diagnostic-secret") {
		t.Fatalf("unsafe pod diagnostic: length=%d truncated=%v utf8=%v credential=%v error=%v", len(observation.LogExcerpt),
			observation.LogTruncated, utf8.ValidString(observation.LogExcerpt), strings.Contains(observation.LogExcerpt, "pod-diagnostic-secret"), err)
	}
}

func TestBuildAdapterLogReadRespondsToCallerCancellation(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	fixture.logs["source"] = logResponse{delay: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _ = fixture.observe(ctx)
	if elapsed := time.Since(started); elapsed > 350*time.Millisecond {
		t.Fatalf("Observe did not release canceled log read: %s", elapsed)
	}
}

func TestBuildAdapterJobMetadataLabelsDoNotChangePodOwnershipContract(t *testing.T) {
	fixture := newBuildLogFixture(t)
	fixture.job.Labels["admission.example/metadata-only"] = "job-only"
	fixture.pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	fixture.logs["source"] = logResponse{body: "owned source log"}
	observation, err := fixture.observe(context.Background())
	if err != nil || !strings.Contains(observation.LogExcerpt, "owned source log") {
		t.Fatalf("unrelated Job metadata changed the accepted ownership contract: %#v, %v", observation, err)
	}
}

type logResponse struct {
	body   string
	status int
	delay  time.Duration
}

// 测试只替换外部 Kubernetes HTTP 边界，Job 渲染、Observe 与日志脱敏均使用真实实现。
type buildLogFixture struct {
	adapter   *buildkube.Adapter
	execution buildworker.BuildExecution
	job       *batchv1.Job
	pods      []corev1.Pod
	logs      map[string]logResponse
	mu        sync.Mutex
	requests  map[string]int
}

func newBuildLogFixture(t *testing.T) *buildLogFixture {
	t.Helper()
	execution := buildExecution()
	job := newAdapter(t, fake.NewClientset()).RenderJob(execution)
	job.UID = "build-log-job-uid"
	controller := true
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "build-log-pod", Namespace: job.Namespace,
		UID: "build-log-pod-uid", Labels: job.Spec.Template.DeepCopy().Labels, Annotations: job.Spec.Template.DeepCopy().Annotations,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}},
		Spec: job.Spec.Template.Spec}
	fixture := &buildLogFixture{execution: execution, job: job, pods: []corev1.Pod{pod}, logs: map[string]logResponse{}, requests: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	fixture.adapter, err = buildkube.New(client, buildkube.Config{
		Namespace: job.Namespace, FieldManager: "orbit-devops-build-worker",
		GitImage:       "alpine/git:v2.49.1@sha256:" + strings.Repeat("a", 64),
		BuildkitImage:  "moby/buildkit:v0.33.0-rootless@sha256:" + strings.Repeat("b", 64),
		ActiveDeadline: 20 * time.Minute, TTL: time.Hour, CPU: "1", Memory: "1Gi", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *buildLogFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/apis/batch/v1/namespaces/" + f.job.Namespace + "/jobs/" + f.job.Name:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.job)
	case "/api/v1/namespaces/" + f.job.Namespace + "/pods":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&corev1.PodList{Items: f.pods})
	case "/api/v1/namespaces/" + f.job.Namespace + "/pods/build-log-pod/log":
		container := r.URL.Query().Get("container")
		f.mu.Lock()
		f.requests[container]++
		response := f.logs[container]
		f.mu.Unlock()
		if response.delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(response.delay):
			}
		}
		if response.status != 0 {
			w.WriteHeader(response.status)
		}
		_, _ = w.Write([]byte(response.body))
	default:
		http.NotFound(w, r)
	}
}

func (f *buildLogFixture) observe(ctx context.Context) (buildworker.ExecutionObservation, error) {
	return f.adapter.Observe(ctx, f.execution, buildworker.ExecutionIdentity{Name: f.job.Name, UID: string(f.job.UID)})
}

func (f *buildLogFixture) readLogRequests() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make(map[string]int, len(f.requests))
	for container, count := range f.requests {
		result[container] = count
	}
	return result
}
