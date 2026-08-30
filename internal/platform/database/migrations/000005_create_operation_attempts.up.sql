ALTER TABLE operations
    ADD CONSTRAINT operations_lease_state_check
    CHECK ((status = 'pending' AND lease_owner IS NULL AND lease_expires_at IS NULL)
        OR (status = 'running' AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status IN ('succeeded', 'failed') AND lease_owner IS NULL AND lease_expires_at IS NULL));

ALTER TABLE operations
    ADD CONSTRAINT operations_terminal_result_check
    CHECK ((status IN ('pending', 'running') AND finished_at IS NULL
            AND error_category IS NULL AND error_summary IS NULL)
        OR (status = 'succeeded' AND finished_at IS NOT NULL
            AND error_category IS NULL AND error_summary IS NULL)
        OR (status = 'failed' AND finished_at IS NOT NULL
            AND error_category IS NOT NULL AND error_summary IS NOT NULL));

CREATE TABLE operation_attempts (
    id uuid PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES operations (id),
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    worker_id text NOT NULL,
    status text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed')),
    error_category text,
    error_summary text,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    UNIQUE (operation_id, attempt_number),
    CHECK ((status = 'running' AND finished_at IS NULL)
        OR (status IN ('succeeded', 'failed') AND finished_at IS NOT NULL)),
    CHECK ((status IN ('running', 'succeeded') AND error_category IS NULL AND error_summary IS NULL)
        OR (status = 'failed' AND error_category IS NOT NULL AND error_summary IS NOT NULL))
);

CREATE INDEX operation_attempts_operation_idx
    ON operation_attempts (operation_id, attempt_number);
