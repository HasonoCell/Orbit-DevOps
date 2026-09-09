CREATE TABLE builds (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    application_id uuid NOT NULL REFERENCES applications (id),
    repository_url text NOT NULL,
    source_commit text NOT NULL,
    dockerfile_path text NOT NULL,
    context_path text NOT NULL,
    platform text NOT NULL,
    destination_repository text NOT NULL,
    input_digest text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    CHECK (source_commit ~ '^[a-f0-9]{40}([a-f0-9]{24})?$'),
    CHECK (input_digest ~ '^sha256:[a-f0-9]{64}$')
);

CREATE INDEX builds_application_history_idx
    ON builds (application_id, created_at DESC, id DESC);

CREATE TABLE build_operations (
    id uuid PRIMARY KEY,
    build_id uuid NOT NULL UNIQUE REFERENCES builds (id),
    actor_id text NOT NULL,
    idempotency_key text NOT NULL,
    traceparent text NOT NULL DEFAULT '',
    tracestate text NOT NULL DEFAULT '',
    status text NOT NULL CHECK (status IN (
        'pending', 'running', 'cancel_requested', 'attention_required',
        'succeeded', 'failed', 'canceled'
    )),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    automatic_retry_count integer NOT NULL DEFAULT 0 CHECK (automatic_retry_count >= 0),
    recovery_required boolean NOT NULL DEFAULT false,
    error_code text,
    error_summary text,
    retry_disposition text CHECK (retry_disposition IS NULL OR retry_disposition IN (
        'retryable', 'non_retryable', 'unknown_outcome'
    )),
    queued_at timestamptz NOT NULL,
    available_at timestamptz NOT NULL,
    lease_owner text,
    lease_expires_at timestamptz,
    current_dispatch_sequence bigint NOT NULL DEFAULT 0 CHECK (current_dispatch_sequence >= 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK ((status IN ('succeeded', 'failed', 'canceled') AND finished_at IS NOT NULL)
        OR (status NOT IN ('succeeded', 'failed', 'canceled') AND finished_at IS NULL))
);

CREATE INDEX build_operations_claim_idx
    ON build_operations (available_at, queued_at, id)
    WHERE status = 'pending';
CREATE INDEX build_operations_expired_lease_idx
    ON build_operations (lease_expires_at, id)
    WHERE status IN ('running', 'cancel_requested');

CREATE TABLE build_attempts (
    id uuid PRIMARY KEY,
    build_operation_id uuid NOT NULL REFERENCES build_operations (id),
    attempt_number integer NOT NULL CHECK (attempt_number > 0),
    worker_id text NOT NULL,
    status text NOT NULL CHECK (status IN (
        'running', 'succeeded', 'failed', 'canceled', 'outcome_unknown'
    )),
    recovered_from_attempt_id uuid REFERENCES build_attempts (id),
    executor_name text,
    executor_uid text,
    error_code text,
    error_summary text,
    retry_disposition text CHECK (retry_disposition IS NULL OR retry_disposition IN (
        'retryable', 'non_retryable', 'unknown_outcome'
    )),
    log_excerpt text NOT NULL DEFAULT '',
    log_truncated boolean NOT NULL DEFAULT false,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    UNIQUE (build_operation_id, attempt_number),
    CHECK ((status = 'running' AND finished_at IS NULL)
        OR (status <> 'running' AND finished_at IS NOT NULL))
);

CREATE INDEX build_attempts_operation_idx
    ON build_attempts (build_operation_id, attempt_number);

CREATE TABLE build_dispatches (
    id uuid PRIMARY KEY,
    build_operation_id uuid NOT NULL REFERENCES build_operations (id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    protocol_version integer NOT NULL DEFAULT 1,
    dispatch_reason text NOT NULL,
    expected_attempt_count integer NOT NULL CHECK (expected_attempt_count >= 0),
    state text NOT NULL CHECK (state IN (
        'pending', 'published', 'consumed', 'obsolete', 'quarantined'
    )),
    available_at timestamptz NOT NULL,
    next_dispatch_at timestamptz NOT NULL,
    publish_token uuid,
    publish_expires_at timestamptz,
    reservation_count integer NOT NULL DEFAULT 0 CHECK (reservation_count >= 0),
    last_error_code text,
    published_at timestamptz,
    consumed_at timestamptz,
    build_attempt_id uuid REFERENCES build_attempts (id),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (build_operation_id, sequence),
    CHECK ((publish_token IS NULL) = (publish_expires_at IS NULL))
);

CREATE INDEX build_dispatches_due_idx
    ON build_dispatches (next_dispatch_at, id)
    WHERE state IN ('pending', 'published');

CREATE TABLE image_artifacts (
    id uuid PRIMARY KEY,
    build_id uuid NOT NULL UNIQUE REFERENCES builds (id),
    project_id uuid NOT NULL REFERENCES projects (id),
    application_id uuid NOT NULL REFERENCES applications (id),
    repository text NOT NULL,
    digest text NOT NULL,
    image_reference text NOT NULL,
    platform text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    CHECK (digest ~ '^sha256:[a-f0-9]{64}$'),
    CHECK (image_reference ~ '@sha256:[a-f0-9]{64}$')
);

CREATE INDEX image_artifacts_application_history_idx
    ON image_artifacts (application_id, created_at DESC, id DESC);

ALTER TABLE releases
    ADD COLUMN image_artifact_id uuid REFERENCES image_artifacts (id);

CREATE INDEX releases_image_artifact_idx
    ON releases (image_artifact_id)
    WHERE image_artifact_id IS NOT NULL;
