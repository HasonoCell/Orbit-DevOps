package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	migratedb "github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestFailureCategoryDoesNotExposeConnectionMaterial(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("private DSN: %w", &pgconn.PgError{Code: "28P01", Message: "private-password"}), "sqlstate=28P01"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "network_unavailable"},
		{&migratedb.Error{OrigErr: &pgconn.PgError{Code: "23505"}, Err: "private SQL"}, "sqlstate=23505"},
		{errors.New("private-password"), "migration_failed"},
	} {
		got := failureCategory(test.err)
		if got != test.want || strings.Contains(got, "private") {
			t.Fatalf("category = %q, want %q", got, test.want)
		}
	}
}
