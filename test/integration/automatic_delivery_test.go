package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/app"
	"github.com/HasonoCell/Orbit-DevOps/internal/build"
	"github.com/HasonoCell/Orbit-DevOps/internal/buildoperation"
	"github.com/HasonoCell/Orbit-DevOps/internal/delivery"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/pipeline"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/HasonoCell/Orbit-DevOps/internal/releaseoperation"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// 该测试用真实 PostgreSQL 固定跨领域事务；Worker/Redis 的物理投递故障由独立测试覆盖。
func TestAutomaticDeliveryOrchestratesExistingBuildAndReleaseDomains(t *testing.T) {
	environment := newTestEnvironmentWithDependencies(t, app.Dependencies{GitSourceInspector: fixedSourceInspector{}})
	project := createProject(t, environment, "automatic-delivery-project")
	application := createApplication(t, environment, project.ID, "automatic-delivery-application")
	targetResponse := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/deployment-targets", "automatic-delivery-target", `{"stage":"development","replicas":1,"containerPort":8080}`)
	defer targetResponse.Body.Close()
	if targetResponse.StatusCode != http.StatusCreated {
		t.Fatalf("create target status = %d", targetResponse.StatusCode)
	}
	target := decodeDeploymentTarget(t, targetResponse)
	createPipeline := environment.postJSON(t, "/api/v1/applications/"+application.ID+"/delivery-pipelines", "automatic-pipeline", `{"name":"main","endpointKey":"integration","repositoryUrl":"https://github.com/example/orbit-devops-demo.git","branch":"main","mode":"auto_release","deploymentTargetId":"`+target.ID+`"}`)
	defer createPipeline.Body.Close()
	if createPipeline.StatusCode != http.StatusCreated {
		t.Fatalf("create pipeline status = %d", createPipeline.StatusCode)
	}
	created := decodeDeliveryPipeline(t, createPipeline)
	enable := environment.postJSON(t, "/api/v1/delivery-pipelines/"+created.Pipeline.ID+"/enable", "automatic-enable", "")
	defer enable.Body.Close()
	if enable.StatusCode != http.StatusOK {
		t.Fatalf("enable pipeline status = %d", enable.StatusCode)
	}

	commit := strings.Repeat("a", 40)
	payload := `{"ref":"refs/heads/main","before":"` + strings.Repeat("0", 40) + `","after":"` + commit + `","forced":false,"deleted":false,"repository":{"id":101,"full_name":"example/orbit-devops-demo","owner":{"id":202}}}`
	webhookResponse := postGitHubWebhook(t, environment, "automatic-delivery-1", "push", payload, "integration-webhook-secret")
	defer webhookResponse.Body.Close()
	if webhookResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status = %d", webhookResponse.StatusCode)
	}

	database, err := sqlx.Open("pgx", environment.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	authorizer := projectauth.New(database)
	buildOperations := buildoperation.New(database)
	releaseOperations := releaseoperation.New(database)
	builds := build.New(database, build.Config{AllowedGitHosts: []string{"github.com"}, Platform: "linux/amd64", RegistryHost: "registry.example", RegistryPrefix: "orbit-devops"}, buildOperations, authorizer)
	releases := delivery.New(database, releaseOperations, authorizer)
	pipelines := pipeline.New(database, pipeline.Config{Platform: "linux/amd64"}, builds, releases, authorizer, fixedSourceInspector{})

	webhookEvent := loadEvent(t, database, "webhook_delivery.received.v1", uuid.Nil)
	if err := pipelines.HandleEvent(context.Background(), webhookEvent); err != nil {
		t.Fatal(err)
	}
	// 重放同一事件只能找到既有 Run，不能创建第二个 Build。
	if err := pipelines.HandleEvent(context.Background(), webhookEvent); err != nil {
		t.Fatal(err)
	}
	var run struct {
		ID      uuid.UUID `db:"id"`
		BuildID uuid.UUID `db:"build_id"`
		Phase   string    `db:"phase"`
	}
	if err := database.Get(&run, `SELECT id,build_id,phase FROM delivery_runs WHERE delivery_pipeline_id=$1`, created.Pipeline.ID); err != nil {
		t.Fatal(err)
	}
	var buildCount int
	if err := database.Get(&buildCount, `SELECT count(*) FROM builds WHERE id=$1`, run.BuildID); err != nil || buildCount != 1 {
		t.Fatalf("build count = %d, err=%v", buildCount, err)
	}

	var buildOperationID uuid.UUID
	if err := database.Get(&buildOperationID, `SELECT id FROM build_operations WHERE build_id=$1`, run.BuildID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	digest := "sha256:" + strings.Repeat("c", 64)
	if _, err := database.Exec(`INSERT INTO image_artifacts(id,build_id,project_id,application_id,repository,digest,image_reference,platform,created_by,created_at)
		VALUES($1,$2,$3,$4,'registry.example/orbit-devops/demo',$5,'registry.example/orbit-devops/demo@'||$5,'linux/amd64','test',$6)`, uuid.New(), run.BuildID, project.ID, application.ID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE build_operations SET status='succeeded',updated_at=$2,finished_at=$2 WHERE id=$1`, buildOperationID, now); err != nil {
		t.Fatal(err)
	}
	if err := pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "build_operation.changed.v1", AggregateID: buildOperationID, ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}

	var releaseID, releaseOperationID uuid.UUID
	if err := database.QueryRowx(`SELECT dr.release_id,ro.id FROM delivery_runs dr JOIN release_operations ro ON ro.release_id=dr.release_id WHERE dr.id=$1`, run.ID).Scan(&releaseID, &releaseOperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE release_operations SET status='succeeded',updated_at=$2,finished_at=$2 WHERE id=$1`, releaseOperationID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := pipelines.HandleEvent(context.Background(), internalevent.Ref{EventID: uuid.New(), Topic: "release_operation.changed.v1", AggregateID: releaseOperationID, ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := database.Get(&run.Phase, `SELECT phase FROM delivery_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if run.Phase != "completed" {
		t.Fatalf("delivery run phase = %q, want completed", run.Phase)
	}
	var audits []struct {
		ActorKind string          `db:"actor_kind"`
		Action    string          `db:"action"`
		Summary   json.RawMessage `db:"summary"`
	}
	if err := database.Select(&audits, `SELECT actor_kind,action,summary FROM audit_records
		WHERE action IN ('build.create','release.create') ORDER BY action`); err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("automatic audit records = %d", len(audits))
	}
	for _, record := range audits {
		var summary map[string]string
		if err := json.Unmarshal(record.Summary, &summary); err != nil {
			t.Fatal(err)
		}
		if record.ActorKind != "system" || summary["configuredBy"] != "local-developer" || summary["webhookDeliveryId"] == "" || summary["deliveryRunId"] != run.ID.String() {
			t.Fatalf("automatic audit %s = %s %#v", record.Action, record.ActorKind, summary)
		}
	}
}

func loadEvent(t *testing.T, database *sqlx.DB, topic string, aggregateID uuid.UUID) internalevent.Ref {
	t.Helper()
	var event internalevent.Ref
	query := `SELECT id AS event_id,topic,aggregate_id,protocol_version FROM internal_event_outbox WHERE topic=$1`
	args := []any{topic}
	if aggregateID != uuid.Nil {
		query += ` AND aggregate_id=$2`
		args = append(args, aggregateID)
	}
	query += ` ORDER BY created_at DESC LIMIT 1`
	if err := database.Get(&event, query, args...); err != nil {
		t.Fatal(err)
	}
	return event
}
