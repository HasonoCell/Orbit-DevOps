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
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestUserCanCreateProject(t *testing.T) {
	t.Parallel()

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

	requestBody := []byte(`{"name":"Platform","slug":"platform"}`)
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		server.URL+"/api/v1/projects",
		bytes.NewReader(requestBody),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-platform-project")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}

	var project struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Slug      string    `json:"slug"`
		CreatedBy string    `json:"createdBy"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.NewDecoder(response.Body).Decode(&project); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if _, err := uuid.Parse(project.ID); err != nil {
		t.Errorf("id = %q, want UUID: %v", project.ID, err)
	}
	if project.Name != "Platform" {
		t.Errorf("name = %q, want %q", project.Name, "Platform")
	}
	if project.Slug != "platform" {
		t.Errorf("slug = %q, want %q", project.Slug, "platform")
	}
	if project.CreatedBy != "local-developer" {
		t.Errorf("createdBy = %q, want %q", project.CreatedBy, "local-developer")
	}
	if project.CreatedAt.IsZero() {
		t.Error("createdAt is zero")
	}
}
