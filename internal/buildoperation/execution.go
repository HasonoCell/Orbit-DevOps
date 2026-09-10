package buildoperation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

const (
	Retryable      = "retryable"
	NonRetryable   = "non_retryable"
	UnknownOutcome = "unknown_outcome"
)

var ErrLeaseLost = errors.New("build operation lease is no longer owned by worker")

type Renewal struct {
	Lease           Lease
	CancelRequested bool
}

type ExecutorIdentity struct {
	Name string
	UID  string
}

type Failure struct {
	Code         string
	Summary      string
	Disposition  string
	LogExcerpt   string
	LogTruncated bool
}

type FailureResult struct {
	Status         Status
	RetryScheduled bool
	AvailableAt    *time.Time
}

type ArtifactResult struct {
	Repository   string
	Digest       string
	LogExcerpt   string
	LogTruncated bool
}

type ImageArtifact struct {
	ID             uuid.UUID `db:"id"`
	BuildID        uuid.UUID `db:"build_id"`
	ProjectID      uuid.UUID `db:"project_id"`
	ApplicationID  uuid.UUID `db:"application_id"`
	Repository     string    `db:"repository"`
	Digest         string    `db:"digest"`
	ImageReference string    `db:"image_reference"`
	Platform       string    `db:"platform"`
	CreatedBy      string    `db:"created_by"`
	CreatedAt      time.Time `db:"created_at"`
}

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Renew 延长当前 BuildAttempt 的业务 Lease，并把取消请求反馈给 Runner。
func (m *Module) Renew(ctx context.Context, lease Lease, duration time.Duration) (Renewal, error) {
	if duration <= 0 {
		return Renewal{}, errors.New("build lease duration must be positive")
	}
	now := m.now()
	expiresAt := now.Add(duration)
	var status Status
	err := m.db.GetContext(ctx, &status, `UPDATE build_operations
	 SET lease_expires_at = $1, updated_at = $2
	 WHERE id = $3 AND status IN ('running','cancel_requested') AND lease_owner = $4
	   AND attempt_count = $5 AND lease_expires_at > $2
	 RETURNING status`, expiresAt, now, lease.BuildOperationID, lease.WorkerID, lease.BuildAttemptNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return Renewal{}, ErrLeaseLost
	}
	if err != nil {
		return Renewal{}, fmt.Errorf("renew build operation lease: %w", err)
	}
	lease.ExpiresAt = expiresAt
	lease.CancelRequested = status == StatusCancelRequested
	return Renewal{Lease: lease, CancelRequested: status == StatusCancelRequested}, nil
}

