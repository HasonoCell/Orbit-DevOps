package kind_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/delivery"
	"github.com/HasonoCell/OrbitOps/internal/diagnostics"
	"github.com/HasonoCell/OrbitOps/internal/dispatch"
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/HasonoCell/OrbitOps/test/testsupport"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKindControlPlaneDeliveryLoop(t *testing.T) {
	adapter, client := newKindAdapter(t)
	environment := newKindControlPlane(t, adapter)

	t.Run("successful release", func(t *testing.T) {
		acceptance := environment.acceptRelease(t, "success", readyImage)
		targetID := uuid.MustParse(acceptance.TargetID)
		cleanupResources(t, client, targetID)

		processed, err := environment.runner.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("run successful operation: %v", err)
		}
		if !processed {
			t.Fatal("successful operation was not processed")
		}

		current := environment.getOperation(t, acceptance.OperationID)
		if current.Status != operation.StatusSucceeded {
			t.Errorf("operation status = %q, want succeeded", current.Status)
		}
		if current.AttemptCount != 1 || len(current.Attempts) != 1 ||
			current.Attempts[0].Status != operation.AttemptSucceeded {
			t.Errorf("operation attempts = %#v, want one succeeded attempt", current.Attempts)
		}

		report := environment.getReleaseDiagnostics(t, acceptance.ReleaseID)
		if report.WorkloadObservation.Metadata.Source != diagnostics.SourceKubernetes ||
			report.WorkloadObservation.Metadata.Status != string(diagnostics.ObservationComplete) {
			t.Errorf("runtime observation metadata = %#v, want complete Kubernetes evidence", report.WorkloadObservation.Metadata)
		}
		deployment := report.WorkloadObservation.Deployment
		if deployment == nil || deployment.ReadyReplicas != 1 {
			t.Errorf("runtime deployment = %#v, want Ready deployment", deployment)
		}
		if deployment.ReleaseID == nil || *deployment.ReleaseID != acceptance.ReleaseID ||
			report.RuntimeReleaseRelation != string(diagnostics.RuntimeReleaseMatches) {
			t.Errorf("runtime release relation = %q, releaseId = %v, want %s", report.RuntimeReleaseRelation, deployment.ReleaseID, acceptance.ReleaseID)
		}
	})

	t.Run("failed release", func(t *testing.T) {
		image := "registry.invalid/orbitops/missing@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		acceptance := environment.acceptRelease(t, "failure", image)
		targetID := uuid.MustParse(acceptance.TargetID)
		cleanupResources(t, client, targetID)

		processed, err := environment.runner.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("run failed operation: %v", err)
		}
		if !processed {
			t.Fatal("failed operation was not processed")
		}

		current := environment.getOperation(t, acceptance.OperationID)
		if current.Status != operation.StatusFailed {
			t.Errorf("operation status = %q, want failed", current.Status)
		}
		if current.ErrorCode == nil || *current.ErrorCode != "image_pull_failed" {
			t.Errorf("operation errorCode = %v, want image_pull_failed", current.ErrorCode)
		}
		if len(current.Attempts) != 1 || current.Attempts[0].Status != operation.AttemptFailed {
			t.Errorf("operation attempts = %#v, want one failed attempt", current.Attempts)
		}

		report := environment.getReleaseDiagnostics(t, acceptance.ReleaseID)
		if report.WorkloadObservation.Metadata.Status != string(diagnostics.ObservationComplete) ||
			report.WorkloadObservation.Deployment == nil {
			t.Errorf("failure runtime observation = %#v, want complete Kubernetes evidence", report.WorkloadObservation)
		}
		if len(report.WorkloadObservation.Pods) == 0 || report.WorkloadObservation.Pods[0].Reason == "" {
			t.Errorf("failure pods = %#v, want Kubernetes pull failure", report.WorkloadObservation.Pods)
		}
	})

	t.Run("automatic reconciliation after external apply", func(t *testing.T) {
		acceptance := environment.acceptRelease(t, "recovery", readyImage)
		targetID := uuid.MustParse(acceptance.TargetID)
		cleanupResources(t, client, targetID)
		items, err := environment.operations.ReserveDispatches(context.Background(), 100, time.Second)
		if err != nil || len(items) != 1 {
			t.Fatalf("reserve pre-interruption intent: %d %v", len(items), err)
		}
		claim, err := environment.operations.ClaimDispatch(context.Background(), items[0].DispatchRef, operation.ClaimRequest{
			WorkerID: "kind-lost-worker", LeaseDuration: 300 * time.Millisecond,
		})
		lease, claimed := claim.Lease, claim.Outcome == operation.ClaimOutcomeClaimed
		if err != nil || !claimed || lease.OperationID.String() != acceptance.OperationID {
			t.Fatalf("claim operation before simulated process loss = %#v, %v, %v", lease, claimed, err)
		}
		release, err := environment.releases.GetRelease(context.Background(), lease.ReleaseID)
		if err != nil {
			t.Fatalf("load release before external Apply: %v", err)
		}
		if err := adapter.Publish(context.Background(), publishRequest(lease, release)); err != nil {
			t.Fatalf("apply before simulated database interruption: %v", err)
		}
		time.Sleep(350 * time.Millisecond)

		processed, err := environment.runner.RunOnce(context.Background())
		if err != nil || !processed {
			t.Fatalf("reconcile externally applied release = %v, error = %v", processed, err)
		}
		current := environment.getOperation(t, acceptance.OperationID)
		if current.Status != operation.StatusSucceeded || current.AttemptCount != 2 ||
			current.Attempts[0].Status != operation.AttemptOutcomeUnknown ||
			current.Attempts[1].Status != operation.AttemptSucceeded {
			t.Fatalf("reconciled operation = %#v", current)
		}
	})

	t.Run("running release cancellation", func(t *testing.T) {
		image := "registry.invalid/orbitops/cancel@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		acceptance := environment.acceptRelease(t, "cancel", image)
		targetID := uuid.MustParse(acceptance.TargetID)
		cleanupResources(t, client, targetID)
		result := make(chan error, 1)
		go func() {
			processed, err := environment.runner.RunOnce(context.Background())
			if err == nil && !processed {
				err = fmt.Errorf("Kind Worker did not claim cancellation operation")
			}
			result <- err
		}()
		eventuallyOperationStatus(t, environment, acceptance.OperationID, operation.StatusRunning, 5*time.Second)
		environment.postCommand(
			t,
			"/api/v1/operations/"+acceptance.OperationID+"/cancel",
			"cancel-kind-running-release",
			http.StatusOK,
		)
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("cancel Kind Worker: %v", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("Kind Worker did not acknowledge cancellation")
		}
		current := environment.getOperation(t, acceptance.OperationID)
		if current.Status != operation.StatusCanceled || len(current.Attempts) != 1 ||
			current.Attempts[0].Status != operation.AttemptCanceled {
			t.Fatalf("canceled Kind operation = %#v", current)
		}
	})

	t.Run("rollback applies a new release", func(t *testing.T) {
		source := environment.acceptRelease(t, "rollback-source", readyImage)
		targetID := uuid.MustParse(source.TargetID)
		cleanupResources(t, client, targetID)
		if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
			t.Fatalf("publish rollback source = %v, error = %v", processed, err)
		}
		response := environment.postJSON(
			t,
			"/api/v1/releases/"+source.ReleaseID+"/rollback",
			"kind-rollback-release",
			"",
		)
		rollback := decodeReleaseAcceptance(t, response)
		if rollback.RollbackOfReleaseID == nil || *rollback.RollbackOfReleaseID != source.ReleaseID {
			t.Fatalf("rollback lineage = %#v, want source %s", rollback, source.ReleaseID)
		}
		if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
			t.Fatalf("publish rollback = %v, error = %v", processed, err)
		}
		current := environment.getOperation(t, rollback.OperationID)
		if current.Status != operation.StatusSucceeded {
			t.Fatalf("rollback operation = %#v", current)
		}
		report := environment.getReleaseDiagnostics(t, rollback.ReleaseID)
		deployment := report.WorkloadObservation.Deployment
		if deployment == nil || deployment.ReleaseID == nil || *deployment.ReleaseID != rollback.ReleaseID {
			t.Fatalf("rollback runtime deployment = %#v, want release %s", deployment, rollback.ReleaseID)
		}
	})

	t.Run("foreign ownership conflict is preserved", func(t *testing.T) {
		acceptance := environment.acceptRelease(t, "ownership-conflict", readyImage)
		targetID := uuid.MustParse(acceptance.TargetID)
		cleanupResources(t, client, targetID)
		name := kube.ResourceName(targetID)
		replicas := int32(1)
		foreign := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: kindNamespace, Labels: map[string]string{"owner": "external-control-plane-test"},
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "external", Image: readyImage}}},
				},
			},
		}
		if _, err := client.AppsV1().Deployments(kindNamespace).Create(
			context.Background(), foreign, metav1.CreateOptions{},
		); err != nil {
			t.Fatalf("create foreign control-plane deployment: %v", err)
		}
		if processed, err := environment.runner.RunOnce(context.Background()); err != nil || !processed {
			t.Fatalf("run ownership-conflict operation = %v, error = %v", processed, err)
		}
		current := environment.getOperation(t, acceptance.OperationID)
		if current.Status != operation.StatusFailed || current.ErrorCode == nil ||
			*current.ErrorCode != "ownership_conflict" {
			t.Fatalf("ownership-conflict operation = %#v", current)
		}
		preserved, err := client.AppsV1().Deployments(kindNamespace).Get(
			context.Background(), name, metav1.GetOptions{},
		)
		if err != nil || preserved.Labels["owner"] != "external-control-plane-test" {
			t.Fatalf("foreign deployment was not preserved: %#v, error = %v", preserved, err)
		}
	})
}

