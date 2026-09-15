package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/jmoiron/sqlx"
)

func initializeFixture(t *testing.T, module *identity.Module) identity.User {
	t.Helper()
	user, err := module.InitializeAdmin(context.Background(), identity.InitializeAdminCommand{
		LoginName: "fixture-admin", DisplayName: "测试管理员", Password: fixturePassword,
		MaintenanceRef: "fixture-security",
	})
	if err != nil {
		t.Fatalf("initialize fixture: %v", err)
	}
	return user
}

func loginFixture(t *testing.T, module *identity.Module) identity.LoginResult {
	t.Helper()
	result, err := module.LoginLocal(context.Background(), identity.LocalLoginCommand{
		LoginName: "fixture-admin", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("login fixture: %v", err)
	}
	return result
}

// fixtureSQL 仅改变独立容器中的测试事实；驱动 DETAIL 不进入输出。
func fixtureSQL(t *testing.T, db *sqlx.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal("configure isolated identity fixture")
	}
}

func TestFailedAuthenticationAuditDependencyFailsClosed(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	fixtureSQL(t, db, `CREATE FUNCTION block_failed_authentication_audit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.action = 'auth.login_failed' THEN RAISE EXCEPTION 'fixture audit unavailable'; END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER block_failed_authentication_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_failed_authentication_audit()`)
	command := identity.LocalLoginCommand{LoginName: "fixture-admin", Password: fixturePassword + "!", SourceIP: "127.0.0.1"}
	if _, err := module.LoginLocal(context.Background(), command); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("audit failure must not be reported as successfully recorded authentication failure: %v", err)
	}
	fixtureSQL(t, db, `DROP TRIGGER block_failed_authentication_audit ON audit_records;
		CREATE FUNCTION require_unproven_system_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.action = 'auth.login_failed' AND NEW.actor_kind <> 'system' THEN
		 RAISE EXCEPTION 'unproven actor must be system'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER require_unproven_system_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION require_unproven_system_audit()`)
	if _, err := module.LoginLocal(context.Background(), command); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("unproven login audit cannot use claimed user as Actor: %v", err)
	}
}

func TestConcurrentInitializationHasOneWinnerAndCannotResetMarker(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	start := make(chan struct{})
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			<-start
			_, err := module.InitializeAdmin(context.Background(), identity.InitializeAdminCommand{
				LoginName: "fixture-admin", DisplayName: "测试管理员", Password: fixturePassword,
				MaintenanceRef: "fixture-concurrent-init",
			})
			results <- err
		}()
	}
	close(start)
	winners := 0
	for i := 0; i < 6; i++ {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, identity.ErrAlreadyInitialized) {
			t.Fatalf("concurrent init returned unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("initialization winners = %d, want 1", winners)
	}
	fixtureSQL(t, db, `UPDATE users SET status = 'disabled'`)
	if _, err := module.InitializeAdmin(context.Background(), identity.InitializeAdminCommand{
		LoginName: "replacement-admin", DisplayName: "恢复不是初始化", Password: fixturePassword,
		MaintenanceRef: "fixture-not-recovery",
	}); !errors.Is(err, identity.ErrAlreadyInitialized) {
		t.Fatalf("missing effective admin reopened initialization: %v", err)
	}
	if _, err := db.Exec(`UPDATE identity_control SET initialized_at = NULL WHERE id = 1`); err == nil {
		t.Fatal("database allowed permanent initialization marker to be reset")
	}
}

func TestInitializationAuditFailureRollsBackUserCredentialAndMarker(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	fixtureSQL(t, db, `CREATE FUNCTION block_identity_initialization_audit() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN IF NEW.action = 'identity.initialize_admin' THEN
		RAISE EXCEPTION 'fixture initialization audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER block_identity_initialization_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_identity_initialization_audit()`)
	command := identity.InitializeAdminCommand{LoginName: "fixture-admin", DisplayName: "测试管理员",
		Password: fixturePassword, MaintenanceRef: "fixture-atomic-init"}
	if _, err := module.InitializeAdmin(context.Background(), command); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("blocked audit error = %v", err)
	}
	fixtureSQL(t, db, `DROP TRIGGER block_identity_initialization_audit ON audit_records`)
	if _, err := module.InitializeAdmin(context.Background(), command); err != nil {
		t.Fatalf("failed transaction left identity data or consumed marker: %v", err)
	}
	loginFixture(t, module)
}

// awaitLockWait 用数据库真实阻塞关系作为屏障，不以 sleep 猜测 goroutine 时序。
func awaitLockWait(t *testing.T, ctx context.Context, db *sqlx.DB, backendPID int) {
	t.Helper()
	for {
		var waiting bool
		if err := db.GetContext(ctx, &waiting, `SELECT cardinality(pg_blocking_pids($1)) > 0`, backendPID); err != nil {
			t.Fatal("gate did not reach the verified PostgreSQL lock barrier")
		}
		if waiting {
			return
		}
	}
}

