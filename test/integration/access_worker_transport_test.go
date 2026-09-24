package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/accessworker"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/test/testsupport"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type accessSnapshotRecorder struct {
	mu        sync.Mutex
	snapshots []access.Snapshot
}

func (r *accessSnapshotRecorder) Reconcile(ctx context.Context, snapshot access.Snapshot, guard func(context.Context) error) error {
	if err := guard(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snapshots = append(r.snapshots, snapshot)
	return nil
}

func (r *accessSnapshotRecorder) copies() []access.Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]access.Snapshot(nil), r.snapshots...)
}

// TestAccessGatewayOutboxRedisRecovery 串起 HTTP 命令、PostgreSQL outbox、Asynq 和 Gateway Worker。
// Kubernetes 写入由记录器替代；真实控制器与流量由独立 Kind 测试验证。
func TestAccessGatewayOutboxRedisRecovery(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "gateway-transport-project")
	application := createApplication(t, environment, project.ID, "gateway-transport-app")
	target := createAccessTarget(t, environment, application.ID, "gateway-transport-target", 8080)
	projectID := uuid.MustParse(project.ID)
	container, redisAddress := testsupport.StartRedis(t)
	database, err := sqlx.ConnectContext(context.Background(), "pgx", environment.databaseURL)
	if err != nil {
		t.Fatalf("connect gateway worker database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	module, err := access.New(database, projectauth.New(database, nil), access.Config{
		ClusterRef: "kind-orbit-devops-s1", Namespace: "orbit-devops-s1", GatewayClassName: "gateway-transport-test",
	}, nil, nil)
	if err != nil {
		t.Fatalf("create access module: %v", err)
	}
	recorder := &accessSnapshotRecorder{}
	worker, err := accessworker.New(module, recorder, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("create gateway worker: %v", err)
	}
	queueConfig := internalEventConfig(redisAddress)
	queueConfig.Queue = "orbit-devops-gateway-transport-test"
	queueConfig.Topics = []string{accessworker.Topic}
	service, err := internalevent.NewService(queueConfig, internalevent.New(database), worker)
	if err != nil {
		t.Fatalf("create gateway event service: %v", err)
	}

	// 命令在 Redis 停机时照常提交；只有 outbox 的投递被推迟。
	if err := container.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop isolated Redis: %v", err)
	}
	hostPath := "/api/v1/projects/" + project.ID + "/access-hosts"
	host := requestAccessDocument[accessHostDocument](t, environment.server, http.MethodPost, hostPath,
		"gateway-transport-host", `{"hostname":"transport.example.test","tlsMode":"http_only"}`, http.StatusCreated)
	route := requestAccessDocument[accessRouteDocument](t, environment.server, http.MethodPost,
		hostPath+"/"+host.ID+"/routes", "gateway-transport-route",
		fmt.Sprintf(`{"pathPrefix":"/api","deploymentTargetId":%q}`, target.ID), http.StatusCreated)
	var desired, applied int64
	var state string
	if err := database.QueryRow(`SELECT desired_revision,applied_revision,state FROM project_gateway_sync
		WHERE project_id=$1`, projectID).Scan(&desired, &applied, &state); err != nil {
		t.Fatalf("load gateway sync before Redis recovery: %v", err)
	}
	if desired != 2 || applied != 0 || state != "pending" {
		t.Fatalf("sync before Redis recovery = desired %d, applied %d, state %q", desired, applied, state)
	}
	var pending int
	if err := database.Get(&pending, `SELECT count(*) FROM internal_event_outbox
		WHERE topic=$1 AND aggregate_id=$2 AND state='pending'`, accessworker.Topic, projectID); err != nil || pending != 2 {
		t.Fatalf("pending gateway events = %d, error = %v", pending, err)
	}
	if err := service.PublishOnce(context.Background()); err == nil {
		t.Fatal("Redis outage unexpectedly published gateway events")
	}
	if err := container.Start(context.Background()); err != nil {
		t.Fatalf("restart isolated Redis: %v", err)
	}
	_, originalPort, _ := net.SplitHostPort(redisAddress)
	restartedPort, err := container.MappedPort(context.Background(), "6379/tcp")
	if err != nil || restartedPort.Port() != originalPort {
		t.Fatalf("Redis endpoint changed after restart: %v", err)
	}
	startInternalEvents(t, service)
	deadline := time.Now().Add(20 * time.Second)
	consumed := 0
	for time.Now().Before(deadline) {
		err := database.QueryRow(`SELECT applied_revision,state FROM project_gateway_sync
			WHERE project_id=$1`, projectID).Scan(&applied, &state)
		if err == nil {
			err = database.Get(&consumed, `SELECT count(*) FROM internal_event_outbox
				WHERE topic=$1 AND aggregate_id=$2 AND state='consumed'`, accessworker.Topic, projectID)
		}
		if err == nil && applied == 2 && state == "applied" && consumed == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if applied != 2 || state != "applied" || consumed != 2 {
		t.Fatalf("gateway worker did not consume latest revision: applied %d, state %q, consumed %d", applied, state, consumed)
	}
	snapshots := recorder.copies()
	if len(snapshots) == 0 {
		t.Fatal("gateway reconciler was never called")
	}
	for _, snapshot := range snapshots {
		if snapshot.Revision != 2 || snapshot.ProjectID != projectID || len(snapshot.Hosts) != 1 ||
			len(snapshot.Routes) != 1 || snapshot.Routes[0].Route.ID.String() != route.ID {
			t.Fatalf("worker replayed stale or incomplete snapshot: %+v", snapshot)
		}
	}

	// 模拟唤醒消息完全丢失但数据库期望已推进；维护扫描应独立恢复收敛。
	if _, err := database.Exec(`UPDATE project_gateway_sync SET desired_revision=3,state='pending',
		next_attempt_at=clock_timestamp() WHERE project_id=$1`, projectID); err != nil {
		t.Fatalf("inject lost gateway wakeup: %v", err)
	}
	maintenanceContext, stopMaintenance := context.WithCancel(context.Background())
	maintenanceDone := make(chan error, 1)
	go func() { maintenanceDone <- worker.Maintain(maintenanceContext, 20*time.Millisecond) }()
	defer func() {
		stopMaintenance()
		if err := <-maintenanceDone; err != nil {
			t.Errorf("stop gateway maintenance: %v", err)
		}
	}()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.QueryRow(`SELECT applied_revision,state FROM project_gateway_sync
			WHERE project_id=$1`, projectID).Scan(&applied, &state); err == nil && applied == 3 && state == "applied" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if applied != 3 || state != "applied" {
		t.Fatalf("maintenance did not repair lost wakeup: applied %d, state %q", applied, state)
	}
	snapshots = recorder.copies()
	if snapshots[len(snapshots)-1].Revision != 3 {
		t.Fatalf("maintenance reconciled revision %d, want 3", snapshots[len(snapshots)-1].Revision)
	}
}