// RecordExecutorIdentity 在启动或接管外部 Job 后记录稳定名称和 UID；同一 Attempt 不允许改绑。
func (m *Module) RecordExecutorIdentity(ctx context.Context, lease Lease, identity ExecutorIdentity) error {
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.UID) == "" {
		return errors.New("build executor identity is incomplete")
	}
	now := m.now()
	result, err := m.db.ExecContext(ctx, `UPDATE build_attempts a SET executor_name = $1, executor_uid = $2
	 FROM build_operations o WHERE a.id = $3 AND a.build_operation_id = o.id AND a.status = 'running'
	   AND (a.executor_name IS NULL OR (a.executor_name = $1 AND a.executor_uid = $2))
	   AND o.id = $4 AND o.status IN ('running','cancel_requested') AND o.lease_owner = $5
	   AND o.attempt_count = $6 AND o.lease_expires_at > $7`, identity.Name, identity.UID,
		lease.BuildAttemptID, lease.BuildOperationID, lease.WorkerID, lease.BuildAttemptNumber, now)
	if err != nil {
		return fmt.Errorf("record build executor identity: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect build executor identity update: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

// Succeed 将当前 Attempt、BuildOperation 与 ImageArtifact 在同一事务中收束。
func (m *Module) Succeed(ctx context.Context, lease Lease, result ArtifactResult) (ImageArtifact, error) {
	if strings.TrimSpace(result.Repository) == "" || !digestPattern.MatchString(result.Digest) {
		return ImageArtifact{}, errors.New("invalid build artifact result")
	}
	if err := validateLog(result.LogExcerpt); err != nil {
		return ImageArtifact{}, err
	}
	now := m.now()
	tx, _, err := m.lockCompletion(ctx, lease, StatusRunning, now)
	if err != nil {
		return ImageArtifact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var artifact ImageArtifact
	if err := tx.GetContext(ctx, &artifact, `SELECT $1::uuid AS id, b.id AS build_id, b.project_id,
	 b.application_id, b.destination_repository AS repository, $2::text AS digest,
	 b.destination_repository || '@' || $2 AS image_reference, b.platform, b.created_by, $3::timestamptz AS created_at
	 FROM builds b WHERE b.id = $4 AND b.destination_repository = $5`, uuid.New(), result.Digest, now,
		lease.BuildID, result.Repository); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImageArtifact{}, errors.New("build artifact repository does not match accepted build")
		}
		return ImageArtifact{}, fmt.Errorf("prepare image artifact: %w", err)
	}
	if _, err := tx.NamedExecContext(ctx, `INSERT INTO image_artifacts
	 (id, build_id, project_id, application_id, repository, digest, image_reference, platform, created_by, created_at)
	 VALUES (:id, :build_id, :project_id, :application_id, :repository, :digest, :image_reference, :platform, :created_by, :created_at)`, artifact); err != nil {
		return ImageArtifact{}, fmt.Errorf("insert image artifact: %w", err)
	}
	if err := completeAttempt(ctx, tx, lease, AttemptSucceeded, Failure{LogExcerpt: result.LogExcerpt, LogTruncated: result.LogTruncated}, now); err != nil {
		return ImageArtifact{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'succeeded', recovery_required = false,
	 lease_owner = NULL, lease_expires_at = NULL, error_code = NULL, error_summary = NULL,
	 retry_disposition = NULL, updated_at = $1, finished_at = $1 WHERE id = $2`, now, lease.BuildOperationID); err != nil {
		return ImageArtifact{}, fmt.Errorf("succeed build operation: %w", err)
	}
	if err := obsoleteDispatches(ctx, tx, lease.BuildOperationID, now); err != nil {
		return ImageArtifact{}, err
	}
	if err := tx.Commit(); err != nil {
		return ImageArtifact{}, fmt.Errorf("commit build success: %w", err)
	}
	return artifact, nil
}

// Fail 依据稳定失败分类结束 Operation，或在预算内登记下一代业务调度意图。
func (m *Module) Fail(ctx context.Context, lease Lease, failure Failure) (FailureResult, error) {
	if err := validateFailure(failure, false); err != nil {
		return FailureResult{}, err
	}
	now := m.now()
	tx, automaticRetryCount, err := m.lockCompletion(ctx, lease, StatusRunning, now)
	if err != nil {
		return FailureResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeAttempt(ctx, tx, lease, AttemptFailed, failure, now); err != nil {
		return FailureResult{}, err
	}
	result := FailureResult{Status: StatusFailed}
	if failure.Disposition == Retryable && automaticRetryCount < m.maximumAutomaticRetries {
		automaticRetryCount++
		availableAt := now.Add(max(m.retryDelay(automaticRetryCount), 0))
		if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'pending',
		 automatic_retry_count = $1, recovery_required = false, available_at = $2,
		 lease_owner = NULL, lease_expires_at = NULL, error_code = NULL, error_summary = NULL,
		 retry_disposition = NULL, updated_at = $3, finished_at = NULL WHERE id = $4`,
			automaticRetryCount, availableAt, now, lease.BuildOperationID); err != nil {
			return FailureResult{}, fmt.Errorf("schedule build retry: %w", err)
		}
		if err := scheduleDispatch(ctx, tx, lease.BuildOperationID, "automatic_retry", now); err != nil {
			return FailureResult{}, err
		}
		result = FailureResult{Status: StatusPending, RetryScheduled: true, AvailableAt: &availableAt}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'failed', recovery_required = false,
		 lease_owner = NULL, lease_expires_at = NULL, error_code = $1, error_summary = $2,
		 retry_disposition = $3, updated_at = $4, finished_at = $4 WHERE id = $5`, failure.Code,
			failure.Summary, failure.Disposition, now, lease.BuildOperationID); err != nil {
			return FailureResult{}, fmt.Errorf("fail build operation: %w", err)
		}
		if err := obsoleteDispatches(ctx, tx, lease.BuildOperationID, now); err != nil {
			return FailureResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return FailureResult{}, fmt.Errorf("commit build failure: %w", err)
	}
	return result, nil
}

// HandleUnknownOutcome 保留外部 Job 可能仍然存在的事实；可恢复时只安排观察，不盲目重建。
func (m *Module) HandleUnknownOutcome(ctx context.Context, lease Lease, failure Failure, retry bool) (FailureResult, error) {
	if err := validateFailure(failure, true); err != nil {
		return FailureResult{}, err
	}
	now := m.now()
	expectedStatus := StatusRunning
	if lease.CancelRequested {
		expectedStatus = StatusCancelRequested
		// 取消结果未知时必须等待人工核验，不能退回 pending 后重新获得执行资格。
		retry = false
	}
	tx, automaticRetryCount, err := m.lockCompletion(ctx, lease, expectedStatus, now)
	if err != nil {
		return FailureResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeAttempt(ctx, tx, lease, AttemptOutcomeUnknown, failure, now); err != nil {
		return FailureResult{}, err
	}
	result := FailureResult{Status: StatusAttentionRequired}
	if retry && automaticRetryCount < m.maximumAutomaticRetries {
		availableAt := now.Add(max(m.retryDelay(automaticRetryCount+1), 0))
		if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'pending', recovery_required = true,
		 available_at = $1, lease_owner = NULL, lease_expires_at = NULL, error_code = NULL,
		 error_summary = NULL, retry_disposition = NULL, updated_at = $2, finished_at = NULL WHERE id = $3`,
			availableAt, now, lease.BuildOperationID); err != nil {
			return FailureResult{}, fmt.Errorf("schedule build reconciliation: %w", err)
		}
		if err := scheduleDispatch(ctx, tx, lease.BuildOperationID, "outcome_reconciliation", now); err != nil {
			return FailureResult{}, err
		}
		result = FailureResult{Status: StatusPending, RetryScheduled: true, AvailableAt: &availableAt}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'attention_required',
		 recovery_required = false, lease_owner = NULL, lease_expires_at = NULL, error_code = $1,
		 error_summary = $2, retry_disposition = 'unknown_outcome', updated_at = $3, finished_at = NULL
		 WHERE id = $4`, failure.Code, failure.Summary, now, lease.BuildOperationID); err != nil {
			return FailureResult{}, fmt.Errorf("require build attention: %w", err)
		}
		if err := obsoleteDispatches(ctx, tx, lease.BuildOperationID, now); err != nil {
			return FailureResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return FailureResult{}, fmt.Errorf("commit unknown build outcome: %w", err)
	}
	return result, nil
}

// ConfirmCanceled 只有当前租约持有者确认外部 Job 已停止后才能进入 canceled。
func (m *Module) ConfirmCanceled(ctx context.Context, lease Lease, logExcerpt string, truncated bool) error {
	if err := validateLog(logExcerpt); err != nil {
		return err
	}
	now := m.now()
	tx, _, err := m.lockCompletion(ctx, lease, StatusCancelRequested, now)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeAttempt(ctx, tx, lease, AttemptCanceled, Failure{LogExcerpt: logExcerpt, LogTruncated: truncated}, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE build_operations SET status = 'canceled', recovery_required = false,
	 lease_owner = NULL, lease_expires_at = NULL, updated_at = $1, finished_at = $1 WHERE id = $2`, now,
		lease.BuildOperationID); err != nil {
		return fmt.Errorf("cancel build operation: %w", err)
	}
	if err := obsoleteDispatches(ctx, tx, lease.BuildOperationID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (m *Module) lockCompletion(ctx context.Context, lease Lease, status Status, now time.Time) (*sqlx.Tx, int, error) {
	tx, err := m.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("begin build completion: %w", err)
	}
	var retryCount int
	if err := tx.GetContext(ctx, &retryCount, `SELECT automatic_retry_count FROM build_operations
	 WHERE id = $1 AND status = $2 AND lease_owner = $3 AND attempt_count = $4 AND lease_expires_at > $5
	 FOR UPDATE`, lease.BuildOperationID, status, lease.WorkerID, lease.BuildAttemptNumber, now); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, ErrLeaseLost
		}
		return nil, 0, fmt.Errorf("lock build completion: %w", err)
	}
	return tx, retryCount, nil
}