func TestBusinessGateReadsNewRevocationAfterWaitingForControlLock(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	login := loginFixture(t, module)
	caller, err := module.ResolveSession(context.Background(), login.Token.CookieValue())
	if err != nil {
		t.Fatal("resolve fixture caller")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	revokeTx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin fixture revocation")
	}
	defer func() { _ = revokeTx.Rollback() }()
	var marker sql.NullTime
	if err := revokeTx.GetContext(ctx, &marker, `SELECT initialized_at FROM identity_control WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal("acquire fixture security lock")
	}
	if _, err := revokeTx.ExecContext(ctx, `UPDATE auth_sessions SET revoked_at = clock_timestamp() WHERE user_id = $1`, user.ID); err != nil {
		t.Fatal("stage fixture revocation")
	}
	gateTx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin fixture acceptance")
	}
	defer func() { _ = gateTx.Rollback() }()
	var backendPID int
	if err := gateTx.GetContext(ctx, &backendPID, `SELECT pg_backend_pid()`); err != nil {
		t.Fatal("locate acceptance backend")
	}
	var oldSnapshotHasSession bool
	if err := gateTx.GetContext(ctx, &oldSnapshotHasSession, `SELECT EXISTS (
		SELECT 1 FROM auth_sessions WHERE user_id = $1 AND revoked_at IS NULL)`, user.ID); err != nil || !oldSnapshotHasSession {
		t.Fatal("waiting transaction must begin with a pre-revocation snapshot")
	}
	result := make(chan error, 1)
	go func() {
		_, err := module.AuthorizeInTx(ctx, gateTx, caller)
		result <- err
	}()
	awaitLockWait(t, ctx, db, backendPID)
	if err := revokeTx.Commit(); err != nil {
		t.Fatal("commit fixture revocation")
	}
	if err := <-result; !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("business gate reused pre-wait authorization: %v", err)
	}
}

func TestBusinessGateRejectsRepeatableReadSnapshot(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	login := loginFixture(t, module)
	caller, err := module.ResolveSession(context.Background(), login.Token.CookieValue())
	if err != nil {
		t.Fatal("resolve fixture")
	}
	tx, err := db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal("begin repeatable read fixture")
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := module.AuthorizeInTx(context.Background(), tx, caller); !errors.Is(err, identity.ErrTransactionMode) {
		t.Fatalf("gate silently accepted unsupported snapshot semantics: %v", err)
	}
}

func TestPasswordRestrictionAndVersionAreReadAgainForOldCaller(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	login := loginFixture(t, module)
	caller, err := module.ResolveSession(context.Background(), login.Token.CookieValue())
	if err != nil {
		t.Fatal("resolve fixture")
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET must_change_password = true WHERE user_id = $1`, user.ID)
	current, err := module.CurrentUser(context.Background(), caller)
	if err != nil || !current.MustChangePassword {
		t.Fatalf("current password restriction was cached: %v", err)
	}
	tx, err := db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin restricted fixture")
	}
	_, err = module.AuthorizeInTx(context.Background(), tx, caller)
	_ = tx.Rollback()
	if !errors.Is(err, identity.ErrPasswordChangeRequired) {
		t.Fatalf("restricted account accepted business: %v", err)
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET must_change_password = false WHERE user_id = $1`, user.ID)
	fixtureSQL(t, db, `UPDATE users SET auth_version = auth_version + 1 WHERE id = $1`, user.ID)
	if _, err := module.CurrentUser(context.Background(), caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Caller bypassed new user version: %v", err)
	}
	if _, err := module.ResolveSession(context.Background(), login.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Cookie bypassed new user version: %v", err)
	}
	loginFixture(t, module)
}

func TestAbsoluteAndIdleExpiryUseServerDatabaseFacts(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	first := loginFixture(t, module)
	fixtureSQL(t, db, `UPDATE auth_sessions SET expires_at = created_at + interval '1 microsecond' WHERE user_id = $1`, user.ID)
	if _, err := module.ResolveSession(context.Background(), first.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("absolute expired session resolved: %v", err)
	}
	second := loginFixture(t, module)
	fixtureSQL(t, db, `UPDATE auth_sessions SET created_at = statement_timestamp() - interval '1 hour',
		last_seen_at = statement_timestamp() - interval '1 hour' WHERE user_id = $1`, user.ID)
	if _, err := module.ResolveSession(context.Background(), second.Token.CookieValue()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("idle expired session resolved: %v", err)
	}
}

func TestUntrustedHashParametersCannotIncreaseVerificationCost(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	fixtureSQL(t, db, `UPDATE local_credentials SET password_hash =
		'$argon2id$v=19$m=2147483647,t=999999,p=255$fixture-salt$fixture-key' WHERE user_id = $1`, user.ID)
	if _, err := module.LoginLocal(context.Background(), identity.LocalLoginCommand{
		LoginName: "fixture-admin", Password: fixturePassword, SourceIP: "127.0.0.1",
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("unsupported profile did not fail safely: %v", err)
	}
}
