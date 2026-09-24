package pgerrors_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/pgerrors"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueConstraintMatchesOnlyTheExpectedConstraint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "wrapped matching constraint", err: fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: "projects_slug_key"}), want: true},
		{name: "other unique constraint", err: &pgconn.PgError{Code: "23505", ConstraintName: "other_key"}},
		{name: "other database error", err: &pgconn.PgError{Code: "23503", ConstraintName: "projects_slug_key"}},
		{name: "non database error", err: errors.New("unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pgerrors.IsUniqueConstraint(test.err, "projects_slug_key"); got != test.want {
				t.Fatalf("IsUniqueConstraint() = %t, want %t", got, test.want)
			}
		})
	}
}