type kindControlPlane struct {
	databaseURL string
	server      *httptest.Server
	runner      *kindQueueRunner
	operations  *operation.Module
	releases    *delivery.Module
}

type releaseAcceptance struct {
	TargetID            string
	ReleaseID           string
	OperationID         string
	RollbackOfReleaseID *string
}

type operationResponse struct {
	Status       operation.OperationStatus  `json:"status"`
	AttemptCount int                        `json:"attemptCount"`
	ErrorCode    *string                    `json:"errorCode"`
	Attempts     []operationAttemptResponse `json:"attempts"`
}

type operationAttemptResponse struct {
	Status operation.AttemptStatus `json:"status"`
}

type releaseDiagnosticResponse struct {
	RuntimeReleaseRelation string `json:"runtimeReleaseRelation"`
	WorkloadObservation    struct {
		Metadata struct {
			Source string `json:"source"`
			Status string `json:"status"`
		} `json:"metadata"`
		Deployment *struct {
			ReleaseID     *string `json:"releaseId"`
			ReadyReplicas int     `json:"readyReplicas"`
		} `json:"deployment"`
		Pods []diagnosticPodResponse `json:"pods"`
	} `json:"workloadObservation"`
}

type diagnosticPodResponse struct {
	Reason string `json:"reason"`
}

