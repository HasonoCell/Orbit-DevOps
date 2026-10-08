package database

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// 新 Pod 的 NetworkPolicy 可能尚未放行首次连接；重试仍须服从启动预算。
func TestPingUntilReadyRetriesTransientConnectionRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	err := pingUntilReady(ctx, func(context.Context) error {
		calls++
		if calls == 1 {
			return &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("startup = %v, calls = %d", err, calls)
	}
}

func TestPingUntilReadyDoesNotRetryAuthenticationFailure(t *testing.T) {
	want := &pgconn.PgError{Code: "28P01", Message: "private connection material"}
	calls := 0
	err := pingUntilReady(context.Background(), func(context.Context) error { calls++; return want })
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("authentication = %v, calls = %d", err, calls)
	}
}

func TestPingUntilReadyHonorsDeadlineAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		if canceled {
			cancel()
		}
		calls := 0
		err := pingUntilReady(ctx, func(context.Context) error {
			calls++
			return &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		})
		cancel()
		want := context.DeadlineExceeded
		if canceled {
			want = context.Canceled
		}
		if !errors.Is(err, want) {
			t.Fatalf("budget = %v, want %v", err, want)
		}
		if canceled && calls != 0 {
			t.Fatalf("canceled startup dialed %d times", calls)
		}
	}
}
