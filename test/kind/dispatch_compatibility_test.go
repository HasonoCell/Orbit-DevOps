package kind_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
)

// 显式传入固定 S2 基线编译的二进制，证明回退兼容性；不下载、不改写旧基线或清库。
func TestKindS2RollbackAndQueueReentry(t *testing.T) {
	apiBinary, workerBinary := os.Getenv("ORBIT_DEVOPS_S2_API_BINARY"), os.Getenv("ORBIT_DEVOPS_S2_WORKER_BINARY")
	if apiBinary == "" || workerBinary == "" {
		t.Skip("set fixed S2 API/Worker binary paths for schema rollback acceptance")
	}
	adapter, client := newKindAdapter(t)
	environment := newKindControlPlane(t, adapter)
	accepted := environment.acceptRelease(t, "compat-q1", readyImage)
	cleanupResources(t, client, uuid.MustParse(accepted.TargetID))
	ctx := context.Background()
	items, err := environment.operations.ReserveDispatches(ctx, 100, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	claim, err := environment.operations.ClaimDispatch(ctx, items[0].DispatchRef, releaseoperation.ClaimRequest{WorkerID: "before-rollback", LeaseDuration: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.operations.Fail(ctx, claim.Lease, releaseoperation.Failure{Code: "kubernetes_unavailable", Summary: "回退前退避", Disposition: releaseoperation.Retryable}); err != nil {
		t.Fatal(err)
	}
	// 首先停 Q1 受理入口，期间没有 Q1 Worker；保留加法 Schema 与 Outbox。
	environment.server.Close()
	apiAddress, workerAddress := unusedLoopbackAddress(t), unusedLoopbackAddress(t)
	common := []string{"ORBIT_DEVOPS_DATABASE_URL=" + environment.databaseURL, "ORBIT_DEVOPS_MIGRATE_ON_BOOT=false", "ORBIT_DEVOPS_ACTOR_ID=kind-developer",
		"ORBIT_DEVOPS_KUBERNETES_CONTEXT=" + kindContext, "ORBIT_DEVOPS_CLUSTER_REF=" + kindCluster, "ORBIT_DEVOPS_NAMESPACE=" + kindNamespace,
		"ORBIT_DEVOPS_API_ADDRESS=" + apiAddress, "ORBIT_DEVOPS_WORKER_ADDRESS=" + workerAddress, "ORBIT_DEVOPS_WORKER_ID=s2-compat-worker"}
	stopAPI := startCompatibilityProcess(t, apiBinary, common)
	awaitCompatibilityHealth(t, "http://"+apiAddress+"/healthz")
	endpoint, _ := url.Parse("http://" + apiAddress)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(endpoint))
	t.Cleanup(proxy.Close)
	environment.server = proxy
	legacy := environment.acceptRelease(t, "compat-s2", readyImage)
	cleanupResources(t, client, uuid.MustParse(legacy.TargetID))
	stopWorker := startCompatibilityProcess(t, workerBinary, common)
	awaitCompatibilityHealth(t, "http://"+workerAddress+"/healthz")
	eventuallyReleaseOperationStatus(t, environment, accepted.ReleaseOperationID, releaseoperation.StatusSucceeded, 30*time.Second)
	eventuallyReleaseOperationStatus(t, environment, legacy.ReleaseOperationID, releaseoperation.StatusSucceeded, 30*time.Second)
	current := environment.getReleaseOperation(t, accepted.ReleaseOperationID)
	if current.AttemptCount != 2 || current.Attempts[0].Status != releaseoperation.AttemptFailed {
		t.Fatalf("S2 rewrote Q1 history: %+v", current)
	}
	stopWorker()
	// 旧 API 在 Worker 停止后受理的 pending 没有 Q1 意图，重新切入必须补齐。
	backlog := environment.acceptRelease(t, "compat-backlog", readyImage)
	cleanupResources(t, client, uuid.MustParse(backlog.TargetID))
	stopAPI()
	proxy.Close()
	batch := uuid.New()
	prepared, err := environment.operations.PrepareDispatches(ctx, batch)
	if err != nil || prepared.Scheduled != 1 {
		t.Fatalf("reentry preparation: %+v %v", prepared, err)
	}
	repeated, err := environment.operations.PrepareDispatches(ctx, batch)
	if err != nil || !repeated.Replayed {
		t.Fatalf("repeated reentry: %+v %v", repeated, err)
	}
	if processed, err := environment.runner.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("Q1 reentry: %v %v", processed, err)
	}
	recovered, err := environment.operations.Get(ctx, uuid.MustParse(backlog.ReleaseOperationID))
	if err != nil || recovered.Status != releaseoperation.StatusSucceeded || recovered.AttemptCount != 1 {
		t.Fatalf("legacy backlog not recovered: %+v %v", recovered, err)
	}
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

// 每个停止函数等待真实进程退出，不以 HTTP 端口消失或租约到期替代退出确认。
func startCompatibilityProcess(t *testing.T, binary string, environment []string) func() {
	t.Helper()
	command := exec.Command(binary)
	command.Env = append(os.Environ(), environment...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = command.Process.Signal(os.Interrupt)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("S2 process exit: %v", err)
				}
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("S2 process failed to stop gracefully")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func awaitCompatibilityHealth(t *testing.T, address string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(address)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("S2 process did not become healthy")
}
