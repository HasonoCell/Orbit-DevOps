CREATE TABLE project_members (
    project_id uuid NOT NULL REFERENCES projects (id),
    actor_id text NOT NULL CHECK (char_length(actor_id) BETWEEN 1 AND 128),
    role text NOT NULL CHECK (role IN ('owner', 'developer', 'viewer')),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, actor_id)
);

CREATE INDEX project_members_actor_idx
    ON project_members (actor_id, project_id);

INSERT INTO project_members
    (project_id, actor_id, role, created_by, created_at, updated_at)
SELECT id, created_by, 'owner', created_by, created_at, created_at
FROM projects;

ALTER TABLE audit_records
    ADD COLUMN actor_kind text NOT NULL DEFAULT 'user'
        CHECK (actor_kind IN ('user', 'system'));
