package kind_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/builddispatch"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildkube"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/diagnostics"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	"github.com/HasonoCell/Orbit-DevOps/internal/platform/kubeconnection"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	kindBuildGitImage        = "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"
	kindBuildkitImage        = "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"
	kindCachedShellImage     = "redis:7-alpine@sha256:ff02b58f971e7d7d156a1267e283fcbbeee91773b6aa36c49dac28ecfe28eadf"
	kindCachedPostgresImage  = "postgres:17-alpine"
	kindBuildRepository      = "https://github.com/nginxinc/NGINX-Demos.git"
	kindBuildCommit          = "611fa05748a4031841e5607cd3069288b0aa9973"
	kindBuildContext         = "nginx-hello-nonroot/plain-text-version"
	kindBuildBudgetContext   = "kind-orbit-upgrade-20261009"
	kindBuildBudgetNamespace = "orbit-build-budget-20261009"
)

type kindSourceInspector struct{}

func (kindSourceInspector) Resolve(_ context.Context, request pipeline.SourceRequest) (pipeline.SourceIdentity, error) {
	if request.RepositoryURL != kindBuildRepository || request.Branch != "main" {
		return pipeline.SourceIdentity{}, pipeline.ErrSourceNotFound
	}
	return kindSourceIdentity(), nil
}

func (kindSourceInspector) Head(_ context.Context, request pipeline.HeadRequest) (pipeline.SourceIdentity, error) {
	identity := kindSourceIdentity()
	if request.RepositoryID != identity.RepositoryID || request.OwnerID != identity.OwnerID || request.GitRef != identity.GitRef {
		return pipeline.SourceIdentity{}, pipeline.ErrSourceOwnerChanged
	}
	return identity, nil
}

func kindSourceIdentity() pipeline.SourceIdentity {
	return pipeline.SourceIdentity{RepositoryID: 101, OwnerID: 202, RepositoryName: "nginxinc/NGINX-Demos",
		RepositoryURL: kindBuildRepository, GitRef: "refs/heads/main", HeadCommit: kindBuildCommit}
}

func TestBuildJobUsesPlatformDiskBudgetAndRejectsUnsafeValues(t *testing.T) {
	config := buildDiskBudgetConfig()
	adapter, err := buildkube.New(fake.NewClientset(), config)
	if err != nil {
		t.Fatal(err)
	}
	execution := buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(), RepositoryURL: "https://github.com/example/demo.git",
		SourceCommit: strings.Repeat("a", 40), DockerfilePath: "Dockerfile", ContextPath: ".",
		Platform: "linux/amd64", DestinationRepository: "registry.example/orbit/demo",
		InputDigest: "sha256:" + strings.Repeat("b", 64),
	}
	job := adapter.RenderJob(execution)
	wantVolumes := map[string]resource.Quantity{
		"workspace":      resource.MustParse("1Gi"),
		"buildkit-state": resource.MustParse("8Gi"),
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		want, controlled := wantVolumes[volume.Name]
		if !controlled {
			continue
		}
		if volume.EmptyDir == nil || volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(want) != 0 {
			t.Fatalf("volume %s size limit = %#v, want %s", volume.Name, volume.EmptyDir, want.String())
		}
		delete(wantVolumes, volume.Name)
	}
	if len(wantVolumes) != 0 {
		t.Fatalf("controlled build volumes not rendered: %#v", wantVolumes)
	}
	containers := append([]corev1.Container{}, job.Spec.Template.Spec.InitContainers...)
	containers = append(containers, job.Spec.Template.Spec.Containers...)
	for _, container := range containers {
		request := container.Resources.Requests[corev1.ResourceEphemeralStorage]
		limit := container.Resources.Limits[corev1.ResourceEphemeralStorage]
		if request.Cmp(resource.MustParse("1Gi")) != 0 || limit.Cmp(resource.MustParse("10Gi")) != 0 {
			t.Fatalf("container %s ephemeral storage = %s/%s", container.Name, request.String(), limit.String())
		}
	}

	tests := []struct {
		name   string
		mutate func(*buildkube.Config)
	}{
		{name: "malformed source", mutate: func(config *buildkube.Config) { config.SourceStorageLimit = "many" }},
		{name: "negative buildkit", mutate: func(config *buildkube.Config) { config.BuildkitStorageLimit = "-1Gi" }},
		{name: "request exceeds limit", mutate: func(config *buildkube.Config) { config.EphemeralStorageRequest = "11Gi" }},
		{name: "volumes exceed limit", mutate: func(config *buildkube.Config) { config.SourceStorageLimit = "3Gi" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsafe := buildDiskBudgetConfig()
			test.mutate(&unsafe)
			if adapter, err := buildkube.New(fake.NewClientset(), unsafe); err == nil || adapter != nil {
				t.Fatalf("unsafe disk budget was accepted: %#v", unsafe)
			}
		})
	}
}