func completeAttempt(ctx context.Context, tx *sqlx.Tx, lease Lease, status AttemptStatus, failure Failure, now time.Time) error {
	var code, summary, disposition *string
	if status == AttemptFailed || status == AttemptOutcomeUnknown {
		code, summary, disposition = &failure.Code, &failure.Summary, &failure.Disposition
	}
	result, err := tx.ExecContext(ctx, `UPDATE build_attempts SET status = $1, error_code = $2,
	 error_summary = $3, retry_disposition = $4, log_excerpt = $5, log_truncated = $6, finished_at = $7
	 WHERE id = $8 AND build_operation_id = $9 AND status = 'running'`, status, code, summary, disposition,
		failure.LogExcerpt, failure.LogTruncated, now, lease.BuildAttemptID, lease.BuildOperationID)
	if err != nil {
		return fmt.Errorf("complete build attempt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("claimed build operation has no active attempt")
	}
	return nil
}

func obsoleteDispatches(ctx context.Context, tx *sqlx.Tx, operationID uuid.UUID, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE build_dispatches SET state = 'obsolete', publish_token = NULL,
	 publish_expires_at = NULL, updated_at = $2 WHERE build_operation_id = $1
	 AND state IN ('pending','published','quarantined')`, operationID, now)
	if err != nil {
		return fmt.Errorf("obsolete build dispatches: %w", err)
	}
	return nil
}

func validateFailure(failure Failure, unknown bool) error {
	if failure.Code == "" || failure.Summary == "" || utf8.RuneCountInString(failure.Code) > 64 ||
		utf8.RuneCountInString(failure.Summary) > 512 {
		return errors.New("invalid build failure evidence")
	}
	want := map[string]bool{Retryable: !unknown, NonRetryable: !unknown, UnknownOutcome: unknown}
	if !want[failure.Disposition] {
		return errors.New("invalid build failure disposition")
	}
	return validateLog(failure.LogExcerpt)
}

func validateLog(logExcerpt string) error {
	if len(logExcerpt) > 64*1024 {
		return errors.New("build log excerpt exceeds 64 KiB")
	}
	return nil
}
