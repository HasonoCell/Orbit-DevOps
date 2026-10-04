package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
)

func TestBoundedDatabasePoolWaitsAndCancelsWithoutGrowing(t *testing.T) {
	environment := newTestEnvironment(t)
	config := database.DefaultPool(1)
	config.MaxIdleConns = 1
	db, err := database.Open(context.Background(), environment.databaseURL, config)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := db.PingContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exhausted pool error = %v", err)
	}
	if stats := db.Stats(); stats.OpenConnections != 1 || stats.WaitCount != 1 || stats.WaitDuration <= 0 {
		t.Fatalf("pool did not honor its budget: %+v", stats)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("pool did not recover after release: %v", err)
	}
}
