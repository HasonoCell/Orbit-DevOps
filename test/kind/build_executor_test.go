package kind_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/buildkube"
	"github.com/HasonoCell/OrbitOps/internal/buildworker"
	"github.com/google/uuid"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	kindBuildGitImage = "alpine/git:v2.49.1@sha256:c0280cf9572316299b08544065d3bf35db65043d5e3963982ec50647d2746e26"
	kindBuildkitImage = "moby/buildkit:v0.33.0-rootless@sha256:80b15f0735e87bab7bf59ec4d695dfb4a7cfb25521cf56dc75d6f256285b63ef"
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
		observation, err = adapter.Observe(ctx, identity)
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