func buildDiskBudgetConfig() buildkube.Config {
	return buildkube.Config{
		Namespace: "orbit-devops-build", FieldManager: "orbit-devops-build-worker",
		GitImage: kindBuildGitImage, BuildkitImage: kindBuildkitImage,
		ActiveDeadline: 5 * time.Minute, TTL: time.Hour, CPU: "1", Memory: "1Gi",
		SourceStorageLimit: "1Gi", BuildkitStorageLimit: "8Gi",
		EphemeralStorageRequest: "1Gi", EphemeralStorageLimit: "10Gi",
	}
}

// TestKindBuildJobEventuallyStopsOverBudget 证明 kubelet 最终处置超预算 Job，且同节点
// PG/app 的 Ready、UID、restart count 与合成健康查询不变；它不声称磁盘配额同步生效。
func TestKindBuildJobEventuallyStopsOverBudget(t *testing.T) {
	if os.Getenv("ORBIT_DEVOPS_KIND_BUILD_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_KIND_BUILD_E2E=1 to run the build disk budget acceptance")
	}
	if kindContext != kindBuildBudgetContext {
		t.Fatalf("Kind context must equal %s", kindBuildBudgetContext)
	}
	buildNamespace := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_BUILD_NAMESPACE"))
	if buildNamespace != kindBuildBudgetNamespace {
		t.Fatalf("ORBIT_DEVOPS_BUILD_NAMESPACE must equal %s", kindBuildBudgetNamespace)
	}
	expectedNodeUID := strings.TrimSpace(os.Getenv("ORBIT_DEVOPS_KIND_NODE_UID"))
	if expectedNodeUID == "" {
		t.Fatal("ORBIT_DEVOPS_KIND_NODE_UID is required")
	}
	boundary := verifiedKindBoundaryForContext(t, expectedNodeUID)
	client := boundary.client
	createKindBuildBudgetNamespace(t, client)
	protectedBefore := createProtectedBuildSentinels(t, boundary, buildNamespace)
	config := buildDiskBudgetConfig()
	config.Namespace = buildNamespace
	// 容量验收不做真实构建；复用仓库固定且已导入 Kind 的小型镜像，避免公网拉取。
	config.GitImage = kindCachedShellImage
	config.BuildkitImage = kindCachedShellImage
	config.SourceStorageLimit = "8Mi"
	config.BuildkitStorageLimit = "8Mi"
	config.EphemeralStorageRequest = "1Mi"
	config.EphemeralStorageLimit = "32Mi"
	config.PollInterval = 250 * time.Millisecond
	adapter, err := buildkube.NewVerifiedLocalAdapter(
		context.Background(), kindKubeconfigPath(), kindContext, config,
	)
	if err != nil {
		t.Fatalf("create verified build adapter: %v", err)
	}
	execution := buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(), RepositoryURL: "https://invalid.example/not-used.git",
		SourceCommit: strings.Repeat("c", 40), DockerfilePath: "Dockerfile", ContextPath: ".",
		Platform: "linux/amd64", DestinationRepository: "registry.invalid/orbit/disk-budget",
		InputDigest: "sha256:" + strings.Repeat("d", 64),
	}
	job := adapter.RenderJob(execution)
	// 测试只替换工作负载并固定已核验节点，不改变生产预算字段、身份或安全上下文。
	job.Spec.Template.Spec.InitContainers[0].Args = []string{`printf 'ready\n' > /workspace/source-ready`}
	job.Spec.Template.Spec.Containers[0].Args = []string{
		`dd if=/dev/zero of=/home/user/.local/share/buildkit/oversize bs=1M count=64; sync; sleep 180`,
	}
	job.Spec.Template.Spec.NodeName = boundary.nodeName
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	created, err := client.BatchV1().Jobs(buildNamespace).Create(ctx, job, metav1.CreateOptions{FieldManager: config.FieldManager})
	if err != nil {
		t.Fatalf("create over-budget build Job: %v", err)
	}
	identity := buildworker.ExecutionIdentity{Name: created.Name, UID: string(created.UID)}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = adapter.Cancel(cleanupContext, identity)
	})

	for {
		observation, observeErr := adapter.Observe(ctx, execution, identity)
		if observeErr != nil {
			t.Fatalf("observe over-budget build Job: %v", observeErr)
		}
		if observation.Phase == buildworker.PhaseFailed {
			if observation.ErrorCode != "executor_evicted" {
				t.Fatalf("over-budget failure = %#v", observation)
			}
			pods, listErr := client.CoreV1().Pods(buildNamespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + created.Name})
			if listErr != nil || len(pods.Items) == 0 {
				t.Fatalf("read over-budget Pod evidence: %v / %d pods", listErr, len(pods.Items))
			}
			pod := pods.Items[len(pods.Items)-1]
			t.Logf("over-budget Pod reached %s/%s: %s", pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
			if pod.Spec.NodeName != boundary.nodeName || pod.Status.Reason != "Evicted" ||
				!strings.Contains(pod.Status.Message, "buildkit-state") {
				t.Fatalf("over-budget Pod evidence = %s/%s: %s", pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
			}
			assertProtectedBuildSentinelsUnchanged(t, boundary, buildNamespace, protectedBefore)
			return
		}
		if observation.Phase != buildworker.PhaseRunning {
			t.Fatalf("over-budget build escaped as %#v", observation)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("over-budget build was not stopped: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

type verifiedKindBoundary struct {
	client   kubernetes.Interface
	nodeName string
}

// verifiedKindBoundaryForContext 只为显式的 loopback Kind context 交付可写 client；
// 调用者还必须把写入限制在自己创建并按 UID 清理的任务 Namespace。
func verifiedKindBoundaryForContext(t *testing.T, expectedNodeUID string) verifiedKindBoundary {
	t.Helper()
	connection := kubeconnection.Config{KubeconfigPath: kindKubeconfigPath(), Context: kindContext}
	restConfig, err := connection.Load()
	if err != nil {
		t.Fatalf("load loopback Kind context: %v", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatalf("create Kind client: %v", err)
	}
	nodes, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil || len(nodes.Items) != 1 {
		t.Fatalf("read unique Kind node: %v / %d nodes", err, len(nodes.Items))
	}
	node := nodes.Items[0]
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if string(node.UID) != expectedNodeUID || !strings.HasPrefix(node.Name, strings.TrimPrefix(kindContext, "kind-")+"-") || !ready {
		t.Fatal("Kind node identity or readiness does not match the explicit test boundary")
	}
	return verifiedKindBoundary{client: client, nodeName: node.Name}
}

// createKindBuildBudgetNamespace 创建本轮唯一任务 Namespace；已存在或 UID 改变时拒绝接管和清理。
func createKindBuildBudgetNamespace(t *testing.T, client kubernetes.Interface) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.CoreV1().Namespaces().Get(ctx, kindBuildBudgetNamespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("build budget Namespace must not already exist: %v", err)
	}
	created, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: kindBuildBudgetNamespace,
		Labels: map[string]string{
			kubeconnection.ManagedByLabel: kubeconnection.ManagedByValue,
			"orbit-devops.dev/test":       "build-budget-20261009",
		},
	}}, metav1.CreateOptions{FieldManager: "orbit-build-budget-acceptance"})
	if err != nil {
		t.Fatalf("create build budget Namespace: %v", err)
	}
	t.Cleanup(func() {
		current, readErr := client.CoreV1().Namespaces().Get(context.Background(), created.Name, metav1.GetOptions{})
		if readErr != nil || current.UID != created.UID || current.Labels["orbit-devops.dev/test"] != "build-budget-20261009" {
			t.Errorf("refuse to delete replaced build budget Namespace: %v", readErr)
			return
		}
		uid := created.UID
		policy := metav1.DeletePropagationForeground
		deleteErr := client.CoreV1().Namespaces().Delete(context.Background(), created.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy,
		})
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			t.Errorf("delete build budget Namespace: %v", deleteErr)
			return
		}
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			_, getErr := client.CoreV1().Namespaces().Get(context.Background(), created.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Error("build budget Namespace was not deleted within one minute")
	})
	if _, err := client.CoreV1().ServiceAccounts(created.Name).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "orbit-devops-build-executor", Namespace: created.Name},
	}, metav1.CreateOptions{FieldManager: "orbit-build-budget-acceptance"}); err != nil {
		t.Fatalf("create build executor ServiceAccount: %v", err)
	}
}