func newKindControlPlane(t *testing.T, adapter *kube.Adapter) *kindControlPlane {
	t.Helper()
	ctx := context.Background()
	postgresContainer, err := postgres.Run(
		ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("orbitops"),
		postgres.WithUsername("orbitops"),
		postgres.WithPassword("orbitops"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := postgresContainer.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	databaseURL, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	runtime, err := app.NewWithDependencies(ctx, app.Config{
		DatabaseURL:     databaseURL,
		LocalActorID:    "kind-developer",
		LocalClusterRef: kindCluster,
		LocalNamespace:  kindNamespace,
		MigrateOnBoot:   true,
	}, app.Dependencies{
		RuntimeSource:     adapter,
		RecoveryPublisher: adapter,
	})
	if err != nil {
		t.Fatalf("start control plane: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close control plane: %v", err)
		}
	})
	server := httptest.NewServer(runtime.Handler())
	t.Cleanup(server.Close)

	db, err := sqlx.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open worker database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close worker database: %v", err)
		}
	})
	operations := operation.New(db)
	releases := delivery.New(db, operations, projectauth.New(db))
	runner, err := worker.New(worker.Config{
		WorkerID:         "kind-worker",
		LeaseDuration:    5 * time.Second,
		OperationTimeout: 45 * time.Second,
	}, operations, releases, adapter)
	if err != nil {
		t.Fatalf("create worker runner: %v", err)
	}

	return &kindControlPlane{
		databaseURL: databaseURL,
		server:      server, runner: &kindQueueRunner{address: redisAddress(t), runner: runner, operations: operations, db: db}, operations: operations, releases: releases,
	}
}

