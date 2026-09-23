package accessworker

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
)

type noOpReconciler struct{}

func (noOpReconciler) Reconcile(context.Context, access.Snapshot, func(context.Context) error) error {
	return nil
}

func TestWorkerRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := New(nil, noOpReconciler{}, time.Second, slog.Default()); err == nil {
		t.Fatal("nil module must fail")
	}
	if _, err := New(new(access.Module), nil, time.Second, slog.Default()); err == nil {
		t.Fatal("nil reconciler must fail")
	}
	if _, err := New(new(access.Module), noOpReconciler{}, 0, slog.Default()); err == nil {
		t.Fatal("missing lease must fail")
	}
}