type protectedBuildSentinelState struct {
	podUIDs  map[string]string
	restarts map[string]int32
}

// createProtectedBuildSentinels 在同一节点启动合成 PG 与 app，不使用真实数据或外部镜像拉取。
func createProtectedBuildSentinels(t *testing.T, boundary verifiedKindBoundary, namespace string) protectedBuildSentinelState {
	t.Helper()
	postgres := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "postgres-sentinel", Namespace: namespace,
		Labels: map[string]string{"orbit-devops.dev/test": "protected-postgres"}}, Spec: corev1.PodSpec{
		NodeName: boundary.nodeName, RestartPolicy: corev1.RestartPolicyAlways,
		Containers: []corev1.Container{{Name: "postgres", Image: kindCachedPostgresImage,
			ImagePullPolicy: corev1.PullNever,
			Env:             []corev1.EnvVar{{Name: "POSTGRES_HOST_AUTH_METHOD", Value: "trust"}, {Name: "POSTGRES_DB", Value: "sentinel"}},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"pg_isready", "-U", "postgres", "-d", "sentinel"},
			}}, PeriodSeconds: 1, FailureThreshold: 30},
		}},
	}}
	app := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "app-sentinel", Namespace: namespace,
		Labels: map[string]string{"orbit-devops.dev/test": "protected-app"}}, Spec: corev1.PodSpec{
		NodeName: boundary.nodeName, RestartPolicy: corev1.RestartPolicyAlways,
		Containers: []corev1.Container{{Name: "app", Image: kindCachedShellImage,
			ImagePullPolicy: corev1.PullNever, Args: []string{"redis-server", "--save", "", "--appendonly", "no"},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"redis-cli", "ping"},
			}}, PeriodSeconds: 1, FailureThreshold: 30},
		}},
	}}
	for _, pod := range []*corev1.Pod{postgres, app} {
		if _, err := boundary.client.CoreV1().Pods(namespace).Create(context.Background(), pod,
			metav1.CreateOptions{FieldManager: "orbit-build-budget-acceptance"}); err != nil {
			t.Fatalf("create protected %s: %v", pod.Name, err)
		}
	}
	state := awaitProtectedBuildSentinels(t, boundary, namespace)
	assertProtectedBuildSentinelCommands(t, boundary, namespace)
	return state
}