func redisAddress(t *testing.T) string { _, address := testsupport.StartRedis(t); return address }

// kindQueueRunner 只给既有验收提供等待边界：真正领取始终来自 Redis 中的指定消息。
// 数据库查询仅选择需要等待的结果，不调用 ClaimNext，也不直接触发 Runner。
type kindQueueRunner struct {
	address    string
	runner     *worker.Runner
	operations *operation.Module
	db         *sqlx.DB
}

func (r *kindQueueRunner) RunOnce(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 70*time.Second)
	defer cancel()
	var ids []uuid.UUID
	if err := r.db.SelectContext(ctx, &ids, `SELECT id FROM operations WHERE status IN ('pending','running','cancel_requested')`); err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}
	service, err := dispatch.New(dispatch.Config{RedisAddress: r.address, Concurrency: 4, PollInterval: 50 * time.Millisecond, RepairInterval: 100 * time.Millisecond,
		ConsumptionGrace: time.Second, TaskTimeout: 60 * time.Second, ShutdownTimeout: time.Second}, r.operations, r.runner)
	if err != nil {
		return false, err
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		complete := true
		for _, id := range ids {
			current, err := r.operations.Get(ctx, id)
			if err != nil {
				return true, err
			}
			if current.Status == operation.StatusPending || current.Status == operation.StatusRunning || current.Status == operation.StatusCancelRequested {
				complete = false
			}
		}
		if complete {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (e *kindControlPlane) acceptRelease(
	t *testing.T,
	suffix string,
	image string,
) releaseAcceptance {
	t.Helper()
	project := e.postJSON(t, "/api/v1/projects", "project-"+suffix,
		fmt.Sprintf(`{"name":"Project %s","slug":"project-%s"}`, suffix, suffix))
	projectID := decodeID(t, project, "project")
	application := e.postJSON(
		t,
		"/api/v1/projects/"+projectID+"/applications",
		"application-"+suffix,
		fmt.Sprintf(`{"name":"Application %s","slug":"application-%s"}`, suffix, suffix),
	)
	applicationID := decodeID(t, application, "application")
	target := e.postJSON(
		t,
		"/api/v1/applications/"+applicationID+"/deployment-targets",
		"target-"+suffix,
		`{"stage":"development","replicas":1,"containerPort":8080}`,
	)
	targetID := decodeID(t, target, "target")
	releaseResponse := e.postJSON(
		t,
		"/api/v1/deployment-targets/"+targetID+"/releases",
		"release-"+suffix,
		fmt.Sprintf(`{"imageReference":%q}`, image),
	)
	defer releaseResponse.Body.Close()
	var document struct {
		Release struct {
			ID                  string  `json:"id"`
			RollbackOfReleaseID *string `json:"rollbackOfReleaseId"`
		} `json:"release"`
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if err := json.NewDecoder(releaseResponse.Body).Decode(&document); err != nil {
		t.Fatalf("decode release acceptance: %v", err)
	}
	return releaseAcceptance{
		TargetID:            targetID,
		ReleaseID:           document.Release.ID,
		OperationID:         document.Operation.ID,
		RollbackOfReleaseID: document.Release.RollbackOfReleaseID,
	}
}

func decodeReleaseAcceptance(t *testing.T, response *http.Response) releaseAcceptance {
	t.Helper()
	defer response.Body.Close()
	var document struct {
		Release struct {
			ID                  string  `json:"id"`
			DeploymentTargetID  string  `json:"deploymentTargetId"`
			RollbackOfReleaseID *string `json:"rollbackOfReleaseId"`
		} `json:"release"`
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode release acceptance: %v", err)
	}
	return releaseAcceptance{
		TargetID: document.Release.DeploymentTargetID, ReleaseID: document.Release.ID,
		OperationID: document.Operation.ID, RollbackOfReleaseID: document.Release.RollbackOfReleaseID,
	}
}

func (e *kindControlPlane) postJSON(
	t *testing.T,
	path string,
	idempotencyKey string,
	body string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		e.server.URL+path,
		bytes.NewBufferString(body),
	)
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if response.StatusCode != http.StatusCreated {
		defer response.Body.Close()
		var failure any
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("POST %s status = %d, want 201; body = %#v", path, response.StatusCode, failure)
	}
	return response
}

func (e *kindControlPlane) postCommand(
	t *testing.T,
	path string,
	idempotencyKey string,
	wantStatus int,
) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build POST %s: %v", path, err)
	}
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("POST %s status = %d, want %d", path, response.StatusCode, wantStatus)
	}
}

