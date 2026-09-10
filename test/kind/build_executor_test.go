package kind_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/builddispatch"
	"github.com/HasonoCell/OrbitOps/internal/buildkube"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/google/uuid"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kindBuildGitImage   = "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"
	kindBuildkitImage   = "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"
	kindBuildRepository = "https://github.com/nginxinc/NGINX-Demos.git"
	kindBuildCommit     = "611fa05748a4031841e5607cd3069288b0aa9973"
	kindBuildContext    = "nginx-hello-nonroot/plain-text-version"
)

// TestKindBuildExecutor 通过真实 Git Fetch、rootless BuildKit Job 和本地 Registry 验证执行边界。
func TestKindBuildExecutor(t *testing.T) {
	if os.Getenv("ORBITOPS_KIND_BUILD_E2E") != "1" {
		t.Skip("set ORBITOPS_KIND_BUILD_E2E=1 to run the real source build acceptance")
	}
	registryHost := os.Getenv("ORBITOPS_KIND_BUILD_REGISTRY")
	if registryHost == "" {
		t.Fatal("ORBITOPS_KIND_BUILD_REGISTRY is required")
	}
	buildNamespace := environmentOrDefault("ORBITOPS_BUILD_NAMESPACE", "orbitops-s4-build")
	adapter, err := buildkube.NewVerifiedLocalAdapter(context.Background(), clientcmd.RecommendedHomeFile, kindContext, buildkube.Config{
		Namespace: buildNamespace, FieldManager: "orbitops-build-worker",
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
		DestinationRepository: registryHost + "/orbitops/kind/" + uuid.NewString(),
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

// TestKindSourceToReleaseControlPlane 验证 Commit 经 API、队列和 Build Job 生成 Artifact，随后由既有 Release 链路发布。
func TestKindSourceToReleaseControlPlane(t *testing.T) {
	if os.Getenv("ORBITOPS_KIND_BUILD_E2E") != "1" {
		t.Skip("set ORBITOPS_KIND_BUILD_E2E=1 to run the real source-to-release acceptance")
	}
	registryHost := os.Getenv("ORBITOPS_KIND_BUILD_REGISTRY")
	if registryHost == "" {
		t.Fatal("ORBITOPS_KIND_BUILD_REGISTRY is required")
	}
	releaseAdapter, client := newKindAdapter(t)
	environment := newKindControlPlane(t, releaseAdapter)
	buildAdapter := newKindBuildAdapter(t)

	operations := buildoperation.New(environment.runner.db)
	builds := build.New(environment.runner.db, build.Config{
		AllowedGitHosts: []string{"github.com"}, Platform: environmentOrDefault("ORBITOPS_KIND_BUILD_PLATFORM", "linux/amd64"),
		RegistryHost: registryHost, RegistryPrefix: "orbitops",
	}, operations, projectauth.New(environment.runner.db))
	runner, err := buildworker.New(buildworker.Config{
		WorkerID: "kind-build-worker", LeaseDuration: 30 * time.Second,
		BuildTimeout: 7 * time.Minute, PollInterval: 250 * time.Millisecond,
	}, operations, builds, buildAdapter)
	if err != nil {
		t.Fatalf("create build runner: %v", err)
	}
	service, err := builddispatch.New(builddispatch.Config{
		RedisAddress: environment.runner.address, Queue: "orbitops-build-kind", Concurrency: 1,
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

	projectResponse := environment.postJSON(t, "/api/v1/projects", "source-to-release-project",
		`{"name":"Source To Release","slug":"source-to-release"}`)
	projectID := decodeID(t, projectResponse, "project")
	applicationResponse := environment.postJSON(t, "/api/v1/projects/"+projectID+"/applications", "source-to-release-application",
		`{"name":"Source To Release","slug":"source-to-release"}`)
	applicationID := decodeID(t, applicationResponse, "application")
	buildResponse := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/builds", "source-to-release-build",
		fmt.Sprintf(`{"repositoryUrl":%q,"sourceCommit":%q,"dockerfilePath":"%s/Dockerfile","contextPath":%q}`,
			kindBuildRepository, kindBuildCommit, kindBuildContext, kindBuildContext))
	defer buildResponse.Body.Close()
	var acceptance sourceBuildAcceptance
	if err := json.NewDecoder(buildResponse.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode build acceptance: %v", err)
	}
	completed := eventuallySourceBuild(t, environment, acceptance.Build.ID, 8*time.Minute)
	if completed.BuildOperation.Status != string(buildoperation.StatusSucceeded) || completed.ImageArtifact == nil {
		t.Fatalf("source build = %#v, want succeeded with artifact", completed)
	}
	assertRegistryManifest(t, buildworker.BuildExecution{
		BuildID: uuid.MustParse(completed.Build.ID), DestinationRepository: completed.Build.DestinationRepository,
	}, completed.ImageArtifact.Digest)

	targetResponse := environment.postJSON(t, "/api/v1/applications/"+applicationID+"/deployment-targets", "source-to-release-target",
		`{"stage":"development","replicas":1,"containerPort":8080}`)
	targetID := decodeID(t, targetResponse, "target")
	cleanupResources(t, client, uuid.MustParse(targetID))
	releaseResponse := environment.postJSON(t, "/api/v1/deployment-targets/"+targetID+"/releases", "source-to-release-release",
		fmt.Sprintf(`{"imageReference":%q,"imageArtifactId":%q}`, completed.ImageArtifact.ImageReference, completed.ImageArtifact.ID))
	release := decodeReleaseAcceptance(t, releaseResponse)
	if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
		t.Fatalf("publish built artifact = %v, error = %v", processed, err)
	}
	operation := environment.getReleaseOperation(t, release.ReleaseOperationID)
	if operation.Status != releaseoperation.StatusSucceeded {
		t.Fatalf("built artifact release operation = %#v", operation)
	}
	report := environment.getReleaseDiagnostics(t, release.ReleaseID)
	if report.RuntimeReleaseRelation != string(diagnostics.RuntimeReleaseMatches) ||
		report.WorkloadObservation.Deployment == nil || report.WorkloadObservation.Deployment.ReadyReplicas != 1 {
		t.Fatalf("built artifact diagnostics = %#v", report)
	}
}

type sourceBuildAcceptance struct {
	Build struct {
		ID                    string `json:"id"`
		DestinationRepository string `json:"destinationRepository"`
	} `json:"build"`
	BuildOperation struct {
		Status string `json:"status"`
	} `json:"buildOperation"`
	ImageArtifact *struct {
		ID             string `json:"id"`
		Digest         string `json:"digest"`
		ImageReference string `json:"imageReference"`
	} `json:"imageArtifact"`
}

func eventuallySourceBuild(t *testing.T, environment *kindControlPlane, buildID string, timeout time.Duration) sourceBuildAcceptance {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var current sourceBuildAcceptance
		environment.getJSON(t, "/api/v1/builds/"+buildID, &current)
		switch current.BuildOperation.Status {
		case string(buildoperation.StatusSucceeded), string(buildoperation.StatusFailed), string(buildoperation.StatusCanceled), string(buildoperation.StatusAttentionRequired):
			return current
		}
		if time.Now().After(deadline) {
			t.Fatalf("build %s did not reach a terminal status", buildID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func newKindBuildAdapter(t *testing.T) *buildkube.Adapter {
	t.Helper()
	adapter, err := buildkube.NewVerifiedLocalAdapter(context.Background(), clientcmd.RecommendedHomeFile, kindContext, buildkube.Config{
		Namespace:    environmentOrDefault("ORBITOPS_BUILD_NAMESPACE", "orbitops-s4-build"),
		FieldManager: "orbitops-build-worker", GitImage: kindBuildGitImage, BuildkitImage: kindBuildkitImage,
		RegistryInsecure: true, DockerHubMirror: os.Getenv("ORBITOPS_KIND_BUILD_REGISTRY"),
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
	registryAPI := environmentOrDefault("ORBITOPS_KIND_BUILD_REGISTRY_API", "http://127.0.0.1:5001")
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
