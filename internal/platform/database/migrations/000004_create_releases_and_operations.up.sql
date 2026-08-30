CREATE TABLE releases (
    id uuid PRIMARY KEY,
    deployment_target_id uuid NOT NULL REFERENCES deployment_targets (id),
    image_reference text NOT NULL,
    target_snapshot jsonb NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE operations (
    id uuid PRIMARY KEY,
    operation_type text NOT NULL CHECK (operation_type = 'release.deploy'),
    release_id uuid NOT NULL UNIQUE REFERENCES releases (id),
    actor_id text NOT NULL,
    idempotency_key text NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_owner text,
    lease_expires_at timestamptz,
    error_category text,
    error_summary text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    CHECK ((status = 'pending' AND lease_owner IS NULL AND lease_expires_at IS NULL)
        OR status <> 'pending'),
    CHECK ((status IN ('pending', 'running') AND finished_at IS NULL)
        OR status IN ('succeeded', 'failed'))
);

CREATE INDEX operations_claim_idx
    ON operations (status, lease_expires_at, created_at);
