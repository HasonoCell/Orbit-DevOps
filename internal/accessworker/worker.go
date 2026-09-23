// Package accessworker 将入口 outbox 唤醒变为按 Project 串行的 Kubernetes 集合调和。
package accessworker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/access"
	"github.com/HasonoCell/Orbit-DevOps/internal/internalevent"
	"github.com/HasonoCell/Orbit-DevOps/internal/kube"
	"github.com/google/uuid"
)

const Topic = "project_gateway.reconcile.v1"

type Reconciler interface {
	Reconcile(context.Context, access.Snapshot, func(context.Context) error) error
}

type Worker struct {
	module        *access.Module
	reconciler    Reconciler
	leaseDuration time.Duration
	logger        *slog.Logger
}

func New(module *access.Module, reconciler Reconciler, leaseDuration time.Duration, logger *slog.Logger) (*Worker, error) {
	if module == nil || reconciler == nil || leaseDuration <= 0 || logger == nil {
		return nil, errors.New("invalid access worker configuration")
	}
	return &Worker{module: module, reconciler: reconciler, leaseDuration: leaseDuration, logger: logger}, nil
}

// HandleEvent 不按队列中可能过期的修订执行，而是始终重读 PostgreSQL 当前完整期望。
func (w *Worker) HandleEvent(ctx context.Context, ref internalevent.Ref) error {
	if ref.Topic != Topic || ref.AggregateID == uuid.Nil || ref.ProtocolVersion != 1 {
		return errors.New("invalid access event")
	}
	return w.reconcile(ctx, ref.AggregateID)
}

func (w *Worker) reconcile(ctx context.Context, projectID uuid.UUID) error {
	lease, acquired, err := w.module.AcquireReconcileLease(ctx, projectID, w.leaseDuration)
	if err != nil || !acquired {
		return err
	}
	snapshot, err := w.module.LoadSnapshot(ctx, projectID)
	code := "access_store_unavailable"
	if err == nil {
		err = w.module.CheckLease(ctx, lease, snapshot.Revision)
	}
	if err == nil {
		err = w.reconciler.Reconcile(ctx, snapshot, func(check context.Context) error {
			return w.module.CheckLease(check, lease, snapshot.Revision)
		})
		if err != nil {
			code = kube.AccessErrorCode(err)
		}
	}
	if err == nil {
		err = w.module.CompleteReconcile(ctx, lease, snapshot.Revision)
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, access.ErrLeaseLost) {
		return nil
	}
	attention := errors.Is(err, kube.ErrAccessBoundary) || errors.Is(err, kube.ErrAccessOwnership)
	if failure := w.module.FailReconcile(ctx, lease, code, attention); failure != nil {
		return errors.Join(err, failure)
	}
	return err
}

// Maintain 覆盖丢失唤醒、租约过期、部分 Apply 以及已应用资源的周期漂移检查。
func (w *Worker) Maintain(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("invalid access maintenance interval")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, w.leaseDuration)
		projects, err := w.module.DueProjects(cycle, 50)
		if err == nil {
			for _, projectID := range projects {
				if cycle.Err() != nil {
					break
				}
				if failure := w.reconcile(cycle, projectID); failure != nil {
					w.logger.WarnContext(cycle, "入口维护暂时不可用", "project_id", projectID)
				}
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			w.logger.WarnContext(ctx, "入口维护扫描暂时不可用")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
