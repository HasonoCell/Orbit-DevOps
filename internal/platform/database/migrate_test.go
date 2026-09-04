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
