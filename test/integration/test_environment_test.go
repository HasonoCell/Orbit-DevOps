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
		DatabaseURL:   databaseURL,
		LocalActorID:  "local-developer",
		MigrateOnBoot: true,
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

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		e.server.URL+"/api/v1/projects",
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

func decodeProject(t *testing.T, response *http.Response) projectDocument {
	t.Helper()

	var project projectDocument
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatalf("decode project response: %v", err)
	}

	return project
}
