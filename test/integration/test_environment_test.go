package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/app"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type testEnvironment struct {
	server              *httptest.Server
	databaseURL         string
	dependencies        app.Dependencies
	postgresContainerID string
}

// serverForActor 使用同一数据库启动另一个本地身份，用于端到端验证项目授权。
func (e *testEnvironment) serverForActor(t *testing.T, actorID string) *httptest.Server {
	t.Helper()

	runtime, err := app.NewWithDependencies(context.Background(), app.Config{
		DatabaseURL:     e.databaseURL,
		LocalActorID:    actorID,
		LocalClusterRef: "kind-orbitops-s1",
		LocalNamespace:  "orbitops-s1",
		MigrateOnBoot:   false,
	}, e.dependencies)
	if err != nil {
		t.Fatalf("start OrbitOps for actor %q: %v", actorID, err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close OrbitOps for actor %q: %v", actorID, err)
		}
	})

	server := httptest.NewServer(runtime.Handler())
	t.Cleanup(server.Close)
	return server
}

type projectDocument struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

type applicationDocument struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

type deploymentTargetDocument struct {
	ProjectID     string    `json:"-"`
	ID            string    `json:"id"`
	ApplicationID string    `json:"applicationId"`
	Stage         string    `json:"stage"`
	ClusterRef    string    `json:"clusterRef"`
	Namespace     string    `json:"namespace"`
	Replicas      int       `json:"replicas"`
	ContainerPort int       `json:"containerPort"`
	CreatedBy     string    `json:"createdBy"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type releaseDocument struct {
	ID                  string                `json:"id"`
	DeploymentTargetID  string                `json:"deploymentTargetId"`
	ImageReference      string                `json:"imageReference"`
	TargetSnapshot      releaseTargetSnapshot `json:"targetSnapshot"`
	RollbackOfReleaseID *string               `json:"rollbackOfReleaseId"`
	CreatedBy           string                `json:"createdBy"`
	CreatedAt           time.Time             `json:"createdAt"`
}

type releaseDetailDocument struct {
	Release             releaseDocument              `json:"release"`
	SnapshotDifferences []snapshotDifferenceDocument `json:"snapshotDifferences"`
	ReleaseOperation    operationDocument            `json:"releaseOperation"`
	AuditTimeline       []auditRecordDocument        `json:"auditTimeline"`
}

type snapshotDifferenceDocument struct {
	Field        string `json:"field"`
	ReleaseValue string `json:"releaseValue"`
	CurrentValue string `json:"currentValue"`
}

type auditRecordDocument struct {
	ID         string         `json:"id"`
	ActorID    string         `json:"actorId"`
	ActorKind  string         `json:"actorKind"`
	Action     string         `json:"action"`
	TargetType string         `json:"targetType"`
	TargetID   string         `json:"targetId"`
	Summary    map[string]any `json:"summary"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type operationSummaryDocument struct {
	ID           string                                  `json:"id"`
	Status       releaseoperation.ReleaseOperationStatus `json:"status"`
	AttemptCount int                                     `json:"attemptCount"`
	ErrorCode    *string                                 `json:"errorCode"`
	ErrorSummary *string                                 `json:"errorSummary"`
	QueuedAt     time.Time                               `json:"queuedAt"`
	StartedAt    *time.Time                              `json:"startedAt"`
	FinishedAt   *time.Time                              `json:"finishedAt"`
}

type releaseHistoryItemDocument struct {
	Release          releaseDocument          `json:"release"`
	ReleaseOperation operationSummaryDocument `json:"releaseOperation"`
}

type releaseHistoryPageDocument struct {
	Items      []releaseHistoryItemDocument `json:"items"`
	NextCursor *string                      `json:"nextCursor"`
}

type releaseTargetSnapshot struct {
	ProjectID     string `json:"projectId"`
	ApplicationID string `json:"applicationId"`
	Stage         string `json:"stage"`
	ClusterRef    string `json:"clusterRef"`
	Namespace     string `json:"namespace"`
	Replicas      int    `json:"replicas"`
	ContainerPort int    `json:"containerPort"`
}

type operationDocument struct {
	ID                  string                                  `json:"id"`
	Type                string                                  `json:"type"`
	ReleaseID           string                                  `json:"releaseId"`
	DeploymentTargetID  string                                  `json:"deploymentTargetId"`
	CreatedBy           string                                  `json:"createdBy"`
	IdempotencyKey      string                                  `json:"idempotencyKey"`
	Status              releaseoperation.ReleaseOperationStatus `json:"status"`
	AttemptCount        int                                     `json:"attemptCount"`
	AutomaticRetryCount int                                     `json:"automaticRetryCount"`
	RecoveryRequired    bool                                    `json:"recoveryRequired"`
	ErrorCode           *string                                 `json:"errorCode"`
	ErrorSummary        *string                                 `json:"errorSummary"`
	RetryDisposition    *string                                 `json:"retryDisposition"`
	QueuedAt            time.Time                               `json:"queuedAt"`
	AvailableAt         time.Time                               `json:"availableAt"`
	CreatedAt           time.Time                               `json:"createdAt"`
	UpdatedAt           time.Time                               `json:"updatedAt"`
	StartedAt           *time.Time                              `json:"startedAt"`
	FinishedAt          *time.Time                              `json:"finishedAt"`
	Attempts            []operationAttemptDocument              `json:"attempts"`
}

type operationAttemptDocument struct {
	ID               string                         `json:"id"`
	Number           int                            `json:"number"`
	WorkerID         string                         `json:"workerId"`
	Status           releaseoperation.AttemptStatus `json:"status"`
	ErrorCode        *string                        `json:"errorCode"`
	ErrorSummary     *string                        `json:"errorSummary"`
	RetryDisposition *string                        `json:"retryDisposition"`
	StartedAt        time.Time                      `json:"startedAt"`
	FinishedAt       *time.Time                     `json:"finishedAt"`
}

type releaseAcceptanceDocument struct {
	Release          releaseDocument   `json:"release"`
	ReleaseOperation operationDocument `json:"releaseOperation"`
}

func newTestEnvironment(t *testing.T) *testEnvironment {
	return newTestEnvironmentWithDependencies(t, app.Dependencies{})
}

func newTestEnvironmentWithDependencies(
	t *testing.T,
	dependencies app.Dependencies,
) *testEnvironment {
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
		LocalActorID:    "local-developer",
		LocalClusterRef: "kind-orbitops-s1",
		LocalNamespace:  "orbitops-s1",
		MigrateOnBoot:   true,
	}, dependencies)
	if err != nil {
		t.Fatalf("start OrbitOps: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close OrbitOps: %v", err)
		}
	})

	server := httptest.NewServer(runtime.Handler())
	t.Cleanup(server.Close)

	return &testEnvironment{
		server: server, databaseURL: databaseURL, dependencies: dependencies,
		postgresContainerID: postgresContainer.GetContainerID(),
	}
}

