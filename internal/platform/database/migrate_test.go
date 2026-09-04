package database

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func TestProjectMemberMigrationBackfillsExistingCreators(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(
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
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	databaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	// 先停在 S1 最终 Schema，再插入历史项目，以验证真实升级路径而非空库初始化。
	migrateDatabaseToVersion(t, databaseURL, 8)
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	projectID := uuid.New()
	createdAt := time.Now().UTC()
	if _, err := database.ExecContext(
		ctx,
		`INSERT INTO projects (id, name, slug, created_by, created_at)
		 VALUES ($1, 'Existing', 'existing', 'existing-owner', $2)`,
		projectID,
		createdAt,
	); err != nil {
		t.Fatalf("insert S1 project: %v", err)
	}

	if err := Migrate(databaseURL); err != nil {
		t.Fatalf("upgrade database: %v", err)
	}

	var role string
	if err := database.QueryRowContext(
		ctx,
		`SELECT role FROM project_members WHERE project_id = $1 AND actor_id = 'existing-owner'`,
		projectID,
	).Scan(&role); err != nil {
		t.Fatalf("load backfilled project owner: %v", err)
	}
	if role != "owner" {
		t.Fatalf("backfilled role = %q, want owner", role)
	}
}

func TestReliableOperationMigrationBackfillsSchedulingState(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(
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
		if err := container.Terminate(context.Background()); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	databaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get PostgreSQL connection string: %v", err)
	}

	// 在 S2 迁移前构造一条真实失败记录，确保调度字段和错误术语均可安全回填。
	migrateDatabaseToVersion(t, databaseURL, 9)
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	projectID := uuid.New()
	applicationID := uuid.New()
	targetID := uuid.New()
	releaseID := uuid.New()
	operationID := uuid.New()
	attemptID := uuid.New()
	createdAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects (id, name, slug, created_by, created_at)
		  VALUES ($1, 'Existing', 'existing-operation', 'owner', $2)`, []any{projectID, createdAt}},
		{`INSERT INTO applications (id, project_id, name, slug, created_by, created_at)
		  VALUES ($1, $2, 'API', 'api', 'owner', $3)`, []any{applicationID, projectID, createdAt}},
		{`INSERT INTO deployment_targets
		  (id, application_id, stage, cluster_ref, namespace, replicas, container_port,
		   created_by, created_at, updated_at)
		  VALUES ($1, $2, 'development', 'kind-orbitops-s1', 'orbitops-s1', 1, 8080,
		          'owner', $3, $3)`, []any{targetID, applicationID, createdAt}},
		{`INSERT INTO releases
		  (id, deployment_target_id, image_reference, target_snapshot, created_by, created_at)
		  VALUES ($1, $2, 'registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		          '{}', 'owner', $3)`, []any{releaseID, targetID, createdAt}},
		{`INSERT INTO operations
		  (id, operation_type, release_id, actor_id, idempotency_key, status,
		   attempt_count, error_category, error_summary, created_at, updated_at,
		   started_at, finished_at)
		  VALUES ($1, 'release.deploy', $2, 'owner', 'existing-operation', 'failed',
		          1, 'image_pull_failed', 'image unavailable', $3, $3, $3, $3)`,
			[]any{operationID, releaseID, createdAt}},
		{`INSERT INTO operation_attempts
		  (id, operation_id, attempt_number, worker_id, status, error_category,
		   error_summary, started_at, finished_at)
		  VALUES ($1, $2, 1, 'worker-one', 'failed', 'image_pull_failed',
		          'image unavailable', $3, $3)`, []any{attemptID, operationID, createdAt}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed pre-S2 operation: %v", err)
		}
	}

	if err := Migrate(databaseURL); err != nil {
		t.Fatalf("upgrade database: %v", err)
	}

	var gotTargetID uuid.UUID
	var queuedAt, availableAt time.Time
	var errorCode, disposition string
	var recoveryRequired bool
	if err := database.QueryRowContext(
		ctx,
		`SELECT deployment_target_id, queued_at, available_at, error_code,
		        retry_disposition, recovery_required
		 FROM operations WHERE id = $1`,
		operationID,
	).Scan(
		&gotTargetID,
		&queuedAt,
		&availableAt,
		&errorCode,
		&disposition,
		&recoveryRequired,
	); err != nil {
		t.Fatalf("load migrated operation: %v", err)
	}
	if gotTargetID != targetID || !queuedAt.Equal(createdAt) || !availableAt.Equal(createdAt) {
		t.Errorf(
			"migrated scheduling state = target %s, queued %s, available %s",
			gotTargetID,
			queuedAt,
			availableAt,
		)
	}
	if errorCode != "image_pull_failed" || disposition != "non_retryable" {
		t.Errorf("migrated failure = %q/%q, want image_pull_failed/non_retryable", errorCode, disposition)
	}
	if recoveryRequired {
		t.Error("migrated terminal operation unexpectedly requires recovery")
	}
	var rollbackOfReleaseID sql.NullString
	if err := database.QueryRowContext(
		ctx,
		`SELECT rollback_of_release_id FROM releases WHERE id = $1`,
		releaseID,
	).Scan(&rollbackOfReleaseID); err != nil {
		t.Fatalf("load migrated release lineage: %v", err)
	}
	if rollbackOfReleaseID.Valid {
		t.Errorf("ordinary migrated release unexpectedly became rollback of %q", rollbackOfReleaseID.String)
	}
}

func migrateDatabaseToVersion(t *testing.T, databaseURL string, version uint) {
	t.Helper()
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		t.Fatalf("open migrations: %v", err)
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open migration database: %v", err)
	}
	driver, err := migratepostgres.WithInstance(database, &migratepostgres.Config{})
	if err != nil {
		_ = database.Close()
		t.Fatalf("create migration driver: %v", err)
	}
	migrator, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		_ = database.Close()
		t.Fatalf("create migrator: %v", err)
	}
	if err := migrator.Migrate(version); err != nil {
		_, _ = migrator.Close()
		t.Fatalf("migrate database to version %d: %v", version, err)
	}
	if sourceErr, databaseErr := migrator.Close(); sourceErr != nil || databaseErr != nil {
		t.Fatalf("close migrator: source=%v database=%v", sourceErr, databaseErr)
	}
}
