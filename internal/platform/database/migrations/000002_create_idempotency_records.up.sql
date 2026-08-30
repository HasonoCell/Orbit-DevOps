CREATE TABLE idempotency_records (
    actor_id text NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash bytea NOT NULL,
    resource_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (actor_id, operation, idempotency_key)
);
