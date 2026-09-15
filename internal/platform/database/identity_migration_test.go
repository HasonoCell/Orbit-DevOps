package database

import (
	"context"
	"database/sql"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// 此测试跨迁移/数据库约束 Seam；所有数据均属于独立且会销毁的 fixture。
func TestLocalIdentityMigrationPreservesLegacyAndGuardsRollback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("identity_migration_fixture"), postgres.WithUsername("fixture"),
		postgres.WithPassword("fixture"), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start migration fixture: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal("get fixture connection string")
	}
	migrateDatabaseToVersion(t, dsn, 20)
	db, err := sqlx.ConnectContext(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal("connect migration fixture")
	}
	t.Cleanup(func() { _ = db.Close() })
	projectID := uuid.New()
	if _, err := db.ExecContext(ctx, `INSERT INTO projects
		(id, name, slug, created_by, created_at) VALUES ($1, 'Legacy', 'legacy', 'historical-actor', clock_timestamp());
	`, projectID); err != nil {
		t.Fatal("seed legacy project")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_members
		(project_id, actor_id, role, created_by, created_at, updated_at)
		VALUES ($1, 'historical-actor', 'owner', 'historical-actor', clock_timestamp(), clock_timestamp())`, projectID); err != nil {
		t.Fatal("seed legacy member")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("upgrade local identity schema: %v", err)
	}
	var role string
	if err := db.GetContext(ctx, &role, `SELECT role FROM project_members
		WHERE project_id = $1 AND actor_id = 'historical-actor'`, projectID); err != nil || role != "owner" {
		t.Fatal("foundation migration prematurely cut over legacy authorization")
	}
	// 空身份可以 down/up；旧业务事实不能因此删除。
	migrateDatabaseToVersion(t, dsn, 20)
	if err := Migrate(dsn); err != nil {
		t.Fatalf("reapply empty identity migration: %v", err)
	}
	module, err := identity.New(db)
	if err != nil {
		t.Fatal("construct identity fixture")
	}
	user, err := module.InitializeAdmin(ctx, identity.InitializeAdminCommand{
		LoginName: "fixture-admin", DisplayName: "测试管理员", Password: "fixture-only!Migration-September-2026",
		MaintenanceRef: "fixture-migration-guard",
	})
	if err != nil {
		t.Fatalf("initialize migrated identity: %v", err)
	}
	// CHECK/FK 是真实数据库契约，不以 Go 生成类型代替数据库约束。
	if _, err := db.ExecContext(ctx, `UPDATE users SET auth_version = 0 WHERE id = $1`, user.ID); err == nil {
		t.Fatal("database accepted invalid authentication version")
	}
	if _, err := db.ExecContext(ctx, `UPDATE users SET platform_role = 'superuser' WHERE id = $1`, user.ID); err == nil {
		t.Fatal("database accepted undefined platform role")
	}
	if _, err := db.ExecContext(ctx, `UPDATE local_credentials SET login_name = 'NOT-NORMALIZED' WHERE user_id = $1`, user.ID); err == nil {
		t.Fatal("database accepted noncanonical local login name")
	}
	if err := identityRollback(dsn); err == nil {
		t.Fatal("down destroyed initialized identity data")
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-admin", Password: "fixture-only!Migration-September-2026", SourceIP: "127.0.0.1",
	}); err != nil {
		t.Fatalf("guarded down did not preserve credentials: %v", err)
	}
}

// identityRollback 使用独立连接；预期 down 失败后 fixture 保持 dirty，绝不 Force 绕过。
func identityRollback(dsn string) error {
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		_ = source.Close()
		return err
	}
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	if err != nil {
		_ = source.Close()
		_ = db.Close()
		return err
	}
	runner, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		_ = source.Close()
		_ = db.Close()
		return err
	}
	defer func() { _, _ = runner.Close() }()
	return runner.Migrate(20)
}
