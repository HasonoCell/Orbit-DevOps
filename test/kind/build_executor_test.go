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
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	kindBuildGitImage   = "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"
	kindBuildkitImage   = "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"
	kindBuildRepository = "https://github.com/nginxinc/NGINX-Demos.git"
	kindBuildCommit     = "611fa05748a4031841e5607cd3069288b0aa9973"
	kindBuildContext    = "nginx-hello-nonroot/plain-text-version"
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