func awaitProtectedBuildSentinels(t *testing.T, boundary verifiedKindBoundary, namespace string) protectedBuildSentinelState {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		state := protectedBuildSentinelState{podUIDs: map[string]string{}, restarts: map[string]int32{}}
		ready := true
		for _, name := range []string{"postgres-sentinel", "app-sentinel"} {
			pod, err := boundary.client.CoreV1().Pods(namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil || pod.Spec.NodeName != boundary.nodeName || pod.Status.Phase != corev1.PodRunning ||
				len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready {
				ready = false
				break
			}
			state.podUIDs[name] = string(pod.UID)
			state.restarts[name] = pod.Status.ContainerStatuses[0].RestartCount
		}
		if ready {
			return state
		}
		time.Sleep(time.Second)
	}
	t.Fatal("protected PostgreSQL and app sentinels did not become Ready on the verified node")
	return protectedBuildSentinelState{}
}

func assertProtectedBuildSentinelsUnchanged(t *testing.T, boundary verifiedKindBoundary, namespace string,
	want protectedBuildSentinelState,
) {
	t.Helper()
	got := awaitProtectedBuildSentinels(t, boundary, namespace)
	for _, name := range []string{"postgres-sentinel", "app-sentinel"} {
		if got.podUIDs[name] != want.podUIDs[name] || got.restarts[name] != want.restarts[name] {
			t.Fatalf("protected %s identity/restarts changed across build eviction", name)
		}
	}
	assertProtectedBuildSentinelCommands(t, boundary, namespace)
	t.Log("protected PostgreSQL/app remained Ready on the same node with unchanged UID/restarts and constant health queries")
}

func assertProtectedBuildSentinelCommands(t *testing.T, boundary verifiedKindBoundary, namespace string) {
	t.Helper()
	execProtectedBuildSentinel(t, boundary, namespace, "postgres-sentinel", "postgres",
		[]string{"psql", "-XAtq", "-U", "postgres", "-d", "sentinel", "-c", "SELECT 424242"}, "424242")
	execProtectedBuildSentinel(t, boundary, namespace, "app-sentinel", "app",
		[]string{"redis-cli", "ping"}, "PONG")
}

