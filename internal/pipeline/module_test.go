package pipeline

import "testing"

func TestNormalizePathRejectsRepositoryEscape(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"../Dockerfile", "/Dockerfile", `dir\Dockerfile`} {
		if _, err := normalizePath(value, "Dockerfile", false); err == nil {
			t.Fatalf("normalizePath(%q) succeeded, want rejection", value)
		}
	}
}

func TestValidBranchRejectsAmbiguousGitRefs(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "feature//one", "feature/../main", "release.lock", "topic@{one}", "topic one"} {
		if validBranch(value) {
			t.Fatalf("validBranch(%q) = true, want false", value)
		}
	}
	if !validBranch("release/2026-09") {
		t.Fatal("ordinary branch was rejected")
	}
}

func TestProjectRunKeepsOperationFailureAtItsOwningStage(t *testing.T) {
	t.Parallel()
	failed := "failed"
	tests := []struct {
		name   string
		row    runRow
		status string
		stage  string
	}{
		{name: "build failure", row: runRow{RunRecord: RunRecord{Phase: "build_created"}, BuildOperationStatus: "failed"}, status: "build_failed"},
		{name: "source verification", row: runRow{RunRecord: RunRecord{Phase: "artifact_ready"}}, status: "candidate_ready", stage: "source_verification"},
		{name: "build only candidate", row: runRow{RunRecord: RunRecord{Phase: "artifact_ready"}, PipelineMode: ModeBuildOnly}, status: "candidate_ready"},
		{name: "release failure", row: runRow{RunRecord: RunRecord{Phase: "release_created"}, ReleaseOperationStatus: &failed}, status: "release_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projected := projectRun(test.row)
			if projected.Status != test.status {
				t.Fatalf("status = %q, want %q", projected.Status, test.status)
			}
			if test.stage == "" && projected.ActiveStage != nil {
				t.Fatalf("active stage = %q, want nil", *projected.ActiveStage)
			}
			if test.stage != "" && (projected.ActiveStage == nil || *projected.ActiveStage != test.stage) {
				t.Fatalf("active stage = %v, want %q", projected.ActiveStage, test.stage)
			}
		})
	}
}