func decodeID(t *testing.T, response *http.Response, resource string) string {
	t.Helper()
	defer response.Body.Close()
	var document struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode %s: %v", resource, err)
	}
	if _, err := uuid.Parse(document.ID); err != nil {
		t.Fatalf("%s id = %q, want UUID: %v", resource, document.ID, err)
	}
	return document.ID
}

func (e *kindControlPlane) getOperation(t *testing.T, operationID string) operationResponse {
	t.Helper()
	var document operationResponse
	e.getJSON(t, "/api/v1/operations/"+operationID, &document)
	return document
}

func (e *kindControlPlane) getReleaseDiagnostics(
	t *testing.T,
	releaseID string,
) releaseDiagnosticResponse {
	t.Helper()
	var document releaseDiagnosticResponse
	e.getJSON(t, "/api/v1/releases/"+releaseID+"/diagnostics", &document)
	return document
}

func (e *kindControlPlane) getJSON(t *testing.T, path string, destination any) {
	t.Helper()
	response, err := e.server.Client().Get(e.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
}

func eventuallyOperationStatus(
	t *testing.T,
	environment *kindControlPlane,
	operationID string,
	want operation.OperationStatus,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if current := environment.getOperation(t, operationID); current.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s did not reach %s", operationID, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func publishRequest(lease operation.Lease, release delivery.Release) worker.PublishRequest {
	return worker.PublishRequest{
		OperationID:        lease.OperationID,
		AttemptID:          lease.AttemptID,
		ReleaseID:          release.ID,
		ProjectID:          release.TargetSnapshot.ProjectID,
		ApplicationID:      release.TargetSnapshot.ApplicationID,
		DeploymentTargetID: release.DeploymentTargetID,
		ImageReference:     release.ImageReference,
		Stage:              release.TargetSnapshot.Stage,
		ClusterRef:         release.TargetSnapshot.ClusterRef,
		Namespace:          release.TargetSnapshot.Namespace,
		Replicas:           release.TargetSnapshot.Replicas,
		ContainerPort:      release.TargetSnapshot.ContainerPort,
	}
}
