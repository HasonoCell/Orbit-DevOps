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
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type testEnvironment struct {
	server *httptest.Server
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
	ID                 string                `json:"id"`
	DeploymentTargetID string                `json:"deploymentTargetId"`
	ImageReference     string                `json:"imageReference"`
	TargetSnapshot     releaseTargetSnapshot `json:"targetSnapshot"`
	CreatedBy          string                `json:"createdBy"`
	CreatedAt          time.Time             `json:"createdAt"`
}

type releaseTargetSnapshot struct {
	ApplicationID string `json:"applicationId"`
	Stage         string `json:"stage"`
	ClusterRef    string `json:"clusterRef"`
	Namespace     string `json:"namespace"`
	Replicas      int    `json:"replicas"`
	ContainerPort int    `json:"containerPort"`
}

type operationDocument struct {
	ID             string     `json:"id"`
	Type           string     `json:"type"`
	ReleaseID      string     `json:"releaseId"`
	CreatedBy      string     `json:"createdBy"`
	IdempotencyKey string     `json:"idempotencyKey"`
	Status         string     `json:"status"`
	AttemptCount   int        `json:"attemptCount"`
	ErrorCategory  *string    `json:"errorCategory"`
	ErrorSummary   *string    `json:"errorSummary"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	StartedAt      *time.Time `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
}

type releaseAcceptanceDocument struct {
	Release   releaseDocument   `json:"release"`
	Operation operationDocument `json:"operation"`
}

func newTestEnvironment(t *testing.T) *testEnvironment {
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

	runtime, err := app.New(ctx, app.Config{
		DatabaseURL:     databaseURL,
		LocalActorID:    "local-developer",
		LocalClusterRef: "kind-orbitops-s1",
		LocalNamespace:  "orbitops-s1",
		MigrateOnBoot:   true,
	})
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

	return &testEnvironment{server: server}
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

	var release releaseDocument
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		t.Fatalf("decode release response: %v", err)
	}

	return release
}

func decodeOperation(t *testing.T, response *http.Response) operationDocument {
	t.Helper()

	var operation operationDocument
	if err := json.NewDecoder(response.Body).Decode(&operation); err != nil {
		t.Fatalf("decode operation response: %v", err)
	}

	return operation
}
