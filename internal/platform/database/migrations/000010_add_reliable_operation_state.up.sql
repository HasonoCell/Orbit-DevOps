ALTER TABLE operations
    ADD COLUMN deployment_target_id uuid,
    ADD COLUMN queued_at timestamptz,
    ADD COLUMN available_at timestamptz,
    ADD COLUMN automatic_retry_count integer NOT NULL DEFAULT 0
        CHECK (automatic_retry_count >= 0),
    ADD COLUMN retry_disposition text;

UPDATE operations
SET deployment_target_id = releases.deployment_target_id,
    queued_at = operations.created_at,
    available_at = operations.created_at
FROM releases
WHERE releases.id = operations.release_id;

UPDATE operations
SET retry_disposition = 'non_retryable'
WHERE status = 'failed';

ALTER TABLE operations
    ALTER COLUMN deployment_target_id SET NOT NULL,
    ALTER COLUMN queued_at SET NOT NULL,
    ALTER COLUMN available_at SET NOT NULL;

ALTER TABLE operations
    RENAME COLUMN error_category TO error_code;

ALTER TABLE releases
    ADD CONSTRAINT releases_id_target_unique UNIQUE (id, deployment_target_id);

ALTER TABLE operations
    DROP CONSTRAINT operations_release_id_fkey,
    ADD CONSTRAINT operations_release_target_fkey
        FOREIGN KEY (release_id, deployment_target_id)
        REFERENCES releases (id, deployment_target_id),
    DROP CONSTRAINT operations_status_check,
    DROP CONSTRAINT operations_check,
    DROP CONSTRAINT operations_check1,
    DROP CONSTRAINT operations_lease_state_check,
    DROP CONSTRAINT operations_terminal_result_check,
    ADD CONSTRAINT operations_status_check
        CHECK (status IN (
            'pending', 'running', 'cancel_requested', 'succeeded',
            'failed', 'canceled', 'attention_required'
        )),
    ADD CONSTRAINT operations_lease_state_check
        CHECK (
            (status = 'pending' AND lease_owner IS NULL AND lease_expires_at IS NULL)
            OR (status IN ('running', 'cancel_requested')
                AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
            OR (status IN ('succeeded', 'failed', 'canceled', 'attention_required')
                AND lease_owner IS NULL AND lease_expires_at IS NULL)
        ),
    ADD CONSTRAINT operations_result_check
        CHECK (
            (status IN ('pending', 'running', 'cancel_requested', 'succeeded', 'canceled')
                AND error_code IS NULL AND error_summary IS NULL
                AND retry_disposition IS NULL)
            OR (status IN ('failed', 'attention_required')
                AND error_code IS NOT NULL AND error_summary IS NOT NULL
                AND retry_disposition IS NOT NULL)
        ),
    ADD CONSTRAINT operations_finished_at_check
        CHECK (
            (status IN ('pending', 'running', 'cancel_requested') AND finished_at IS NULL)
            OR (status IN ('succeeded', 'failed', 'canceled', 'attention_required')
                AND finished_at IS NOT NULL)
        ),
    ADD CONSTRAINT operations_retry_disposition_check
        CHECK (retry_disposition IS NULL OR retry_disposition IN (
            'retryable', 'non_retryable', 'unknown_outcome'
        ));

DROP INDEX operations_claim_idx;

CREATE INDEX operations_claim_idx
    ON operations (status, available_at, queued_at, id);

ALTER TABLE operation_attempts
    ADD COLUMN retry_disposition text;

UPDATE operation_attempts
SET retry_disposition = 'non_retryable'
WHERE status = 'failed';

ALTER TABLE operation_attempts
    RENAME COLUMN error_category TO error_code;

ALTER TABLE operation_attempts
    DROP CONSTRAINT operation_attempts_status_check,
    DROP CONSTRAINT operation_attempts_check,
    DROP CONSTRAINT operation_attempts_check1,
    ADD CONSTRAINT operation_attempts_status_check
        CHECK (status IN ('running', 'succeeded', 'failed', 'canceled', 'outcome_unknown')),
    ADD CONSTRAINT operation_attempts_finished_at_check
        CHECK (
            (status = 'running' AND finished_at IS NULL)
            OR (status IN ('succeeded', 'failed', 'canceled', 'outcome_unknown')
                AND finished_at IS NOT NULL)
        ),
    ADD CONSTRAINT operation_attempts_result_check
        CHECK (
            (status IN ('running', 'succeeded', 'canceled')
                AND error_code IS NULL AND error_summary IS NULL
                AND retry_disposition IS NULL)
            OR (status = 'failed'
                AND error_code IS NOT NULL AND error_summary IS NOT NULL
                AND retry_disposition IN ('retryable', 'non_retryable'))
            OR (status = 'outcome_unknown'
                AND error_code IS NOT NULL AND error_summary IS NOT NULL
                AND retry_disposition = 'unknown_outcome')
        ),
    ADD CONSTRAINT operation_attempts_retry_disposition_check
        CHECK (retry_disposition IS NULL OR retry_disposition IN (
            'retryable', 'non_retryable', 'unknown_outcome'
        ));

-- S1 没有目标级串行约束。若升级时已存在同一目标的并发运行记录，保留最早
-- Operation 的租约，其余结果因可能已写入 Kubernetes 而进入人工处理状态。
WITH duplicate_active AS (
    SELECT id
    FROM (
        SELECT id,
               row_number() OVER (
                   PARTITION BY deployment_target_id
                   ORDER BY queued_at, id
               ) AS target_position
        FROM operations
        WHERE status IN ('running', 'cancel_requested')
    ) AS ranked
    WHERE target_position > 1
)
UPDATE operation_attempts
SET status = 'outcome_unknown',
    error_code = 'concurrent_target_operation',
    error_summary = 'operation was already running during target-serial scheduling migration',
    retry_disposition = 'unknown_outcome',
    finished_at = now()
WHERE operation_id IN (SELECT id FROM duplicate_active)
  AND status = 'running';

WITH duplicate_active AS (
    SELECT id
    FROM (
        SELECT id,
               row_number() OVER (
                   PARTITION BY deployment_target_id
                   ORDER BY queued_at, id
               ) AS target_position
        FROM operations
        WHERE status IN ('running', 'cancel_requested')
    ) AS ranked
    WHERE target_position > 1
)
UPDATE operations
SET status = 'attention_required',
    lease_owner = NULL,
    lease_expires_at = NULL,
    error_code = 'concurrent_target_operation',
    error_summary = 'operation was already running during target-serial scheduling migration',
    retry_disposition = 'unknown_outcome',
    updated_at = now(),
    finished_at = now()
WHERE id IN (SELECT id FROM duplicate_active);

CREATE UNIQUE INDEX operations_active_target_idx
    ON operations (deployment_target_id)
    WHERE status IN ('running', 'cancel_requested');
