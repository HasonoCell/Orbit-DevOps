ALTER TABLE operation_attempts
    DROP CONSTRAINT operation_attempts_retry_disposition_check,
    DROP CONSTRAINT operation_attempts_result_check,
    DROP CONSTRAINT operation_attempts_finished_at_check,
    DROP CONSTRAINT operation_attempts_status_check;

ALTER TABLE operation_attempts
    RENAME COLUMN error_code TO error_category;

ALTER TABLE operation_attempts
    DROP COLUMN retry_disposition,
    ADD CONSTRAINT operation_attempts_status_check
        CHECK (status IN ('running', 'succeeded', 'failed')),
    ADD CHECK (
        (status = 'running' AND finished_at IS NULL)
        OR (status IN ('succeeded', 'failed') AND finished_at IS NOT NULL)
    ),
    ADD CHECK (
        (status IN ('running', 'succeeded')
            AND error_category IS NULL AND error_summary IS NULL)
        OR (status = 'failed'
            AND error_category IS NOT NULL AND error_summary IS NOT NULL)
    );

DROP INDEX operations_active_target_idx;
DROP INDEX operations_claim_idx;

CREATE INDEX operations_claim_idx
    ON operations (status, lease_expires_at, created_at);

ALTER TABLE operations
    DROP CONSTRAINT operations_retry_disposition_check,
    DROP CONSTRAINT operations_finished_at_check,
    DROP CONSTRAINT operations_result_check,
    DROP CONSTRAINT operations_lease_state_check,
    DROP CONSTRAINT operations_status_check,
    DROP CONSTRAINT operations_release_target_fkey;

ALTER TABLE operations
    RENAME COLUMN error_code TO error_category;

ALTER TABLE operations
    DROP COLUMN retry_disposition,
    DROP COLUMN automatic_retry_count,
    DROP COLUMN available_at,
    DROP COLUMN queued_at,
    DROP COLUMN deployment_target_id,
    ADD CONSTRAINT operations_release_id_fkey
        FOREIGN KEY (release_id) REFERENCES releases (id),
    ADD CONSTRAINT operations_status_check
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    ADD CONSTRAINT operations_lease_state_check
        CHECK (
            (status = 'pending' AND lease_owner IS NULL AND lease_expires_at IS NULL)
            OR (status = 'running' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
            OR (status IN ('succeeded', 'failed')
                AND lease_owner IS NULL AND lease_expires_at IS NULL)
        ),
    ADD CONSTRAINT operations_terminal_result_check
        CHECK (
            (status IN ('pending', 'running') AND finished_at IS NULL
                AND error_category IS NULL AND error_summary IS NULL)
            OR (status = 'succeeded' AND finished_at IS NOT NULL
                AND error_category IS NULL AND error_summary IS NULL)
            OR (status = 'failed' AND finished_at IS NOT NULL
                AND error_category IS NOT NULL AND error_summary IS NOT NULL)
        ),
    ADD CHECK (
        (status = 'pending' AND lease_owner IS NULL AND lease_expires_at IS NULL)
        OR status <> 'pending'
    ),
    ADD CHECK (
        (status IN ('pending', 'running') AND finished_at IS NULL)
        OR status IN ('succeeded', 'failed')
    );

ALTER TABLE releases
    DROP CONSTRAINT releases_id_target_unique;
