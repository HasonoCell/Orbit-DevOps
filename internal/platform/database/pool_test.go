package database_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
)

func TestOpenDatabasePreservesCancellationWithoutLeakingDSN(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := database.Open(ctx, "postgres://test:private-password@127.0.0.1:1/test", database.DefaultPool(4))
	if db != nil || !errors.Is(err, database.ErrUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup = %v / %v", db, err)
	}
	if strings.Contains(err.Error(), "private-password") {
		t.Fatal("connection material escaped through startup error")
	}
}
