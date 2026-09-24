package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestProductionTargetStageMigrationPreservesHistoryAndRejectsUnsafeDown(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("orbitdevops"), postgres.WithUsername("orbitdevops"),
		postgres.WithPassword("orbitdevops"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	databaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}
	migrateDatabaseToVersion(t, databaseURL, 25)
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	projectID, applicationID, developmentID, releaseID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id,name,slug,created_by,created_at) VALUES($1,'Demo','demo','owner',$2)`, []any{projectID, now}},
		{`INSERT INTO applications(id,project_id,name,slug,created_by,created_at) VALUES($1,$2,'API','api','owner',$3)`, []any{applicationID, projectID, now}},
		{`INSERT INTO deployment_targets(id,application_id,stage,cluster_ref,namespace,replicas,container_port,created_by,created_at,updated_at)
			VALUES($1,$2,'development','kind-orbit-devops-s1','orbit-devops-s1',1,8080,'owner',$3,$3)`, []any{developmentID, applicationID, now}},
		{`INSERT INTO releases(id,deployment_target_id,image_reference,target_snapshot,created_by,created_at)
			VALUES($1,$2,'registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',$3::jsonb,'owner',$4)`, []any{releaseID, developmentID, `{"stage":"development"}`, now}},
	} {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed development history: %v", err)
		}
	}
	if err := Migrate(databaseURL); err != nil {
		t.Fatalf("upgrade database: %v", err)
	}
	var gotTargetID uuid.UUID
	var snapshot string
	if err := database.QueryRowContext(ctx, `SELECT deployment_target_id,target_snapshot->>'stage' FROM releases WHERE id=$1`, releaseID).Scan(&gotTargetID, &snapshot); err != nil {
		t.Fatalf("read historical release: %v", err)
	}
	if gotTargetID != developmentID || snapshot != "development" {
		t.Fatalf("historical release changed: target=%s stage=%s", gotTargetID, snapshot)
	}

	productionID := uuid.New()
	if _, err := database.ExecContext(ctx, `INSERT INTO deployment_targets(id,application_id,stage,cluster_ref,namespace,replicas,container_port,created_by,created_at,updated_at)
		VALUES($1,$2,'production','kind-orbit-devops-s1','orbit-devops-s1',1,8080,'owner',$3,$3)`, productionID, applicationID, now); err != nil {
		t.Fatalf("create production target after migration: %v", err)
	}
	down, err := migrations.ReadFile("migrations/000026_add_production_target_stage.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(down)); err == nil || !strings.Contains(err.Error(), "production targets exist") {
		t.Fatalf("unsafe down result = %v, want explicit refusal", err)
	}
	var productionExists bool
	if err := database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_targets WHERE id=$1)`, productionID).Scan(&productionExists); err != nil || !productionExists {
		t.Fatalf("production target after refused down: exists=%t err=%v", productionExists, err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM deployment_targets WHERE id=$1`, productionID); err != nil {
		t.Fatalf("remove test production target: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(down)); err != nil {
		t.Fatalf("restore development-only check: %v", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO deployment_targets(id,application_id,stage,cluster_ref,namespace,replicas,container_port,created_by,created_at,updated_at)
		VALUES($1,$2,'production','kind-orbit-devops-s1','orbit-devops-s1',1,8080,'owner',$3,$3)`, productionID, applicationID, now); err == nil {
		t.Fatal("development-only check accepted production after down")
	}
}
