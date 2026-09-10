package integration_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/build"
	"github.com/HasonoCell/OrbitOps/internal/buildoperation"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/internalevent"
	"github.com/HasonoCell/OrbitOps/internal/pipeline"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jmoiron/sqlx"
	"github.com/prometheus/client_golang/prometheus"
)

type configurableSourceInspector struct {
	mu      sync.Mutex
	head    string
	headErr error
}

func (s *configurableSourceInspector) Resolve(_ context.Context, request pipeline.SourceRequest) (pipeline.SourceIdentity, error) {
	return pipeline.SourceIdentity{RepositoryID: 101, OwnerID: 202, RepositoryName: "example/orbitops-demo",
		RepositoryURL: "https://github.com/example/orbitops-demo.git", GitRef: "refs/heads/" + request.Branch, HeadCommit: s.currentHead()}, nil
}

func (s *configurableSourceInspector) Head(_ context.Context, request pipeline.HeadRequest) (pipeline.SourceIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headErr != nil {
		return pipeline.SourceIdentity{}, s.headErr
	}
	return pipeline.SourceIdentity{RepositoryID: request.RepositoryID, OwnerID: request.OwnerID,
		RepositoryName: "example/orbitops-demo", RepositoryURL: request.RepositoryURL,
		GitRef: request.GitRef, HeadCommit: s.head}, nil
}

func (s *configurableSourceInspector) currentHead() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head
}

func (s *configurableSourceInspector) setHead(head string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head, s.headErr = head, err
}

type automaticPipelineFixture struct {
	environment *testEnvironment
	db          *sqlx.DB
	pipelines   *pipeline.Module
	pipelineID  uuid.UUID
	projectID   uuid.UUID
	appID       uuid.UUID
}

func newAutomaticPipelineFixture(t *testing.T, inspector *configurableSourceInspector) *automaticPipelineFixture {
	t.Helper()
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{GitSourceInspector: inspector})
	project := createProject(t, environment, "automatic-fault-project")
	application := createApplication(t, environment, project.ID, "automatic-fault-application")
	targetResponse := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/deployment-targets", "automatic-fault-target", `{"stage":"development","replicas":1,"containerPort":8080}`)
	target := decodeDeploymentTarget(t, targetResponse)
	targetResponse.Body.Close()
	createResponse := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/delivery-pipelines", "automatic-fault-pipeline", `{"name":"main","endpointKey":"integration","repositoryUrl":"https://github.com/example/orbitops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"`+target.ID+`"}`)
	created := decodeDeliveryPipeline(t, createResponse)
	createResponse.Body.Close()
	enableResponse := environment.postJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID+"/enable", "automatic-fault-enable", "")
	if enableResponse.StatusCode != 200 {
		t.Fatalf("enable automatic pipeline status = %d", enableResponse.StatusCode)
	}
	enableResponse.Body.Close()
	db := openTestDatabase(t, environment.databaseURL)
	authorizer := projectauth.New(db)
	buildOperations := buildoperation.New(db)
	releaseOperations := releaseoperation.New(db)
	builds := build.New(db, build.Config{AllowedGitHosts: []string{"github.com"}, Platform: "linux/amd64", RegistryHost: "registry.example", RegistryPrefix: "orbitops"}, buildOperations, authorizer)
	releases := delivery.New(db, releaseOperations, authorizer)
	return &automaticPipelineFixture{environment: environment, db: db,
		pipelines:  pipeline.New(db, pipeline.Config{Platform: "linux/amd64", SourceRetryBaseDelay: 10 * time.Millisecond}, builds, releases, authorizer, inspector),
		pipelineID: uuid.MustParse(created.Pipeline.ID), projectID: uuid.MustParse(project.ID), appID: uuid.MustParse(application.ID)}
}