func execProtectedBuildSentinel(t *testing.T, boundary verifiedKindBoundary, namespace, pod, container string,
	command []string, want string,
) {
	t.Helper()
	current, err := boundary.client.CoreV1().Pods(namespace).Get(context.Background(), pod, metav1.GetOptions{})
	if err != nil || current.Spec.NodeName != boundary.nodeName {
		t.Fatalf("protected %s left the verified node", pod)
	}
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatalf("find kubectl for protected sentinel: %v", err)
	}
	arguments := []string{"--kubeconfig", kindKubeconfigPath(), "--context", kindContext,
		"-n", namespace, "exec", pod, "-c", container, "--"}
	process := exec.Command(kubectl, append(arguments, command...)...)
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	if err := process.Run(); err != nil || strings.TrimSpace(stdout.String()) != want || stderr.Len() != 0 {
		t.Fatalf("protected %s health command failed", pod)
	}
}

// TestKindBuildExecutor 通过真实 Git Fetch、rootless BuildKit Job 和本地 Registry 验证执行边界。
func TestKindBuildExecutor(t *testing.T) {
	if os.Getenv("ORBIT_DEVOPS_KIND_BUILD_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_KIND_BUILD_E2E=1 to run the real source build acceptance")
	}
	registryHost := os.Getenv("ORBIT_DEVOPS_KIND_BUILD_REGISTRY")
	if registryHost == "" {
		t.Fatal("ORBIT_DEVOPS_KIND_BUILD_REGISTRY is required")
	}
	buildNamespace := environmentOrDefault("ORBIT_DEVOPS_BUILD_NAMESPACE", "orbit-devops-s4-build")
	adapter, err := buildkube.NewVerifiedLocalAdapter(context.Background(), kindKubeconfigPath(), kindContext, buildkube.Config{
		Namespace: buildNamespace, FieldManager: "orbit-devops-build-worker",
		GitImage: kindBuildGitImage, BuildkitImage: kindBuildkitImage, RegistryInsecure: true,
		ActiveDeadline: 5 * time.Minute, TTL: time.Hour, CPU: "1", Memory: "1Gi", PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create verified build adapter: %v", err)
	}

	execution := buildworker.BuildExecution{
		BuildOperationID: uuid.New(), BuildAttemptID: uuid.New(), BuildID: uuid.New(),
		ProjectID: uuid.New(), ApplicationID: uuid.New(),
		RepositoryURL:  "https://github.com/docker-library/hello-world.git",
		SourceCommit:   "522bcd2faf422c60b9d20e64d7cd6d56600aec97",
		DockerfilePath: "amd64/Dockerfile", ContextPath: "amd64", Platform: "linux/amd64",
		DestinationRepository: registryHost + "/orbit-devops/kind/" + uuid.NewString(),
		InputDigest:           "sha256:" + strings.Repeat("a", 64),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	identity, err := adapter.Start(ctx, execution)
	if err != nil {
		t.Fatalf("start build Job: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = adapter.Cancel(cleanupContext, identity)
	})

	var observation buildworker.ExecutionObservation
	for {
		observation, err = adapter.Observe(ctx, execution, identity)
		if err != nil {
			t.Fatalf("observe build Job: %v", err)
		}
		if observation.Phase != buildworker.PhaseRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("build Job did not finish: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if observation.Phase != buildworker.PhaseSucceeded || observation.Repository != execution.DestinationRepository ||
		!strings.HasPrefix(observation.Digest, "sha256:") {
		t.Fatalf("build observation = %#v", observation)
	}
	assertRegistryManifest(t, execution, observation.Digest)
}

// TestKindPushToReadyDelivery 验证真实签名 Push 经 Pipeline、BuildKit、OCI 和 Release 最终达到 Ready。
func TestKindPushToReadyDelivery(t *testing.T) {
	if os.Getenv("ORBIT_DEVOPS_KIND_BUILD_E2E") != "1" {
		t.Skip("set ORBIT_DEVOPS_KIND_BUILD_E2E=1 to run the real source-to-release acceptance")
	}
	registryHost := os.Getenv("ORBIT_DEVOPS_KIND_BUILD_REGISTRY")
	if registryHost == "" {
		t.Fatal("ORBIT_DEVOPS_KIND_BUILD_REGISTRY is required")
	}
	releaseAdapter, client := newKindAdapter(t)
	environment := newKindControlPlane(t, releaseAdapter)
	buildAdapter := newKindBuildAdapter(t)

	operations := buildoperation.New(environment.runner.db)
	builds := build.New(environment.runner.db, build.Config{
		AllowedGitHosts: []string{"github.com"}, Platform: environmentOrDefault("ORBIT_DEVOPS_KIND_BUILD_PLATFORM", "linux/amd64"),
		RegistryHost: registryHost, RegistryPrefix: "orbit-devops",
	}, operations, projectauth.New(environment.runner.db, nil))
	runner, err := buildworker.New(buildworker.Config{
		WorkerID: "kind-build-worker", LeaseDuration: 30 * time.Second,
		BuildTimeout: 7 * time.Minute, PollInterval: 250 * time.Millisecond,
	}, operations, builds, buildAdapter)
	if err != nil {
		t.Fatalf("create build runner: %v", err)
	}
	service, err := builddispatch.New(builddispatch.Config{
		RedisAddress: environment.runner.address, Queue: "orbit-devops-build-kind", Concurrency: 1,
		PollInterval: 50 * time.Millisecond, RepairInterval: 100 * time.Millisecond,
		ConsumptionGrace: time.Second, TaskTimeout: 8 * time.Minute, ShutdownTimeout: 5 * time.Second,
	}, operations, runner)
	if err != nil {
		t.Fatalf("create build dispatch service: %v", err)
	}
	serviceContext, stopService := context.WithCancel(context.Background())
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service.Run(serviceContext) }()
	t.Cleanup(func() {
		stopService()
		select {
		case err := <-serviceDone:
			if err != nil {
				t.Errorf("stop build dispatch service: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("build dispatch service did not stop")
		}
	})

	pipelineModule := pipeline.New(environment.runner.db, pipeline.Config{Platform: environmentOrDefault("ORBIT_DEVOPS_KIND_BUILD_PLATFORM", "linux/amd64")}, builds, environment.releases, projectauth.New(environment.runner.db, nil), kindSourceInspector{})
	eventService, err := internalevent.NewService(internalevent.Config{
		RedisAddress: environment.runner.address, Queue: "orbit-devops-pipeline-kind", Concurrency: 2,
		PollInterval: 50 * time.Millisecond, ConsumptionGrace: time.Second,
		TaskTimeout: 30 * time.Second, ShutdownTimeout: 5 * time.Second,
	}, internalevent.New(environment.runner.db), pipelineModule)
	if err != nil {
		t.Fatalf("create pipeline event service: %v", err)
	}
	eventContext, stopEvents := context.WithCancel(context.Background())
	eventDone := make(chan error, 1)
	go func() { eventDone <- eventService.Run(eventContext) }()
	t.Cleanup(func() {
		stopEvents()
		select {
		case err := <-eventDone:
			if err != nil {
				t.Errorf("stop pipeline event service: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("pipeline event service did not stop")
		}
	})

	projectResponse := environment.postJSON(t, "/api/v1/projects", "push-to-ready-project",
		`{"name":"Push To Ready","slug":"push-to-ready"}`)
	projectID := decodeID(t, projectResponse, "project")
	applicationResponse := environment.postJSON(t, "/api/v1/projects/"+projectID+"/applications", "push-to-ready-application",
		`{"name":"Push To Ready","slug":"push-to-ready"}`)
	applicationID := decodeID(t, applicationResponse, "application")
	targetResponse := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/deployment-targets", "push-to-ready-target",
		`{"stage":"development","replicas":1,"containerPort":8080}`)
	targetID := decodeID(t, targetResponse, "target")
	cleanupResources(t, client, uuid.MustParse(targetID))
	pipelineResponse := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/delivery-pipelines", "push-to-ready-pipeline",
		fmt.Sprintf(`{"name":"main","endpointKey":"kind","repositoryUrl":%q,"branch":"main","dockerfilePath":"%s/Dockerfile","contextPath":%q,"mode":"auto_release","deploymentTargetId":%q}`,
			kindBuildRepository, kindBuildContext, kindBuildContext, targetID))
	defer pipelineResponse.Body.Close()
	var pipelineDocument struct {
		Pipeline struct {
			ID string `json:"id"`
		} `json:"pipeline"`
	}
	if err := json.NewDecoder(pipelineResponse.Body).Decode(&pipelineDocument); err != nil || pipelineDocument.Pipeline.ID == "" {
		t.Fatalf("decode pipeline: %#v, error = %v", pipelineDocument, err)
	}
	environment.postCommand(t, "/api/v1/delivery-pipelines/"+pipelineDocument.Pipeline.ID+"/enable", "push-to-ready-enable", http.StatusOK)
	postKindPush(t, environment, kindBuildCommit)
	completed := eventuallyAutomaticRelease(t, environment.runner.db, uuid.MustParse(pipelineDocument.Pipeline.ID), 8*time.Minute)
	assertRegistryManifest(t, buildworker.BuildExecution{BuildID: completed.BuildID, DestinationRepository: completed.Repository}, completed.Digest)
	if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
		t.Fatalf("publish automatic artifact = %v, error = %v", processed, err)
	}
	eventuallyDeliveryPhase(t, environment.runner.db, completed.RunID, "completed", 15*time.Second)
	operation := environment.getReleaseOperation(t, completed.ReleaseOperationID.String())
	if operation.Status != releaseoperation.StatusSucceeded {
		t.Fatalf("automatic release operation = %#v", operation)
	}
	report := environment.getReleaseDiagnostics(t, completed.ReleaseID.String())
	if report.RuntimeReleaseRelation != string(diagnostics.RuntimeReleaseMatches) ||
		report.WorkloadObservation.Deployment == nil || report.WorkloadObservation.Deployment.ReadyReplicas != 1 {
		t.Fatalf("automatic delivery diagnostics = %#v", report)
	}

	// 人工复用同一 Artifact/Digest 发布 production，两个 Target 的运行时资源不能互相覆盖。
	productionResponse := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/deployment-targets", "push-to-ready-production-target",
		`{"stage":"production","replicas":1,"containerPort":8080}`)
	productionID := decodeID(t, productionResponse, "production target")
	cleanupResources(t, client, uuid.MustParse(productionID))
	var artifactID uuid.UUID
	if err := environment.runner.db.Get(&artifactID, `SELECT image_artifact_id FROM delivery_runs WHERE id=$1`, completed.RunID); err != nil {
		t.Fatal(err)
	}
	imageReference := completed.Repository + "@" + completed.Digest
	manualResponse := environment.postJSON(t, "/api/v1/deployment-targets/"+productionID+"/releases", "push-to-ready-manual-production",
		fmt.Sprintf(`{"imageReference":%q,"imageArtifactId":%q}`, imageReference, artifactID.String()))
	manual := decodeReleaseAcceptance(t, manualResponse)
	if manual.TargetID != productionID || manual.ReleaseID == completed.ReleaseID.String() {
		t.Fatalf("manual production acceptance = %#v", manual)
	}
	if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
		t.Fatalf("publish production artifact = %v, error = %v", processed, err)
	}
	productionOperation := environment.getReleaseOperation(t, manual.ReleaseOperationID)
	productionReport := environment.getReleaseDiagnostics(t, manual.ReleaseID)
	if productionOperation.Status != releaseoperation.StatusSucceeded ||
		productionReport.RuntimeReleaseRelation != string(diagnostics.RuntimeReleaseMatches) ||
		productionReport.WorkloadObservation.Deployment == nil ||
		productionReport.WorkloadObservation.Deployment.ReadyReplicas != 1 {
		t.Fatalf("production operation/report = %#v / %#v", productionOperation, productionReport)
	}
	for _, targetID := range []string{targetID, productionID} {
		deployment, err := client.AppsV1().Deployments(kindNamespace).Get(context.Background(), kube.ResourceName(uuid.MustParse(targetID)), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read %s deployment: %v", targetID, err)
		}
		if deployment.Labels[kube.TargetIDLabel] != targetID || deployment.Spec.Template.Spec.Containers[0].Image != imageReference {
			t.Fatalf("deployment %s does not preserve target/digest: %#v", targetID, deployment)
		}
		service, err := client.CoreV1().Services(kindNamespace).Get(context.Background(), kube.ResourceName(uuid.MustParse(targetID)), metav1.GetOptions{})
		if err != nil || service.Labels[kube.TargetIDLabel] != targetID {
			t.Fatalf("service %s does not preserve target: %#v error=%v", targetID, service, err)
		}
	}
}

type automaticReleaseResult struct {
	RunID              uuid.UUID `db:"run_id"`
	BuildID            uuid.UUID `db:"build_id"`
	ReleaseID          uuid.UUID `db:"release_id"`
	ReleaseOperationID uuid.UUID `db:"release_operation_id"`
	Repository         string    `db:"destination_repository"`
	Digest             string    `db:"digest"`
}

func postKindPush(t *testing.T, environment *kindControlPlane, commit string) {
	t.Helper()
	payload := []byte(fmt.Sprintf(`{"ref":"refs/heads/main","before":"%s","after":%q,"forced":false,"deleted":false,"repository":{"id":101,"full_name":"nginxinc/NGINX-Demos","owner":{"id":202}}}`, strings.Repeat("0", 40), commit))
	mac := hmac.New(sha256.New, []byte("kind-webhook-secret"))
	_, _ = mac.Write(payload)
	request, err := http.NewRequest(http.MethodPost, environment.server.URL+"/api/v1/webhooks/github/kind", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-GitHub-Delivery", "kind-push-to-ready")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("accept Kind webhook status = %d", response.StatusCode)
	}
}

func eventuallyAutomaticRelease(t *testing.T, db *sqlx.DB, pipelineID uuid.UUID, timeout time.Duration) automaticReleaseResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var result automaticReleaseResult
		err := db.Get(&result, `SELECT dr.id AS run_id,dr.build_id,dr.release_id,
			ro.id AS release_operation_id,b.destination_repository,ia.digest
			FROM delivery_runs dr JOIN builds b ON b.id=dr.build_id
			JOIN image_artifacts ia ON ia.id=dr.image_artifact_id
			JOIN release_operations ro ON ro.release_id=dr.release_id
			WHERE dr.delivery_pipeline_id=$1 AND dr.phase='release_created'`, pipelineID)
		if err == nil {
			return result
		}
		var failed struct {
			Status    string  `db:"status"`
			ErrorCode *string `db:"error_code"`
		}
		if db.Get(&failed, `SELECT bo.status,bo.error_code FROM delivery_runs dr JOIN build_operations bo ON bo.build_id=dr.build_id WHERE dr.delivery_pipeline_id=$1`, pipelineID) == nil &&
			(failed.Status == "failed" || failed.Status == "canceled" || failed.Status == "attention_required") {
			t.Fatalf("automatic Build stopped at %s/%v", failed.Status, failed.ErrorCode)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("automatic delivery did not create Release before deadline")
	return automaticReleaseResult{}
}

func eventuallyDeliveryPhase(t *testing.T, db *sqlx.DB, runID uuid.UUID, phase string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current string
		if db.Get(&current, `SELECT phase FROM delivery_runs WHERE id=$1`, runID) == nil && current == phase {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("delivery run did not reach %s", phase)
}

func newKindBuildAdapter(t *testing.T) *buildkube.Adapter {
	t.Helper()
	adapter, err := buildkube.NewVerifiedLocalAdapter(context.Background(), kindKubeconfigPath(), kindContext, buildkube.Config{
		Namespace:    environmentOrDefault("ORBIT_DEVOPS_BUILD_NAMESPACE", "orbit-devops-s4-build"),
		FieldManager: "orbit-devops-build-worker", GitImage: kindBuildGitImage, BuildkitImage: kindBuildkitImage,
		RegistryInsecure: true, DockerHubMirror: os.Getenv("ORBIT_DEVOPS_KIND_BUILD_REGISTRY"),
		DockerHubMirrorInsecure: true, ActiveDeadline: 7 * time.Minute, TTL: time.Hour,
		CPU: "1", Memory: "1Gi", PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create verified build adapter: %v", err)
	}
	return adapter
}

// assertRegistryManifest 从 Registry API 再读一次 tag，证明产物不只存在于 Job 的返回文本中。
func assertRegistryManifest(t *testing.T, execution buildworker.BuildExecution, wantDigest string) {
	t.Helper()
	registryAPI := environmentOrDefault("ORBIT_DEVOPS_KIND_BUILD_REGISTRY_API", "http://127.0.0.1:5002")
	repository := strings.TrimPrefix(execution.DestinationRepository, strings.SplitN(execution.DestinationRepository, "/", 2)[0]+"/")
	request, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/v2/%s/manifests/build-%s", registryAPI, repository, execution.BuildID), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read registry manifest: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Docker-Content-Digest") != wantDigest {
		t.Fatalf("registry manifest status=%d digest=%q, want %q", response.StatusCode,
			response.Header.Get("Docker-Content-Digest"), wantDigest)
	}
}