func (e *testEnvironment) postProject(
	t *testing.T,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()
	return e.postJSON(t, "/api/v1/projects", idempotencyKey, requestBody)
}

func (e *testEnvironment) postJSON(
	t *testing.T,
	path string,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		e.server.URL+path,
		bytes.NewBufferString(requestBody),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	return response
}

func (e *testEnvironment) putJSON(
	t *testing.T,
	path string,
	idempotencyKey string,
	requestBody string,
) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPut,
		e.server.URL+path,
		bytes.NewBufferString(requestBody),
	)
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	return response
}

func (e *testEnvironment) get(t *testing.T, path string) *http.Response {
	t.Helper()

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		e.server.URL+path,
		nil,
	)
	if err != nil {
		t.Fatalf("build GET request: %v", err)
	}

	response, err := e.server.Client().Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	return response
}

func decodeProject(t *testing.T, response *http.Response) projectDocument {
	t.Helper()

	var project projectDocument
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatalf("decode project response: %v", err)
	}

	return project
}

func decodeApplication(t *testing.T, response *http.Response) applicationDocument {
	t.Helper()

	var application applicationDocument
	if err := json.NewDecoder(response.Body).Decode(&application); err != nil {
		t.Fatalf("decode application response: %v", err)
	}

	return application
}

func decodeDeploymentTarget(
	t *testing.T,
	response *http.Response,
) deploymentTargetDocument {
	t.Helper()

	var target deploymentTargetDocument
	if err := json.NewDecoder(response.Body).Decode(&target); err != nil {
		t.Fatalf("decode deployment target response: %v", err)
	}

	return target
}

func decodeReleaseAcceptance(
	t *testing.T,
	response *http.Response,
) releaseAcceptanceDocument {
	t.Helper()

	var acceptance releaseAcceptanceDocument
	if err := json.NewDecoder(response.Body).Decode(&acceptance); err != nil {
		t.Fatalf("decode release acceptance response: %v", err)
	}

	return acceptance
}

func decodeRelease(t *testing.T, response *http.Response) releaseDocument {
	t.Helper()

	return decodeReleaseDetail(t, response).Release
}

func decodeReleaseDetail(t *testing.T, response *http.Response) releaseDetailDocument {
	t.Helper()

	var detail releaseDetailDocument
	if err := json.NewDecoder(response.Body).Decode(&detail); err != nil {
		t.Fatalf("decode release response: %v", err)
	}
	return detail
}

func decodeReleaseOperation(t *testing.T, response *http.Response) operationDocument {
	t.Helper()

	var operation operationDocument
	if err := json.NewDecoder(response.Body).Decode(&operation); err != nil {
		t.Fatalf("decode operation response: %v", err)
	}

	return operation
}