func (f *automaticPipelineFixture) acceptPush(t *testing.T, deliveryID, commit string) internalevent.Ref {
	t.Helper()
	payload := `{"ref":"refs/heads/main","before":"` + strings.Repeat("0", 40) + `","after":"` + commit + `","forced":false,"deleted":false,"repository":{"id":101,"full_name":"example/orbitops-demo","owner":{"id":202}}}`
	response := postGitHubWebhook(t, f.environment, deliveryID, "push", payload, "integration-webhook-secret")
	response.Body.Close()
	if response.StatusCode != 202 {
		t.Fatalf("accept webhook status = %d", response.StatusCode)
	}
	var event internalevent.Ref
	if err := f.db.Get(&event, `SELECT e.id AS event_id,e.topic,e.aggregate_id,e.protocol_version
		FROM internal_event_outbox e JOIN webhook_deliveries w ON w.id=e.aggregate_id
		WHERE e.topic='webhook_delivery.received.v1' AND w.provider_delivery_id=$1`, deliveryID); err != nil {
		t.Fatal(err)
	}
	return event
}

func internalEventConfig(address string) internalevent.Config {
	return internalevent.Config{RedisAddress: address, Queue: "orbitops-pipeline-test", Concurrency: 2,
		PollInterval: 20 * time.Millisecond, ConsumptionGrace: 100 * time.Millisecond,
		TaskTimeout: 5 * time.Second, ShutdownTimeout: time.Second}
}

