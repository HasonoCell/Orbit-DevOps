package projectauth

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemberCursorIsBoundToProject(t *testing.T) {
	t.Parallel()

	projectID := uuid.New()
	cursor := memberCursor{
		Scope:     "project-members",
		ProjectID: projectID,
		CreatedAt: time.Now().UTC(),
		UserID:    uuid.New(),
	}
	encoded := encodeMemberCursor(cursor)

	decoded, err := decodeMemberCursor(encoded, projectID)
	if err != nil {
		t.Fatalf("decode matching cursor: %v", err)
	}
	if decoded != cursor {
		t.Fatalf("decoded cursor = %#v, want %#v", decoded, cursor)
	}
	if _, err := decodeMemberCursor(encoded, uuid.New()); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("decode cursor for another project error = %v, want ErrInvalidCursor", err)
	}
}

func TestMemberCursorRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	projectID := uuid.New()
	for _, value := range []string{
		"not-base64!",
		encodeMemberCursor(memberCursor{Scope: "wrong", ProjectID: projectID, CreatedAt: time.Now().UTC(), UserID: uuid.New()}),
		encodeMemberCursor(memberCursor{Scope: "project-members", ProjectID: projectID, UserID: uuid.New()}),
	} {
		if _, err := decodeMemberCursor(value, projectID); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("decode %q error = %v, want ErrInvalidCursor", value, err)
		}
	}
}