// TestAccessGatewayWorkerRecoversExpiredLease 模拟进程在取得租约后退出，由维护扫描接管。
func TestAccessGatewayWorkerRecoversExpiredLease(t *testing.T) {
	environment := newTestEnvironment(t)
	project := createProject(t, environment, "gateway-expired-lease")
	response := requestJSON(t, environment.server, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/access-hosts", "gateway-expired-host",
		`{"hostname":"expired-lease.example.test","tlsMode":"http_only"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create host status = %d", response.StatusCode)
	}
	database, err := sqlx.ConnectContext(context.Background(), "pgx", environment.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	module, err := access.New(database, projectauth.New(database, nil), access.Config{
		ClusterRef: "kind-orbit-devops-s1", Namespace: "orbit-devops-s1",
		GatewayClassName: "expired-lease-test",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.MustParse(project.ID)
	lease, acquired, err := module.AcquireReconcileLease(context.Background(), projectID, 200*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("acquire abandoned lease: acquired=%t error=%v", acquired, err)
	}
	recorder := &accessSnapshotRecorder{}
	worker, err := accessworker.New(module, recorder, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ref := internalevent.Ref{Topic: accessworker.Topic, AggregateID: projectID, ProtocolVersion: 1}
	if err := worker.HandleEvent(context.Background(), ref); err != nil {
		t.Fatalf("duplicate wakeup while lease held: %v", err)
	}
	if len(recorder.copies()) != 0 {
		t.Fatal("second worker applied while abandoned lease was still valid")
	}
	if err := module.CheckLease(context.Background(), lease, lease.Revision); err != nil {
		t.Fatalf("abandoned lease expired too early: %v", err)
	}
	maintenanceContext, stop := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- worker.Maintain(maintenanceContext, 20*time.Millisecond) }()
	defer func() {
		stop()
		if err := <-finished; err != nil {
			t.Errorf("stop maintenance: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var desired, applied int64
	var state string
	for time.Now().Before(deadline) {
		err := database.QueryRow(`SELECT desired_revision,applied_revision,state FROM project_gateway_sync
			WHERE project_id=$1`, projectID).Scan(&desired, &applied, &state)
		if err == nil && desired == 1 && applied == 1 && state == "applied" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if desired != 1 || applied != 1 || state != "applied" {
		t.Fatalf("expired lease did not recover: desired=%d applied=%d state=%q", desired, applied, state)
	}
	if err := module.CheckLease(context.Background(), lease, lease.Revision); err != access.ErrLeaseLost {
		t.Fatalf("abandoned worker retained authority: %v", err)
	}
	if snapshots := recorder.copies(); len(snapshots) == 0 || snapshots[len(snapshots)-1].Revision != 1 {
		t.Fatalf("maintenance did not reconcile latest snapshot: %+v", snapshots)
	}
}