func startInternalEvents(t *testing.T, service *internalevent.Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("stop internal events: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("internal event service did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func awaitAutomaticCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("automatic delivery condition was not reached")
}

func (f *automaticPipelineFixture) completeBuild(t *testing.T, commit string) uuid.UUID {
	t.Helper()
	var row struct {
		RunID       uuid.UUID `db:"run_id"`
		BuildID     uuid.UUID `db:"build_id"`
		OperationID uuid.UUID `db:"operation_id"`
	}
	if err := f.db.Get(&row, `SELECT dr.id AS run_id,dr.build_id,bo.id AS operation_id
		FROM delivery_runs dr JOIN build_operations bo ON bo.build_id=dr.build_id
		WHERE dr.source_commit=$1`, commit); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat(string(commit[0]), 64)
	now := time.Now().UTC()
	if _, err := f.db.Exec(`INSERT INTO image_artifacts(id,build_id,project_id,application_id,repository,digest,image_reference,platform,created_by,created_at)
		VALUES($1,$2,$3,$4,'registry.example/orbitops/demo',$5,'registry.example/orbitops/demo@'||$5,'linux/amd64','test',$6)`, uuid.New(), row.BuildID, f.projectID, f.appID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE build_operations SET status='succeeded',updated_at=$2,finished_at=$2 WHERE id=$1`, row.OperationID, now); err != nil {
		t.Fatal(err)
	}
	if err := f.pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "build_operation.changed.v1", AggregateID: row.OperationID, ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	return row.RunID
}

// Redis 中断不会回滚已验签事实；恢复后同一事件继续创建唯一 Build。
func TestInternalEventsRecoverWebhookAfterRedisOutage(t *testing.T) {
	inspector := &configurableSourceInspector{head: strings.Repeat("a", 40)}
	fixture := newAutomaticPipelineFixture(t, inspector)
	container, address := testsupport.StartRedis(t)
	service, err := internalevent.NewService(internalEventConfig(address), internalevent.New(fixture.db), fixture.pipelines)
	if err != nil {
		t.Fatal(err)
	}
	fixture.acceptPush(t, "pipeline-redis-outage", strings.Repeat("a", 40))
	if err := container.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := service.PublishOnce(context.Background()); err == nil {
		t.Fatal("Redis outage unexpectedly published internal event")
	}
	if err := container.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, originalPort, _ := net.SplitHostPort(address)
	restartedPort, err := container.MappedPort(context.Background(), "6379/tcp")
	if err != nil || restartedPort.Port() != originalPort {
		t.Fatalf("Redis endpoint changed after restart: %v", err)
	}
	startInternalEvents(t, service)
	awaitAutomaticCondition(t, func() bool {
		var count int
		return fixture.db.Get(&count, `SELECT count(*) FROM delivery_runs WHERE delivery_pipeline_id=$1`, fixture.pipelineID) == nil && count == 1
	})
}

// 已入队消息丢失和同一引用重放都只增加运输次数，不能复制 Run 或 Build。
func TestInternalEventsRepairLossAndIgnoreDuplicateMessages(t *testing.T) {
	inspector := &configurableSourceInspector{head: strings.Repeat("b", 40)}
	fixture := newAutomaticPipelineFixture(t, inspector)
	_, address := testsupport.StartRedis(t)
	event := fixture.acceptPush(t, "pipeline-lost-message", strings.Repeat("b", 40))
	service, err := internalevent.NewService(internalEventConfig(address), internalevent.New(fixture.db), fixture.pipelines)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	queueInspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: address})
	defer queueInspector.Close()
	tasks, err := queueInspector.ListPendingTasks("orbitops-pipeline-test")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("published internal events = %d, error = %v", len(tasks), err)
	}
	if err := queueInspector.DeleteTask("orbitops-pipeline-test", tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := service.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(event)
	client := asynq.NewClient(asynq.RedisClientOpt{Addr: address})
	defer client.Close()
	if _, err := client.Enqueue(asynq.NewTask(internalevent.TaskType, payload), asynq.Queue("orbitops-pipeline-test")); err != nil {
		t.Fatal(err)
	}
	startInternalEvents(t, service)
	awaitAutomaticCondition(t, func() bool {
		var runCount, buildCount int
		_ = fixture.db.Get(&runCount, `SELECT count(*) FROM delivery_runs WHERE delivery_pipeline_id=$1`, fixture.pipelineID)
		_ = fixture.db.Get(&buildCount, `SELECT count(*) FROM builds WHERE application_id=$1`, fixture.appID)
		return runCount == 1 && buildCount == 1
	})
	var reservations int
	if err := fixture.db.Get(&reservations, `SELECT reservation_count FROM internal_event_outbox WHERE id=$1`, event.EventID); err != nil || reservations < 2 {
		t.Fatalf("event reservations = %d, error = %v", reservations, err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(service, pipeline.NewMetrics(fixture.db))
	metrics, err := registry.Gather()
	if err != nil || len(metrics) < 10 {
		t.Fatalf("pipeline metrics = %d, error = %v", len(metrics), err)
	}
}

// 连续 Push 可以各自构建，但只有构建完成时仍是 Head 的 Commit 能跨过自动发布门禁。
func TestAutomaticDeliveryOnlyReleasesCurrentHead(t *testing.T) {
	commits := []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)}
	inspector := &configurableSourceInspector{head: commits[2]}
	fixture := newAutomaticPipelineFixture(t, inspector)
	for index, commit := range commits {
		event := fixture.acceptPush(t, "ordered-push-"+string(rune('a'+index)), commit)
		if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	rows := make([]struct {
		RunID       uuid.UUID `db:"run_id"`
		BuildID     uuid.UUID `db:"build_id"`
		OperationID uuid.UUID `db:"operation_id"`
		Commit      string    `db:"source_commit"`
	}, 0)
	if err := fixture.db.Select(&rows, `SELECT dr.id AS run_id,dr.build_id,bo.id AS operation_id,dr.source_commit
		FROM delivery_runs dr JOIN build_operations bo ON bo.build_id=dr.build_id ORDER BY dr.source_commit`); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		digest := "sha256:" + strings.Repeat(string(row.Commit[0]), 64)
		now := time.Now().UTC()
		if _, err := fixture.db.Exec(`INSERT INTO image_artifacts(id,build_id,project_id,application_id,repository,digest,image_reference,platform,created_by,created_at)
			VALUES($1,$2,$3,$4,'registry.example/orbitops/demo',$5,'registry.example/orbitops/demo@'||$5,'linux/amd64','test',$6)`, uuid.New(), row.BuildID, fixture.projectID, fixture.appID, digest, now); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.db.Exec(`UPDATE build_operations SET status='succeeded',updated_at=$2,finished_at=$2 WHERE id=$1`, row.OperationID, now); err != nil {
			t.Fatal(err)
		}
		if err := fixture.pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "build_operation.changed.v1", AggregateID: row.OperationID, ProtocolVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	phases := make([]string, 0)
	if err := fixture.db.Select(&phases, `SELECT phase FROM delivery_runs ORDER BY source_commit`); err != nil {
		t.Fatal(err)
	}
	if strings.Join(phases, ",") != "superseded,superseded,release_created" {
		t.Fatalf("ordered push phases = %v", phases)
	}
	var releaseCount int
	if err := fixture.db.Get(&releaseCount, `SELECT count(*) FROM releases`); err != nil || releaseCount != 1 {
		t.Fatalf("automatic releases = %d, error = %v", releaseCount, err)
	}
}

// 短时 GitHub 故障不会猜测发布；人工 Reconcile 仍必须重新通过同一 Head 门禁。
func TestAutomaticDeliveryReconcilesTransientSourceFailure(t *testing.T) {
	commit := strings.Repeat("d", 40)
	inspector := &configurableSourceInspector{head: commit, headErr: pipeline.ErrSourceUnavailable}
	fixture := newAutomaticPipelineFixture(t, inspector)
	event := fixture.acceptPush(t, "transient-source", commit)
	if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	runID := fixture.completeBuild(t, commit)
	var current struct {
		Phase     string     `db:"phase"`
		Reason    *string    `db:"reason_code"`
		NextAt    *time.Time `db:"source_check_next_at"`
		ReleaseID *uuid.UUID `db:"release_id"`
	}
	if err := fixture.db.Get(&current, `SELECT phase,reason_code,source_check_next_at,release_id FROM delivery_runs WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if current.Phase != "artifact_ready" || current.Reason == nil || *current.Reason != "source_verification_unavailable" || current.NextAt == nil || current.ReleaseID != nil {
		t.Fatalf("deferred source verification = %#v", current)
	}
	inspector.setHead(commit, nil)
	if err := fixture.pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "delivery_run.reconcile.v1", AggregateID: runID, ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Get(&current, `SELECT phase,reason_code,source_check_next_at,release_id FROM delivery_runs WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if current.Phase != "release_created" || current.ReleaseID == nil || current.Reason != nil {
		t.Fatalf("reconciled source verification = %#v", current)
	}
}

// Owner 转移和 Pipeline 换代都会永久关闭旧 Run 的自动发布资格。
func TestAutomaticDeliveryBlocksChangedAuthority(t *testing.T) {
	t.Run("repository owner changed", func(t *testing.T) {
		commit := strings.Repeat("e", 40)
		inspector := &configurableSourceInspector{head: commit, headErr: pipeline.ErrSourceOwnerChanged}
		fixture := newAutomaticPipelineFixture(t, inspector)
		event := fixture.acceptPush(t, "owner-changed", commit)
		if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		runID := fixture.completeBuild(t, commit)
		assertRunPhaseAndReason(t, fixture.db, runID, "blocked", "repository_owner_changed")
	})

	t.Run("repository or branch missing", func(t *testing.T) {
		commit := strings.Repeat("7", 40)
		inspector := &configurableSourceInspector{head: commit, headErr: pipeline.ErrSourceNotFound}
		fixture := newAutomaticPipelineFixture(t, inspector)
		event := fixture.acceptPush(t, "source-missing", commit)
		if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		runID := fixture.completeBuild(t, commit)
		assertRunPhaseAndReason(t, fixture.db, runID, "blocked", "repository_or_branch_not_found")
	})

	t.Run("activation generation changed", func(t *testing.T) {
		commit := strings.Repeat("f", 40)
		inspector := &configurableSourceInspector{head: commit}
		fixture := newAutomaticPipelineFixture(t, inspector)
		event := fixture.acceptPush(t, "pipeline-disabled", commit)
		if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
			t.Fatal(err)
		}
		response := fixture.environment.postJSON(t, "/api/v1/delivery-pipelines/"+fixture.pipelineID.String()+"/disable", "disable-before-release", "")
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("disable pipeline status = %d", response.StatusCode)
		}
		runID := fixture.completeBuild(t, commit)
		assertRunPhaseAndReason(t, fixture.db, runID, "superseded", "pipeline_configuration_changed")
	})
}

func assertRunPhaseAndReason(t *testing.T, db *sqlx.DB, runID uuid.UUID, phase, reason string) {
	t.Helper()
	var current struct {
		Phase  string `db:"phase"`
		Reason string `db:"reason_code"`
	}
	if err := db.Get(&current, `SELECT phase,reason_code FROM delivery_runs WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if current.Phase != phase || current.Reason != reason {
		t.Fatalf("run = %#v, want %s/%s", current, phase, reason)
	}
}

// 多个重放消费者可以同时看到 Build 成功，但行锁和 phase_version 只允许一个 Release。
func TestAutomaticDeliveryConcurrentAdvanceCreatesOneRelease(t *testing.T) {
	commit := strings.Repeat("9", 40)
	inspector := &configurableSourceInspector{head: commit}
	fixture := newAutomaticPipelineFixture(t, inspector)
	event := fixture.acceptPush(t, "concurrent-advance", commit)
	if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var row struct {
		BuildID     uuid.UUID `db:"build_id"`
		OperationID uuid.UUID `db:"operation_id"`
	}
	if err := fixture.db.Get(&row, `SELECT dr.build_id,bo.id AS operation_id FROM delivery_runs dr JOIN build_operations bo ON bo.build_id=dr.build_id WHERE dr.source_commit=$1`, commit); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := "sha256:" + strings.Repeat("9", 64)
	if _, err := fixture.db.Exec(`INSERT INTO image_artifacts(id,build_id,project_id,application_id,repository,digest,image_reference,platform,created_by,created_at)
		VALUES($1,$2,$3,$4,'registry.example/orbitops/demo',$5,'registry.example/orbitops/demo@'||$5,'linux/amd64','test',$6)`, uuid.New(), row.BuildID, fixture.projectID, fixture.appID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`UPDATE build_operations SET status='succeeded',updated_at=$2,finished_at=$2 WHERE id=$1`, row.OperationID, now); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	errorsByWorker := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsByWorker <- fixture.pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "build_operation.changed.v1", AggregateID: row.OperationID, ProtocolVersion: 1})
		}()
	}
	workers.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
	var releases int
	if err := fixture.db.Get(&releases, `SELECT count(*) FROM releases`); err != nil || releases != 1 {
		t.Fatalf("concurrent releases = %d, error = %v", releases, err)
	}
}

// Webhook 超过保留期后可以删除，Run 冻结的安全触发摘要仍能被项目成员查询。
func TestAutomaticDeliveryRetainsTriggerAfterWebhookCleanup(t *testing.T) {
	commit := strings.Repeat("8", 40)
	inspector := &configurableSourceInspector{head: commit}
	fixture := newAutomaticPipelineFixture(t, inspector)
	event := fixture.acceptPush(t, "retained-trigger", commit)
	if err := fixture.pipelines.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var runID uuid.UUID
	if err := fixture.db.Get(&runID, `SELECT id FROM delivery_runs WHERE delivery_pipeline_id=$1`, fixture.pipelineID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`UPDATE webhook_deliveries SET processed_at=$2 WHERE id=$1`, event.AggregateID, time.Now().UTC().Add(-91*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pipelines.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	detail, err := fixture.pipelines.GetRun(context.Background(), runID, "local-developer")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.WebhookDeliveryID != nil || detail.Trigger.EventType != "push" || detail.Trigger.RepositoryFullName != "example/orbitops-demo" || detail.Trigger.GitRef != "refs/heads/main" {
		t.Fatalf("retained run trigger = %#v", detail)
	}
}
