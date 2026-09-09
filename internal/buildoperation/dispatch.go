package buildoperation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DispatchRef 是队列中允许出现的完整业务引用，不包含源码、Registry 或凭据。
type DispatchRef struct {
	DispatchID       uuid.UUID `db:"id" json:"build_dispatch_id"`
	BuildOperationID uuid.UUID `db:"build_operation_id" json:"build_operation_id"`
	Sequence         int64     `db:"sequence" json:"sequence"`
	ProtocolVersion  int       `db:"protocol_version" json:"protocol_version"`
}

// Dispatch 是投递器取得的短期运输权限，不是 Build Worker 的业务 Lease。
type Dispatch struct {
	DispatchRef
	AvailableAt      time.Time `db:"available_at"`
	PublishToken     uuid.UUID `db:"publish_token"`
	ReservationCount int       `db:"reservation_count"`
}

type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
}

type Lease struct {
	BuildOperationID       uuid.UUID
	BuildID                uuid.UUID
	BuildAttemptID         uuid.UUID
	BuildAttemptNumber     int
	RecoveredFromAttemptID *uuid.UUID
	PreviousExecutorName   *string
	PreviousExecutorUID    *string
	WorkerID               string
	ExpiresAt              time.Time
	TraceParent            string
	TraceState             string
	Recovery               bool
}

type DispatchState string

const (
	DispatchStatePending     DispatchState = "pending"
	DispatchStatePublished   DispatchState = "published"
	DispatchStateConsumed    DispatchState = "consumed"
	DispatchStateObsolete    DispatchState = "obsolete"
	DispatchStateQuarantined DispatchState = "quarantined"
)

type ClaimOutcome string

const (
	ClaimOutcomeClaimed  ClaimOutcome = "claimed"
	ClaimOutcomeIgnored  ClaimOutcome = "ignored"
	ClaimOutcomeDeferred ClaimOutcome = "deferred"
	ClaimOutcomeResolved ClaimOutcome = "resolved"
)

type DispatchClaim struct {
	Outcome ClaimOutcome
	Lease   Lease
}

type claimCandidate struct {
	ID                      uuid.UUID  `db:"id"`
	BuildID                 uuid.UUID  `db:"build_id"`
	Status                  Status     `db:"status"`
	AttemptCount            int        `db:"attempt_count"`
	RecoveryRequired        bool       `db:"recovery_required"`
	TraceParent             string     `db:"traceparent"`
	TraceState              string     `db:"tracestate"`
	CurrentDispatchSequence int64      `db:"current_dispatch_sequence"`
	AvailableAt             time.Time  `db:"available_at"`
	LeaseExpiresAt          *time.Time `db:"lease_expires_at"`
}

// ReserveDispatches 只取得 Outbox 运输权限，不创建 BuildAttempt。
func (m *Module) ReserveDispatches(ctx context.Context, limit int, leaseDuration time.Duration) ([]Dispatch, error) {
	if limit < 1 || limit > 1000 || leaseDuration <= 0 {
		return nil, errors.New("invalid build dispatch reservation")
	}
	now := m.now()
	items := []Dispatch{}
	if _, err := m.db.ExecContext(ctx, `WITH invalid AS (
	 SELECT d.id FROM build_dispatches d JOIN build_operations o ON o.id = d.build_operation_id
	 WHERE d.state IN ('pending','published') AND d.sequence = o.current_dispatch_sequence
	   AND d.protocol_version <> 1
	 ORDER BY d.id FOR UPDATE OF d SKIP LOCKED LIMIT $2
	) UPDATE build_dispatches d SET state = 'quarantined', publish_token = NULL,
	 publish_expires_at = NULL, last_error_code = 'unsupported_dispatch_version', updated_at = $1
	 FROM invalid WHERE d.id = invalid.id`, now, limit); err != nil {
		return nil, fmt.Errorf("quarantine build dispatches: %w", err)
	}
	if err := m.db.SelectContext(ctx, &items, `WITH due AS (
	 SELECT d.id FROM build_dispatches d JOIN build_operations o ON o.id = d.build_operation_id
	 WHERE d.state IN ('pending','published') AND d.protocol_version = 1
	   AND d.next_dispatch_at <= $1 AND (d.publish_expires_at IS NULL OR d.publish_expires_at <= $1)
	   AND d.sequence = o.current_dispatch_sequence AND d.expected_attempt_count = o.attempt_count
	   AND (o.status = 'pending' OR (o.status IN ('running','cancel_requested') AND o.lease_expires_at <= $1))
	 ORDER BY d.next_dispatch_at, d.id FOR UPDATE OF d SKIP LOCKED LIMIT $2
	) UPDATE build_dispatches d SET publish_token = $3, publish_expires_at = $4,
	 reservation_count = reservation_count + 1, updated_at = $1 FROM due WHERE d.id = due.id
	 RETURNING d.id, d.build_operation_id, d.sequence, d.protocol_version, d.available_at,
	 d.publish_token, d.reservation_count`, now, limit, uuid.New(), now.Add(leaseDuration)); err != nil {
		return nil, fmt.Errorf("reserve build dispatches: %w", err)
	}
	return items, nil
}

