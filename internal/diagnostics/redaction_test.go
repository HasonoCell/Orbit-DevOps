package diagnostics

import "testing"

func TestSanitizeEvidenceTextBoundsAndRedactsCommonCredentials(t *testing.T) {
	t.Parallel()

	value := "Authorization: Bearer abc.def.ghi password=hunter2 " + string(make([]rune, 600))
	got := SanitizeEvidenceText(value, 512)
	if containsAny(got, "abc.def.ghi", "hunter2") {
		t.Fatalf("sanitized text still contains credential: %q", got)
	}
	if len([]rune(got)) > 512 {
		t.Fatalf("sanitized text length = %d, want <= 512", len([]rune(got)))
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if len(candidate) > 0 && len(value) >= len(candidate) {
			for start := 0; start+len(candidate) <= len(value); start++ {
				if value[start:start+len(candidate)] == candidate {
					return true
				}
			}
		}
	}
	return false
}
