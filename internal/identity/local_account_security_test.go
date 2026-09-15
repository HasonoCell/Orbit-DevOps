package identity_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/jmoiron/sqlx"
)

func TestLocalAccountCommandsDoNotExposePasswordsThroughFormattingOrJSON(t *testing.T) {
	t.Parallel()
	commands := []any{
		identity.CreateLocalUserCommand{TemporaryPassword: fixturePassword},
		identity.ChangePasswordCommand{CurrentPassword: fixturePassword, NewPassword: fixturePassword},
		identity.ResetLocalPasswordCommand{TemporaryPassword: fixturePassword},
		identity.ReauthenticateLocalCommand{Password: fixturePassword},
	}
	for _, command := range commands {
		encoded, err := json.Marshal(command)
		if err != nil {
			t.Fatal("marshal identity command")
		}
		if strings.Contains(string(encoded), fixturePassword) ||
			strings.Contains(fmt.Sprintf("%+v %#v", command, command), fixturePassword) {
			t.Fatalf("%T exposed credential through a generic output surface", command)
		}
	}
}

func TestLocalAccountAuditFailureRollsBackCredentialsAndRevocation(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx := context.Background()
	// 各成功命令审计故障都是真实 PG 事务故障，而非替换内部 audit 协作方。
	fixtureSQL(t, db, `CREATE FUNCTION block_local_account_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.action IN ('user.create', 'user.password.change', 'user.password.reset', 'auth.reauth') THEN
		 RAISE EXCEPTION 'fixture account audit failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER block_local_account_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_local_account_audit()`)
	create := identity.CreateLocalUserCommand{Caller: admin, LoginName: "fixture-developer",
		DisplayName: "测试开发者", TemporaryPassword: fixturePassword}
	if _, err := module.CreateLocalUser(ctx, create); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("user was admitted without success audit: %v", err)
	}
	fixtureSQL(t, db, `DROP TRIGGER block_local_account_audit ON audit_records`)
	user, err := module.CreateLocalUser(ctx, create)
	if err != nil {
		t.Fatalf("failed admission consumed user or unique credential: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("login admitted fixture: %v", err)
	}
	caller := resolveFixture(t, module, login)
	fixtureSQL(t, db, `CREATE TRIGGER block_local_account_audit BEFORE INSERT ON audit_records
		FOR EACH ROW EXECUTE FUNCTION block_local_account_audit()`)
	if err := module.ChangePassword(ctx, identity.ChangePasswordCommand{Caller: caller,
		CurrentPassword: fixturePassword, NewPassword: fixturePassword + "-changed", SourceIP: "127.0.0.1",
	}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("password change succeeded without audit: %v", err)
	}
	if err := module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{
		Caller: admin, UserID: user.ID, TemporaryPassword: fixturePassword + "-reset",
	}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("password reset succeeded without audit: %v", err)
	}
	if _, err := module.ReauthenticateLocal(ctx, identity.ReauthenticateLocalCommand{
		Caller: caller, Password: fixturePassword, SourceIP: "127.0.0.1",
	}); !errors.Is(err, identity.ErrUnavailable) {
		t.Fatalf("session rotation succeeded without audit: %v", err)
	}
	current, err := module.CurrentUser(ctx, caller)
	if err != nil || !current.MustChangePassword {
		t.Fatalf("failed transactions changed restriction/version or revoked old Caller: %v", err)
	}
	resolveFixture(t, module, login)
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1",
	}); err != nil {
		t.Fatalf("failed transactions changed password: %v", err)
	}
}

// awaitControlledCommandWait 找真实控制锁等待者作为屏障，只读取 PG 阻塞元数据。
// fixture 中没有其他活动业务连接，不查看认证 SQL 参数/密码或靠延时猜测入队先后。
func awaitControlledCommandWait(t *testing.T, ctx context.Context, db *sqlx.DB, excludedPID int) int {
	t.Helper()
	for {
		var pid int
		err := db.GetContext(ctx, &pid, `SELECT pid FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> $1 AND state = 'active'
			AND cardinality(pg_blocking_pids(pid)) > 0
			AND query = 'SELECT initialized_at FROM identity_control WHERE id = 1 FOR UPDATE'
			ORDER BY query_start, pid LIMIT 1`, excludedPID)
		if err == nil {
			return pid
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("identity command did not reach PostgreSQL control lock barrier")
		}
	}
}

func TestResetCommittingFirstRejectsEarlierVerifiedReauthenticationProof(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	user, err := module.CreateLocalUser(ctx, identity.CreateLocalUserCommand{Caller: admin,
		LoginName: "fixture-developer", DisplayName: "测试开发者", TemporaryPassword: fixturePassword,
	})
	if err != nil {
		t.Fatalf("create concurrency fixture user: %v", err)
	}
	login, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-developer", Password: fixturePassword, SourceIP: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("login concurrency fixture user: %v", err)
	}
	caller := resolveFixture(t, module, login)
	barrier, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin control lock fixture")
	}
	defer func() { _ = barrier.Rollback() }()
	var marker sql.NullTime
	if err := barrier.GetContext(ctx, &marker, `SELECT initialized_at FROM identity_control WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal("hold control lock fixture")
	}
	resetResult := make(chan error, 1)
	go func() {
		resetResult <- module.ResetLocalPassword(ctx, identity.ResetLocalPasswordCommand{
			Caller: admin, UserID: user.ID, TemporaryPassword: fixturePassword + "-reset",
		})
	}()
	resetPID := awaitControlledCommandWait(t, ctx, db, 0)
	reauthResult := make(chan error, 1)
	go func() {
		_, err := module.ReauthenticateLocal(ctx, identity.ReauthenticateLocalCommand{
			Caller: caller, Password: fixturePassword, SourceIP: "127.0.0.1",
		})
		reauthResult <- err
	}()
	awaitControlledCommandWait(t, ctx, db, resetPID)
	if err := barrier.Commit(); err != nil {
		t.Fatal("release control lock fixture")
	}
	if err := <-resetResult; err != nil {
		t.Fatalf("first queued reset did not commit: %v", err)
	}
	if err := <-reauthResult; !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("pre-reset password proof minted a post-reset session: %v", err)
	}
	if _, err := module.CurrentUser(ctx, caller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("old Caller survived committed reset: %v", err)
	}
}