// ConfirmDispatch 只确认本轮 Publish Token；迟到确认不能覆盖后续运输所有者。
func (m *Module) ConfirmDispatch(ctx context.Context, dispatch Dispatch, errorCode string, grace time.Duration) error {
	if dispatch.PublishToken == uuid.Nil || grace <= 0 {
		return errors.New("invalid build dispatch confirmation")
	}
	now := m.now()
	if errorCode == "" {
		_, err := m.db.ExecContext(ctx, `UPDATE build_dispatches SET state = 'published',
		 published_at = COALESCE(published_at, $3), next_dispatch_at = $4,
		 publish_token = NULL, publish_expires_at = NULL, last_error_code = NULL, updated_at = $3
		 WHERE id = $1 AND publish_token = $2 AND state IN ('pending','published')`,
			dispatch.DispatchID, dispatch.PublishToken, now, now.Add(grace))
		return err
	}
	_, err := m.db.ExecContext(ctx, `UPDATE build_dispatches SET state = 'pending',
	 next_dispatch_at = $3, publish_token = NULL, publish_expires_at = NULL,
	 last_error_code = $4, updated_at = $2
	 WHERE id = $1 AND publish_token = $5 AND state IN ('pending','published')`,
		dispatch.DispatchID, now, now.Add(grace), errorCode, dispatch.PublishToken)
	return err
}

