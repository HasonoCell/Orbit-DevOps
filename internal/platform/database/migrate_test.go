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
		postgres.WithDatabase("orbitdevops"),
		postgres.WithUsername("orbitdevops"),
		postgres.WithPassword("orbitdevops"),
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
		`SELECT role FROM legacy_project_members WHERE project_id = $1 AND actor_id = 'existing-owner'`,
		projectID,
	).Scan(&role); err != nil {
		t.Fatalf("load backfilled project owner: %v", err)
	}
	if role != "owner" {
		t.Fatalf("backfilled role = %q, want owner", role)
	}
}

func TestReliableReleaseOperationMigrationBackfillsSchedulingState(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(
		ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("orbitdevops"),
		postgres.WithUsername("orbitdevops"),
		postgres.WithPassword("orbitdevops"),
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
	releaseOperationID := uuid.New()
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
		  VALUES ($1, $2, 'development', 'kind-orbit-devops-s1', 'orbit-devops-s1', 1, 8080,
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
			[]any{releaseOperationID, releaseID, createdAt}},
		{`INSERT INTO operation_attempts
		  (id, operation_id, attempt_number, worker_id, status, error_category,
		   error_summary, started_at, finished_at)
		  VALUES ($1, $2, 1, 'worker-one', 'failed', 'image_pull_failed',
		          'image unavailable', $3, $3)`, []any{attemptID, releaseOperationID, createdAt}},
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
		 FROM release_operations WHERE id = $1`,
		releaseOperationID,
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

// 调度术语迁移只重命名已有 Schema，不重建表或丢失既有升级路径。
func TestDispatchTerminologyMigrationRenamesSchema(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(
		ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("orbitdevops"),
		postgres.WithUsername("orbitdevops"),
		postgres.WithPassword("orbitdevops"),
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

	// 先停在 Q1 原始 Schema，确保迁移适用于已经运行过 000013/000014 的数据库。
	migrateDatabaseToVersion(t, databaseURL, 14)
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	projectID := uuid.New()
	applicationID := uuid.New()
	targetID := uuid.New()
	releaseID := uuid.New()
	releaseOperationID := uuid.New()
	dispatchID := uuid.New()
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects (id, name, slug, created_by, created_at)
		  VALUES ($1, 'Dispatch migration', 'dispatch-migration', 'owner', $2)`, []any{projectID, createdAt}},
		{`INSERT INTO applications (id, project_id, name, slug, created_by, created_at)
		  VALUES ($1, $2, 'API', 'api', 'owner', $3)`, []any{applicationID, projectID, createdAt}},
		{`INSERT INTO deployment_targets
		  (id, application_id, stage, cluster_ref, namespace, replicas, container_port,
		   created_by, created_at, updated_at)
		  VALUES ($1, $2, 'development', 'kind-orbit-devops-s1', 'orbit-devops-s1', 1, 8080,
		          'owner', $3, $3)`, []any{targetID, applicationID, createdAt}},
		{`INSERT INTO releases
		  (id, deployment_target_id, image_reference, target_snapshot, created_by, created_at)
		  VALUES ($1, $2, 'registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		          '{}', 'owner', $3)`, []any{releaseID, targetID, createdAt}},
		{`INSERT INTO operations
		  (id, operation_type, release_id, deployment_target_id, actor_id, idempotency_key,
		   status, queued_at, available_at, created_at, updated_at, dispatch_generation)
		  VALUES ($1, 'release.deploy', $2, $3, 'owner', 'dispatch-migration',
		          'pending', $4, $4, $4, $4, 7)`, []any{releaseOperationID, releaseID, targetID, createdAt}},
		{`INSERT INTO operation_dispatches
		  (id, operation_id, generation, version, reason, expected_attempt_count, state,
		   available_at, next_dispatch_at, delivery_count, created_at, updated_at)
		  VALUES ($1, $2, 7, 1, 'accepted', 0, 'published', $3, $3, 2, $3, $3)`,
			[]any{dispatchID, releaseOperationID, createdAt}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed Q1 dispatch: %v", err)
		}
	}
	if err := Migrate(databaseURL); err != nil {
		t.Fatalf("upgrade dispatch terminology: %v", err)
	}

	if _, err := database.ExecContext(ctx, `SELECT current_dispatch_sequence FROM release_operations LIMIT 0`); err != nil {
		t.Fatalf("query renamed operation sequence: %v", err)
	}
	if _, err := database.ExecContext(ctx, `SELECT sequence, protocol_version, dispatch_reason, reservation_count FROM operation_dispatches LIMIT 0`); err != nil {
		t.Fatalf("query renamed dispatch fields: %v", err)
	}
	var oldColumnCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND (
			(table_name = 'release_operations' AND column_name = 'dispatch_generation') OR
			(table_name = 'operation_dispatches' AND column_name IN ('generation', 'version', 'reason', 'delivery_count'))
		)`).Scan(&oldColumnCount); err != nil {
		t.Fatalf("inspect old dispatch fields: %v", err)
	}
	if oldColumnCount != 0 {
		t.Fatalf("old dispatch fields remain: %d", oldColumnCount)
	}
	var renamedConstraintCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conname IN (
			'release_operations_current_dispatch_sequence_check',
			'operation_dispatches_sequence_check',
			'operation_dispatches_reservation_count_check',
			'operation_dispatches_operation_id_sequence_key'
		)`).Scan(&renamedConstraintCount); err != nil {
		t.Fatalf("inspect renamed dispatch constraints: %v", err)
	}
	if renamedConstraintCount != 4 {
		t.Fatalf("renamed dispatch constraints = %d, want 4", renamedConstraintCount)
	}
	var operationSequence, dispatchSequence int64
	var protocolVersion, reservationCount int
	var dispatchReason string
	if err := database.QueryRowContext(ctx, `SELECT o.current_dispatch_sequence,
		d.sequence, d.protocol_version, d.dispatch_reason, d.reservation_count
		FROM release_operations o JOIN operation_dispatches d ON d.operation_id = o.id
		WHERE o.id = $1 AND d.id = $2`, releaseOperationID, dispatchID).Scan(
		&operationSequence,
		&dispatchSequence,
		&protocolVersion,
		&dispatchReason,
		&reservationCount,
	); err != nil {
		t.Fatalf("load renamed dispatch data: %v", err)
	}
	if operationSequence != 7 || dispatchSequence != 7 || protocolVersion != 1 || dispatchReason != "accepted" || reservationCount != 2 {
		t.Fatalf("renamed dispatch data = operation sequence %d, dispatch sequence %d, version %d, reason %q, reservations %d",
			operationSequence, dispatchSequence, protocolVersion, dispatchReason, reservationCount)
	}
	var newTableExists, oldTableMissing bool
	if err := database.QueryRowContext(ctx, `SELECT
		to_regclass('public.release_operations') IS NOT NULL,
		to_regclass('public.operations') IS NULL`).Scan(&newTableExists, &oldTableMissing); err != nil {
		t.Fatalf("inspect release operation table rename: %v", err)
	}
	if !newTableExists || !oldTableMissing {
		t.Fatalf("release operation tables = new exists %v, old missing %v", newTableExists, oldTableMissing)
	}
	var staleObjectCount int
	if err := database.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM pg_constraint
		 WHERE conrelid = 'release_operations'::regclass AND conname LIKE 'operations_%') +
		(SELECT count(*) FROM pg_indexes
		 WHERE schemaname = current_schema() AND tablename = 'release_operations'
		   AND indexname LIKE 'operations_%')`).Scan(&staleObjectCount); err != nil {
		t.Fatalf("inspect release operation schema object names: %v", err)
	}
	if staleObjectCount != 0 {
		t.Fatalf("stale operation schema object names remain: %d", staleObjectCount)
	}

	// Down migration 必须只恢复旧术语；既有调度事实随后仍可再次升级。
	migrateDatabaseToVersion(t, databaseURL, 14)
	var operationGeneration, dispatchGeneration int64
	var version, deliveryCount int
	var reason string
	if err := database.QueryRowContext(ctx, `SELECT o.dispatch_generation,
		d.generation, d.version, d.reason, d.delivery_count
		FROM operations o JOIN operation_dispatches d ON d.operation_id = o.id
		WHERE o.id = $1 AND d.id = $2`, releaseOperationID, dispatchID).Scan(
		&operationGeneration,
		&dispatchGeneration,
		&version,
		&reason,
		&deliveryCount,
	); err != nil {
		t.Fatalf("load dispatch data after down migration: %v", err)
	}
	if operationGeneration != 7 || dispatchGeneration != 7 || version != 1 || reason != "accepted" || deliveryCount != 2 {
		t.Fatalf("restored dispatch data = operation generation %d, dispatch generation %d, version %d, reason %q, deliveries %d",
			operationGeneration, dispatchGeneration, version, reason, deliveryCount)
	}
	if err := Migrate(databaseURL); err != nil {
		t.Fatalf("reapply dispatch terminology migration: %v", err)
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
