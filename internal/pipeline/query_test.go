package pipeline

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPipelineCursorIsBoundToApplication(t *testing.T) {
	t.Parallel()

	applicationID := uuid.New()
	cursor := pipelineCursor{
		Scope:         "delivery-pipelines",
		ApplicationID: applicationID,
		CreatedAt:     time.Now().UTC(),
		ID:            uuid.New(),
	}
	encoded := encodePipelineCursor(cursor)

	decoded, err := decodePipelineCursor(encoded, applicationID)
	if err != nil {
		t.Fatalf("decode matching cursor: %v", err)
	}
	if decoded != cursor {
		t.Fatalf("decoded cursor = %#v, want %#v", decoded, cursor)
	}
	if _, err := decodePipelineCursor(encoded, uuid.New()); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("decode cursor for another application error = %v, want ErrInvalidCursor", err)
	}
}

func TestPipelineCursorRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	applicationID := uuid.New()
	for _, value := range []string{
		"not-base64!",
		encodePipelineCursor(pipelineCursor{Scope: "wrong", ApplicationID: applicationID, CreatedAt: time.Now().UTC(), ID: uuid.New()}),
		encodePipelineCursor(pipelineCursor{Scope: "delivery-pipelines", ApplicationID: applicationID, ID: uuid.New()}),
	} {
		if _, err := decodePipelineCursor(value, applicationID); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("decode %q error = %v, want ErrInvalidCursor", value, err)
		}
	}
}
