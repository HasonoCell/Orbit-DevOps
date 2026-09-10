package buildkube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/buildkube"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildAdapterReusesOwnedJobAndRejectsForeignJob(t *testing.T) {
	execution := buildExecution()
	adapter := newAdapter(t, fake.NewClientset())
	job := adapter.RenderJob(execution)
	job.UID = types.UID("owned-job-uid")
	client := fake.NewClientset(job)
	adapter = newAdapter(t, client)
	identity, err := adapter.Start(context.Background(), execution)
	if err != nil || identity.Name != job.Name || identity.UID != "owned-job-uid" {
		t.Fatalf("reuse owned job = %#v, %v", identity, err)
	}
	jobs, err := client.BatchV1().Jobs(job.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs after idempotent start = %d, %v", len(jobs.Items), err)
	}

	foreign := job.DeepCopy()
	foreign.Labels[buildkube.BuildIDLabel] = uuid.NewString()
	foreignClient := fake.NewClientset(foreign)
	_, err = newAdapter(t, foreignClient).Start(context.Background(), execution)
	var failure *buildworker.FailureError
	if !errors.As(err, &failure) || failure.Code() != "build_job_ownership_conflict" {
		t.Fatalf("foreign job error = %v", err)
	}
}

func TestBuildAdapterReadsOnlyValidatedTerminationResult(t *testing.T) {
	execution := buildExecution()
	adapter := newAdapter(t, fake.NewClientset())
	job := adapter.RenderJob(execution)
	job.UID = "successful-job-uid"
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "successful-pod", Namespace: job.Namespace,
		Labels: map[string]string{"job-name": job.Name}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name: "buildkit", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 0, Message: `{"repository":"` + execution.DestinationRepository + `","digest":"sha256:` + strings.Repeat("e", 64) + `"}`,
		}},
	}}}}
	client := fake.NewClientset(job, pod)
	observation, err := newAdapter(t, client).Observe(context.Background(), execution,
		buildworker.ExecutionIdentity{Name: job.Name, UID: string(job.UID)})
	if err != nil || observation.Phase != buildworker.PhaseSucceeded || observation.Repository != execution.DestinationRepository {
		t.Fatalf("successful observation = %#v, %v", observation, err)
	}

	pod.Status.ContainerStatuses[0].State.Terminated.Message = `{"repository":"registry.invalid/forged","digest":"not-a-digest"}`
	invalidClient := fake.NewClientset(job, pod)
	invalid, err := newAdapter(t, invalidClient).Observe(context.Background(), execution,
		buildworker.ExecutionIdentity{Name: job.Name, UID: string(job.UID)})
	if err != nil || invalid.Phase != buildworker.PhaseUnknown || invalid.ErrorCode != "build_result_invalid" {
		t.Fatalf("invalid result observation = %#v, %v", invalid, err)
	}
}

// 恢复观察不能因为数据库尚未记录 UID，就信任同名但归属或输入摘要不同的 Job。
func TestBuildAdapterObserveRejectsUnownedJobWithoutRecordedUID(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*batchv1.Job)
	}{
		{
			name: "ownership label mismatch",
			mutate: func(job *batchv1.Job) {
				job.Labels[buildkube.BuildIDLabel] = uuid.NewString()
			},
		},
		{
			name: "input digest mismatch",
			mutate: func(job *batchv1.Job) {
				job.Annotations[buildkube.InputDigestAnnotation] = "sha256:" + strings.Repeat("f", 64)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution := buildExecution()
			adapter := newAdapter(t, fake.NewClientset())
			job := adapter.RenderJob(execution)
			job.UID = types.UID("foreign-job-uid")
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			test.mutate(job)

			observation, err := newAdapter(t, fake.NewClientset(job)).Observe(
				context.Background(), execution, buildworker.ExecutionIdentity{Name: job.Name})
			if err != nil || observation.Phase != buildworker.PhaseUnknown ||
				observation.ErrorCode != "build_job_ownership_conflict" {
				t.Fatalf("unowned job observation = %#v, %v", observation, err)
			}
		})
	}
}

func TestBuildAdapterTranslatesPlatformControlledFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*corev1.Pod)
		wantCode    string
		disposition string
	}{
		{
			name: "source fetch", wantCode: "source_fetch_failed", disposition: buildoperation.Retryable,
			mutate: func(pod *corev1.Pod) {
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "source", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 128},
				}}}
			},
		},
		{
			name: "registry unavailable", wantCode: "registry_unavailable", disposition: buildoperation.Retryable,
			mutate: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "buildkit", State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: `{"error_code":"registry_unavailable"}`},
				}}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			execution := buildExecution()
			adapter := newAdapter(t, fake.NewClientset())
			job := adapter.RenderJob(execution)
			job.UID = types.UID("failed-job-uid")
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "failed-pod", Namespace: job.Namespace,
				Labels: map[string]string{"job-name": job.Name}}}
			test.mutate(pod)
			observation, err := newAdapter(t, fake.NewClientset(job, pod)).Observe(
				context.Background(), execution, buildworker.ExecutionIdentity{Name: job.Name, UID: string(job.UID)})
			if err != nil || observation.Phase != buildworker.PhaseFailed || observation.ErrorCode != test.wantCode ||
				observation.Disposition != test.disposition {
				t.Fatalf("failure observation = %#v, %v", observation, err)
			}
		})
	}
}

func TestBuildAdapterCancellationUsesJobUID(t *testing.T) {
	execution := buildExecution()
	adapter := newAdapter(t, fake.NewClientset())
	job := adapter.RenderJob(execution)
	job.UID = "cancel-job-uid"
	client := fake.NewClientset(job)
	observation, err := newAdapter(t, client).Cancel(context.Background(), buildworker.ExecutionIdentity{Name: job.Name, UID: string(job.UID)})
	if err != nil || observation.Phase != buildworker.PhaseCanceled {
		t.Fatalf("cancel observation = %#v, %v", observation, err)
	}
	if _, err := client.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("job still exists after cancellation")
	}
}

func newAdapter(t *testing.T, client *fake.Clientset) *buildkube.Adapter {
	t.Helper()
	adapter, err := buildkube.New(client, buildkube.Config{
		Namespace: "orbitops-build", FieldManager: "orbitops-build-worker",
		GitImage:           "alpine/git:v2.49.1@sha256:" + strings.Repeat("a", 64),
		BuildkitImage:      "moby/buildkit:v0.33.0-rootless@sha256:" + strings.Repeat("b", 64),
		RegistrySecretName: "orbitops-registry", ActiveDeadline: 20 * time.Minute,
		TTL: time.Hour, CPU: "1", Memory: "1Gi", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func buildExecution() buildworker.BuildExecution {
	return buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(), RepositoryURL: "https://github.com/example/demo.git",
		SourceCommit: strings.Repeat("c", 40), DockerfilePath: "Dockerfile", ContextPath: ".",
		Platform: "linux/amd64", DestinationRepository: "registry.example/orbitops/demo", InputDigest: "sha256:" + strings.Repeat("d", 64),
	}
}
