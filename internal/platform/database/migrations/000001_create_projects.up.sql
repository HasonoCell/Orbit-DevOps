CREATE TABLE projects (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE audit_records (
    id uuid PRIMARY KEY,
    actor_id text NOT NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    summary jsonb NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE INDEX audit_records_target_idx
    ON audit_records (target_type, target_id, created_at);
