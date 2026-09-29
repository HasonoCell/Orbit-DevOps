package projectauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/project"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	"github.com/google/uuid"
)

func TestMemberCandidateLookupIsExactAuthorizedAndAmbiguitySafe(t *testing.T) {
	db, identities, owner := roleFixture(t)
	ctx := context.Background()
	auth := projectauth.New(db, identities)
	p, err := project.New(db, auth).Create(ctx, project.CreateCommand{Caller: owner,
		Name: "候选查找", Slug: "candidate-lookup", IdempotencyKey: "candidate-project"})
	if err != nil {
		t.Fatal(err)
	}
	first := roleUser(t, identities, owner, "candidate-first")
	second := roleUser(t, identities, owner, "candidate-second")
	viewer := roleUser(t, identities, owner, "candidate-viewer")
	if _, err := auth.AddMember(ctx, projectauth.AddMemberCommand{Caller: owner, ProjectID: p.ID,
		UserID: viewer.UserID(), Role: projectauth.RoleViewer, IdempotencyKey: "candidate-viewer"}); err != nil {
		t.Fatal(err)
	}
	for _, lookup := range []struct{ kind, value string }{
		{"login_name", "CANDIDATE-FIRST"}, {"user_id", first.UserID().String()},
	} {
		result, err := auth.ResolveMemberCandidate(ctx, p.ID, owner, lookup.kind, lookup.value)
		if err != nil || result.Status != "found" || result.Candidate == nil || result.Candidate.UserID != first.UserID() || result.Candidate.DisplayName != "测试成员" {
			t.Fatalf("lookup %+v: %+v, %v", lookup, result, err)
		}
	}
	if _, err := auth.ResolveMemberCandidate(ctx, p.ID, viewer, "login_name", "candidate-first"); !errors.Is(err, projectauth.ErrForbidden) {
		t.Fatalf("viewer searched directory: %v", err)
	}
	if result, err := auth.ResolveMemberCandidate(ctx, p.ID, owner, "login_name", "missing-user"); err != nil || result.Status != "not_found" || result.Candidate != nil {
		t.Fatalf("missing lookup: %+v, %v", result, err)
	}
	if _, err := auth.ResolveMemberCandidate(ctx, p.ID, owner, "user_id", "invalid"); !errors.Is(err, projectauth.ErrInvalidMember) {
		t.Fatalf("invalid UUID: %v", err)
	}
	// 两个有效 User 有同一个已验证邮箱时，不返回任意一人的信息。
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_providers
 (id,display_name,issuer,client_id,client_secret_ref,enabled,created_by,created_at,updated_at)
 VALUES ('candidate-oidc','测试 OIDC','https://example.com/oidc','candidate','fixture-ref',false,'fixture',now(),now())`); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []uuid.UUID{first.UserID(), second.UserID()} {
		if _, err := db.ExecContext(ctx, `INSERT INTO external_identities
 (id,provider_id,subject,status,user_id,display_name,email,email_verified,created_at,updated_at)
 VALUES ($1,'candidate-oidc',$2,'linked',$3,'测试成员','shared@example.com',true,now(),now())`, uuid.New(), userID.String(), userID); err != nil {
			t.Fatal(err)
		}
	}
	result, err := auth.ResolveMemberCandidate(ctx, p.ID, owner, "verified_email", "SHARED@example.com")
	if err != nil || result.Status != "ambiguous" || result.Candidate != nil {
		t.Fatalf("ambiguous email leaked candidate: %+v, %v", result, err)
	}
	page, err := auth.ListMembers(ctx, p.ID, owner, 20, "")
	if err != nil || len(page.Items) != 2 || page.Items[0].DisplayName == "" {
		t.Fatalf("member list display name: %+v, %v", page, err)
	}
}
