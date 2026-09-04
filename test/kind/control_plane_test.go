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
	"github.com/HasonoCell/OrbitOps/internal/kube"
	"github.com/HasonoCell/OrbitOps/internal/operation"
	"github.com/HasonoCell/OrbitOps/internal/projectauth"
	"github.com/HasonoCell/OrbitOps/internal/worker"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
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

		snapshot := environment.getRuntimeSnapshot(t, acceptance.TargetID)
		if snapshot.Source != "kubernetes" || snapshot.Freshness != "fresh" {
			t.Errorf("runtime source/freshness = %q/%q, want kubernetes/fresh", snapshot.Source, snapshot.Freshness)
		}
		if !snapshot.DeploymentExists || snapshot.ReadyReplicas != 1 {
			t.Errorf("runtime snapshot = %#v, want Ready deployment", snapshot)
		}
		if snapshot.ReleaseID == nil || *snapshot.ReleaseID != acceptance.ReleaseID {
			t.Errorf("runtime releaseId = %v, want %s", snapshot.ReleaseID, acceptance.ReleaseID)
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

		snapshot := environment.getRuntimeSnapshot(t, acceptance.TargetID)
		if snapshot.Freshness != "fresh" || !snapshot.DeploymentExists {
			t.Errorf("failure runtime snapshot = %#v, want fresh Kubernetes state", snapshot)
		}
		if len(snapshot.Pods) == 0 || snapshot.Pods[0].Reason == "" {
			t.Errorf("failure pods = %#v, want Kubernetes pull failure", snapshot.Pods)
		}
	})
}

type kindControlPlane struct {
	server *httptest.Server
	runner *worker.Runner
}

type releaseAcceptance struct {
	TargetID    string
	ReleaseID   string
	OperationID string
}

type operationResponse struct {
	Status       string                     `json:"status"`
	AttemptCount int                        `json:"attemptCount"`
	ErrorCode    *string                    `json:"errorCode"`
	Attempts     []operationAttemptResponse `json:"attempts"`
}

type operationAttemptResponse struct {
	Status string `json:"status"`
}

type runtimeSnapshotResponse struct {
	Source           string               `json:"source"`
	Freshness        string               `json:"freshness"`
	DeploymentExists bool                 `json:"deploymentExists"`
	ReleaseID        *string              `json:"releaseId"`
	ReadyReplicas    int                  `json:"readyReplicas"`
	Pods             []runtimePodResponse `json:"pods"`
}

type runtimePodResponse struct {
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
	}, app.Dependencies{RuntimeObserver: adapter})
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

	return &kindControlPlane{server: server, runner: runner}
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
			ID string `json:"id"`
		} `json:"release"`
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if err := json.NewDecoder(releaseResponse.Body).Decode(&document); err != nil {
		t.Fatalf("decode release acceptance: %v", err)
	}
	return releaseAcceptance{
		TargetID:    targetID,
		ReleaseID:   document.Release.ID,
		OperationID: document.Operation.ID,
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

func (e *kindControlPlane) getRuntimeSnapshot(
	t *testing.T,
	targetID string,
) runtimeSnapshotResponse {
	t.Helper()
	var document runtimeSnapshotResponse
	e.getJSON(t, "/api/v1/deployment-targets/"+targetID+"/runtime-snapshot", &document)
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
