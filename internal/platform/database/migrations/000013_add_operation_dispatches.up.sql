-- 分发代次独立于业务 Attempt；旧版本可继续写入默认值，重新切入队列前离线准备。
ALTER TABLE operations ADD COLUMN dispatch_generation bigint NOT NULL DEFAULT 0
    CHECK (dispatch_generation >= 0);

CREATE TABLE operation_dispatches (
    id uuid PRIMARY KEY,
    operation_id uuid NOT NULL REFERENCES operations(id),
    generation bigint NOT NULL CHECK (generation > 0),
    version integer NOT NULL DEFAULT 1,
    reason text NOT NULL,
    expected_attempt_count integer NOT NULL CHECK (expected_attempt_count >= 0),
    state text NOT NULL CHECK (state IN ('pending', 'published', 'consumed', 'obsolete', 'quarantined')),
    available_at timestamptz NOT NULL,
    next_dispatch_at timestamptz NOT NULL,
    publish_token uuid,
    publish_expires_at timestamptz,
    delivery_count integer NOT NULL DEFAULT 0 CHECK (delivery_count >= 0),
    last_error_code text,
    published_at timestamptz,
    consumed_at timestamptz,
    attempt_id uuid REFERENCES operation_attempts(id),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (operation_id, generation),
    CHECK ((publish_token IS NULL) = (publish_expires_at IS NULL))
);

CREATE INDEX operation_dispatches_due_idx
    ON operation_dispatches(next_dispatch_at, id)
    WHERE state IN ('pending', 'published');
CREATE INDEX operations_expired_lease_idx
    ON operations(lease_expires_at, id)
    WHERE status IN ('running', 'cancel_requested');
