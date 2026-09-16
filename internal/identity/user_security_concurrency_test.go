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

func holdSecurityControl(t *testing.T, ctx context.Context, db *sqlx.DB) *sqlx.Tx {
	t.Helper()
	tx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("begin security transaction barrier")
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var marker sql.NullTime
	if err := tx.GetContext(ctx, &marker, `SELECT initialized_at FROM identity_control WHERE id = 1 FOR UPDATE`); err != nil {
		t.Fatal("hold security transaction barrier")
	}
	return tx
}

func TestConcurrentAdministratorDemotionsHaveExactlyOneWinner(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	firstUser := initializeFixture(t, module)
	first := resolveFixture(t, module, loginFixture(t, module))
	secondUser, _, _ := completeLocalFixture(t, module, first, "fixture-concurrent-admin")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
		Caller: first, UserID: secondUser.ID, Role: identity.RolePlatformAdmin,
	}); err != nil {
		t.Fatalf("grant second concurrency administrator: %v", err)
	}
	second := resolveFixture(t, module, completedLoginFixture(t, module, "fixture-concurrent-admin"))
	barrier := holdSecurityControl(t, ctx, db)
	firstResult := make(chan error, 1)
	go func() {
		_, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
			Caller: first, UserID: firstUser.ID, Role: identity.RoleUser,
		})
		firstResult <- err
	}()
	firstPID := awaitControlledCommandWait(t, ctx, db, 0)
	secondResult := make(chan error, 1)
	go func() {
		_, err := module.ChangePlatformRole(ctx, identity.ChangePlatformRoleCommand{
			Caller: second, UserID: secondUser.ID, Role: identity.RoleUser,
		})
		secondResult <- err
	}()
	awaitControlledCommandWait(t, ctx, db, firstPID)
	if err := barrier.Commit(); err != nil {
		t.Fatal("release administrator demotion barrier")
	}
	if err := <-firstResult; err != nil {
		t.Fatalf("first serialized demotion: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, identity.ErrLastAdministrator) {
		t.Fatalf("second demotion reused a pre-wait administrator count: %v", err)
	}
	if _, err := module.CurrentUser(ctx, first); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("successful demotion retained old proof: %v", err)
	}
	if _, err := module.CurrentUser(ctx, second); err != nil {
		t.Fatalf("rejected demotion partially revoked last administrator: %v", err)
	}
}

func TestConcurrentOwnerDisablesCannotRemoveBothEffectiveOwners(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	first, firstCaller, _ := completeLocalFixture(t, module, admin, "fixture-concurrent-first-owner")
	second, secondCaller, _ := completeLocalFixture(t, module, admin, "fixture-concurrent-second-owner")
	project := ownerFixtureProject(t, db, first.ID.String())
	ownerFixtureMember(t, db, project, second.ID.String(), "owner")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	barrier := holdSecurityControl(t, ctx, db)
	firstResult := make(chan error, 1)
	go func() {
		_, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: first.ID})
		firstResult <- err
	}()
	firstPID := awaitControlledCommandWait(t, ctx, db, 0)
	secondResult := make(chan error, 1)
	go func() {
		_, err := module.DisableUser(ctx, identity.DisableUserCommand{Caller: admin, UserID: second.ID})
		secondResult <- err
	}()
	awaitControlledCommandWait(t, ctx, db, firstPID)
	if err := barrier.Commit(); err != nil {
		t.Fatal("release owner disable barrier")
	}
	if err := <-firstResult; err != nil {
		t.Fatalf("first serialized owner disable: %v", err)
	}
	if err := <-secondResult; !errors.Is(err, identity.ErrLastProjectOwner) {
		t.Fatalf("second disable reused a pre-wait effective owner count: %v", err)
	}
	if _, err := module.CurrentUser(ctx, firstCaller); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("successful owner disable retained old proof: %v", err)
	}
	if _, err := module.CurrentUser(ctx, secondCaller); err != nil {
		t.Fatalf("rejected owner disable partially revoked last owner: %v", err)
	}
}

func TestCanceledUserDisableLeavesNoStatusAuditOrRevocation(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	target, caller, _ := completeLocalFixture(t, module, admin, "fixture-cancel-disable")
	barrierCtx, finish := context.WithTimeout(context.Background(), 15*time.Second)
	defer finish()
	barrier := holdSecurityControl(t, barrierCtx, db)
	requestCtx, cancel := context.WithCancel(barrierCtx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := module.DisableUser(requestCtx, identity.DisableUserCommand{Caller: admin, UserID: target.ID})
		result <- err
	}()
	awaitControlledCommandWait(t, barrierCtx, db, 0)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lock wait accepted disable or lost cancellation: %v", err)
	}
	if err := barrier.Commit(); err != nil {
		t.Fatal("release canceled disable barrier")
	}
	current, err := module.CurrentUser(barrierCtx, caller)
	if err != nil || current.User.Status != "active" {
		t.Fatalf("canceled disable changed status/version/revocation: %v", err)
	}
	var audits int
	if err := db.GetContext(barrierCtx, &audits, `SELECT count(*) FROM audit_records
		WHERE target_id = $1 AND action = 'user.disable'`, target.ID); err != nil || audits != 0 {
		t.Fatal("canceled disable created a success audit")
	}
}
