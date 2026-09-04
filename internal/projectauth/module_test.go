package projectauth

import "testing"

func TestRoleAllowsOnlyDeclaredPermissions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		role       string
		permission Permission
		allowed    bool
	}{
		{name: "owner manages members", role: RoleOwner, permission: PermissionManageMembers, allowed: true},
		{name: "owner resolves unknown", role: RoleOwner, permission: PermissionResolveUnknown, allowed: true},
		{name: "developer reads", role: RoleDeveloper, permission: PermissionRead, allowed: true},
		{name: "developer changes delivery", role: RoleDeveloper, permission: PermissionDevelop, allowed: true},
		{name: "developer cannot manage members", role: RoleDeveloper, permission: PermissionManageMembers},
		{name: "developer cannot resolve unknown", role: RoleDeveloper, permission: PermissionResolveUnknown},
		{name: "viewer reads", role: RoleViewer, permission: PermissionRead, allowed: true},
		{name: "viewer cannot change delivery", role: RoleViewer, permission: PermissionDevelop},
		{name: "unknown role is denied", role: "unexpected", permission: PermissionRead},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if actual := roleAllows(testCase.role, testCase.permission); actual != testCase.allowed {
				t.Fatalf("roleAllows(%q, %q) = %t, want %t", testCase.role, testCase.permission, actual, testCase.allowed)
			}
		})
	}
}

func TestValidateActorIDRejectsAmbiguousValues(t *testing.T) {
	t.Parallel()

	for _, actorID := range []string{"", " leading", "trailing ", "line\nbreak"} {
		if err := validateActorID(actorID); err == nil {
			t.Errorf("validateActorID(%q) accepted invalid value", actorID)
		}
	}
	if err := validateActorID("local-developer"); err != nil {
		t.Fatalf("validateActorID accepted valid value: %v", err)
	}
}
