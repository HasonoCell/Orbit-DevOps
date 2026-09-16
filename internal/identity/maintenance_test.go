package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
)

func TestMaintenanceFreezeAndExplicitRecoveryKeepMissingOwnershipVisible(t *testing.T) {
	t.Parallel()
	module, database := newIdentity(t)
	adminUser := initializeFixture(t, module)
	projectID := ownerFixtureProject(t, database, adminUser.ID.String())
	ctx := context.Background()
	if err := module.FreezeUser(ctx, identity.FreezeUserCommand{UserID: adminUser.ID,
		MaintenanceRef: "fixture-emergency-freeze"}); err != nil {
		t.Fatalf("freeze last owner administrator: %v", err)
	}
	var state string
	if err := database.GetContext(ctx, &state, `SELECT identity_state FROM projects WHERE id=$1`, projectID); err != nil || state != "frozen" {
		t.Fatalf("last-owner project was not explicitly frozen: %q, %v", state, err)
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{LoginName: "fixture-admin",
		Password: fixturePassword, SourceIP: "127.0.0.1"}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("frozen user retained login: %v", err)
	}
	recovered, err := module.RecoverAdmin(ctx, identity.RecoverAdminCommand{UserID: adminUser.ID,
		LoginName: "fixture-admin", Password: fixturePassword + "-recovered", MaintenanceRef: "fixture-admin-recovery"})
	if err != nil || recovered.PlatformRole != identity.RolePlatformAdmin || recovered.Status != "active" {
		t.Fatalf("recover administrator: %#v, %v", recovered, err)
	}
	if err := module.RecoverProjectOwner(ctx, identity.RecoverProjectOwnerCommand{ProjectID: projectID,
		UserID: adminUser.ID, MaintenanceRef: "fixture-owner-recovery"}); err != nil {
		t.Fatalf("recover project owner: %v", err)
	}
	if err := database.GetContext(ctx, &state, `SELECT identity_state FROM projects WHERE id=$1`, projectID); err != nil || state != "governed" {
		t.Fatalf("owner recovery did not restore project governance: %q, %v", state, err)
	}
}

func TestLegacyClaimRequiresCompleteProjectMappingAndIsReplaySafe(t *testing.T) {
	t.Parallel()
	module, database := newIdentity(t)
	initializeFixture(t, module)
	admin := resolveFixture(t, module, loginFixture(t, module))
	first, _, _ := completeLocalFixture(t, module, admin, "fixture-legacy-owner")
	second, _, _ := completeLocalFixture(t, module, admin, "fixture-legacy-viewer")
	projectID := uuid.New()
	fixtureSQL(t, database, `INSERT INTO projects(id,name,slug,created_by,created_at)
		VALUES($1,'Legacy fixture',$2,'legacy-owner',clock_timestamp())`, projectID, projectID.String())
	fixtureSQL(t, database, `INSERT INTO legacy_project_members(project_id,actor_id,role,created_by,created_at,updated_at)
		VALUES($1,'legacy-owner','owner','legacy-owner',clock_timestamp(),clock_timestamp()),
		($1,'legacy-viewer','viewer','legacy-owner',clock_timestamp(),clock_timestamp())`, projectID)
	ctx := context.Background()
	partial := identity.ClaimLegacyMembersCommand{Mappings: []identity.LegacyClaimMapping{{ActorID: "legacy-owner", UserID: first.ID}},
		MaintenanceRef: "fixture-partial-claim"}
	if _, err := module.ClaimLegacyMembers(ctx, partial); !errors.Is(err, identity.ErrLegacyClaimConflict) {
		t.Fatalf("partial project claim was accepted: %v", err)
	}
	command := identity.ClaimLegacyMembersCommand{Mappings: []identity.LegacyClaimMapping{
		{ActorID: "legacy-owner", UserID: first.ID}, {ActorID: "legacy-viewer", UserID: second.ID},
	}, MaintenanceRef: "fixture-complete-claim"}
	report, err := module.ClaimLegacyMembers(ctx, command)
	if err != nil || report.Projects != 1 || report.Members != 2 {
		t.Fatalf("legacy claim dry-run: %#v, %v", report, err)
	}
	var members int
	if err := database.GetContext(ctx, &members, `SELECT count(*) FROM project_members WHERE project_id=$1`, projectID); err != nil || members != 0 {
		t.Fatal("dry-run wrote current project members")
	}
	command.Execute = true
	if _, err := module.ClaimLegacyMembers(ctx, command); err != nil {
		t.Fatalf("execute complete legacy claim: %v", err)
	}
	var audits int
	if err := database.GetContext(ctx, &audits, `SELECT count(*) FROM audit_records
		WHERE action='identity.claim_legacy_members' AND target_id=$1`, projectID); err != nil || audits != 1 {
		t.Fatal("executed claim did not write one project audit")
	}
	if _, err := module.ClaimLegacyMembers(ctx, command); err != nil {
		t.Fatalf("same claim replay failed: %v", err)
	}
	if err := database.GetContext(ctx, &audits, `SELECT count(*) FROM audit_records
		WHERE action='identity.claim_legacy_members' AND target_id=$1`, projectID); err != nil || audits != 1 {
		t.Fatal("same claim replay duplicated audit")
	}
	command.Mappings[0].UserID = second.ID
	if _, err := module.ClaimLegacyMembers(ctx, command); !errors.Is(err, identity.ErrLegacyClaimConflict) {
		t.Fatalf("different legacy mapping replay was accepted: %v", err)
	}
}