// ClaimDispatch 原子消费有效调度意图、取得业务 Lease 并创建唯一 BuildAttempt。
func (m *Module) ClaimDispatch(ctx context.Context, ref DispatchRef, request ClaimRequest) (DispatchClaim, error) {
	ignored := DispatchClaim{Outcome: ClaimOutcomeIgnored}
	if ref.DispatchID == uuid.Nil || ref.BuildOperationID == uuid.Nil || ref.Sequence <= 0 || ref.ProtocolVersion != 1 {
		return ignored, errors.New("invalid build dispatch reference")
	}
	if request.WorkerID == "" || request.LeaseDuration <= 0 {
		return ignored, errors.New("invalid build worker lease request")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return ignored, err
	}
	defer func() { _ = tx.Rollback() }()
	var candidate claimCandidate
	err = tx.GetContext(ctx, &candidate, `SELECT id, build_id, status, attempt_count,
	 recovery_required, traceparent, tracestate, current_dispatch_sequence, available_at,
	 lease_expires_at FROM build_operations WHERE id = $1 FOR UPDATE`, ref.BuildOperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ignored, nil
	}
	if err != nil {
		return ignored, err
	}
	if candidate.CurrentDispatchSequence != ref.Sequence {
		return ignored, nil
	}
	var intent struct {
		State           DispatchState `db:"state"`
		Expected        int           `db:"expected_attempt_count"`
		ProtocolVersion int           `db:"protocol_version"`
	}
	err = tx.GetContext(ctx, &intent, `SELECT state, expected_attempt_count, protocol_version
	 FROM build_dispatches WHERE id = $1 AND build_operation_id = $2 AND sequence = $3 FOR UPDATE`,
		ref.DispatchID, ref.BuildOperationID, ref.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return ignored, nil
	}
	if err != nil {
		return ignored, err
	}
	if intent.State != DispatchStatePending && intent.State != DispatchStatePublished {
		return ignored, nil
	}
	now := m.now()
	if intent.ProtocolVersion != 1 || intent.Expected != candidate.AttemptCount {
		if _, err := tx.ExecContext(ctx, `UPDATE build_dispatches SET state = 'obsolete',
		 publish_token = NULL, publish_expires_at = NULL, updated_at = $2 WHERE id = $1`, ref.DispatchID, now); err != nil {
			return ignored, err
		}
		return ignored, tx.Commit()
	}
	recovery := candidate.RecoveryRequired
	var recoveredFrom *uuid.UUID
	var previousExecutorName, previousExecutorUID *string
	switch candidate.Status {
	case StatusPending:
		if recovery {
			var previous struct {
				ID           uuid.UUID `db:"id"`
				ExecutorName *string   `db:"executor_name"`
				ExecutorUID  *string   `db:"executor_uid"`
			}
			if err := tx.GetContext(ctx, &previous, `SELECT id, executor_name, executor_uid FROM build_attempts
			 WHERE build_operation_id = $1 AND status = 'outcome_unknown'
			 ORDER BY attempt_number DESC LIMIT 1`, candidate.ID); err == nil {
				recoveredFrom = &previous.ID
				previousExecutorName, previousExecutorUID = previous.ExecutorName, previous.ExecutorUID
			} else if !errors.Is(err, sql.ErrNoRows) {
				return ignored, err
			}
		}
	case StatusRunning, StatusCancelRequested:
		if candidate.LeaseExpiresAt == nil || candidate.LeaseExpiresAt.After(now) {
			return ignored, nil
		}
		// 接管发生时才把旧的 running Attempt 记为结果未知，避免仅凭一次扫描改写执行历史。
		var previous struct {
			ID           uuid.UUID `db:"id"`
			ExecutorName *string   `db:"executor_name"`
			ExecutorUID  *string   `db:"executor_uid"`
		}
		if err := tx.GetContext(ctx, &previous, `UPDATE build_attempts SET status = 'outcome_unknown',
		 error_code = 'worker_lease_expired', error_summary = 'build worker lease expired before the executor result was committed',
		 retry_disposition = 'unknown_outcome', finished_at = $2
		 WHERE build_operation_id = $1 AND attempt_number = $3 AND status = 'running'
		 RETURNING id, executor_name, executor_uid`,
			candidate.ID, now, candidate.AttemptCount); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ignored, nil
			}
			return ignored, err
		}
		recoveredFrom = &previous.ID
		previousExecutorName, previousExecutorUID = previous.ExecutorName, previous.ExecutorUID
		recovery = true
	default:
		return ignored, nil
	}
	if candidate.AvailableAt.After(now) {
		if _, err := tx.ExecContext(ctx, `UPDATE build_dispatches SET state = 'pending',
		 next_dispatch_at = $2, publish_token = NULL, publish_expires_at = NULL, updated_at = $3
		 WHERE id = $1`, ref.DispatchID, candidate.AvailableAt, now); err != nil {
			return ignored, err
		}
		return DispatchClaim{Outcome: ClaimOutcomeDeferred}, tx.Commit()
	}
	attemptID := uuid.New()
	attemptNumber := candidate.AttemptCount + 1
	expiresAt := now.Add(request.LeaseDuration)
	if _, err := tx.ExecContext(ctx, `INSERT INTO build_attempts
	 (id, build_operation_id, attempt_number, worker_id, status, recovered_from_attempt_id, started_at)
	 VALUES ($1, $2, $3, $4, 'running', $5, $6)`, attemptID, candidate.ID, attemptNumber,
		request.WorkerID, recoveredFrom, now); err != nil {
		return ignored, err
	}
	nextStatus := StatusRunning
	if candidate.Status == StatusCancelRequested {
		nextStatus = StatusCancelRequested
	}
	if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = $2,
	 attempt_count = $3, lease_owner = $4, lease_expires_at = $5,
	 recovery_required = false, error_code = NULL, error_summary = NULL,
	 retry_disposition = NULL, started_at = COALESCE(started_at, $6), updated_at = $6
	 WHERE id = $1`, candidate.ID, nextStatus, attemptNumber, request.WorkerID, expiresAt, now); err != nil {
		return ignored, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE build_dispatches SET state = 'consumed',
	 consumed_at = $2, build_attempt_id = $3, publish_token = NULL, publish_expires_at = NULL,
	 updated_at = $2 WHERE id = $1`, ref.DispatchID, now, attemptID); err != nil {
		return ignored, err
	}
	if err := tx.Commit(); err != nil {
		return ignored, err
	}
	return DispatchClaim{Outcome: ClaimOutcomeClaimed, Lease: Lease{
		BuildOperationID: candidate.ID, BuildID: candidate.BuildID,
		BuildAttemptID: attemptID, BuildAttemptNumber: attemptNumber,
		RecoveredFromAttemptID: recoveredFrom, PreviousExecutorName: previousExecutorName,
		PreviousExecutorUID: previousExecutorUID,
		WorkerID:            request.WorkerID, ExpiresAt: expiresAt,
		TraceParent: candidate.TraceParent, TraceState: candidate.TraceState,
		Recovery: recovery,
	}}, nil
}

// RepairDispatches 为过期业务 Lease 登记新的恢复意图；重复扫描不会增加调度序列。
func (m *Module) RepairDispatches(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("invalid build dispatch repair limit")
	}
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := m.now()
	ids := []uuid.UUID{}
	if err := tx.SelectContext(ctx, &ids, `SELECT o.id FROM build_operations o
	 WHERE o.status IN ('running','cancel_requested') AND o.lease_expires_at <= $1
	 AND NOT EXISTS (SELECT 1 FROM build_dispatches d WHERE d.build_operation_id = o.id
	   AND d.sequence = o.current_dispatch_sequence AND d.expected_attempt_count = o.attempt_count
	   AND d.state IN ('pending','published','quarantined'))
	 ORDER BY o.lease_expires_at, o.id FOR UPDATE OF o SKIP LOCKED LIMIT $2`, now, limit); err != nil {
		return 0, fmt.Errorf("find build dispatch repairs: %w", err)
	}
	for _, id := range ids {
		if err := scheduleDispatch(ctx, tx, id, "lease_recovery", now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}
