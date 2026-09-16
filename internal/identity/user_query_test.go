package identity

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestIdentityCursorIsBoundToScopeAndSubject(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	cursor := identityCursor{
		Scope:     "sessions",
		SubjectID: userID,
		CreatedAt: time.Now().UTC(),
		ID:        uuid.New(),
	}
	encoded := encodeIdentityCursor(cursor)

	decoded, err := decodeIdentityCursor(encoded, "sessions", userID)
	if err != nil {
		t.Fatalf("decode matching cursor: %v", err)
	}
	if decoded != cursor {
		t.Fatalf("decoded cursor = %#v, want %#v", decoded, cursor)
	}

	for name, testCase := range map[string]struct {
		scope     string
		subjectID uuid.UUID
	}{
		"different endpoint": {scope: "external-identities", subjectID: userID},
		"different user":     {scope: "sessions", subjectID: uuid.New()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeIdentityCursor(encoded, testCase.scope, testCase.subjectID); !errors.Is(err, ErrInvalidCursor) {
				t.Fatalf("decode cursor error = %v, want ErrInvalidCursor", err)
			}
		})
	}
}

func TestIdentityCursorRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"not-base64!",
		encodeIdentityCursor(identityCursor{Scope: "sessions", SubjectID: uuid.New(), ID: uuid.New()}),
		encodeIdentityCursor(identityCursor{Scope: "sessions", SubjectID: uuid.New(), CreatedAt: time.Now().UTC()}),
	} {
		if _, err := decodeIdentityCursor(value, "sessions", uuid.New()); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("decode %q error = %v, want ErrInvalidCursor", value, err)
		}
	}
}
