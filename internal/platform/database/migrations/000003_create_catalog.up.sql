CREATE TABLE applications (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    name text NOT NULL,
    slug text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (project_id, slug)
);

CREATE TABLE deployment_targets (
    id uuid PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id),
    stage text NOT NULL CHECK (stage = 'development'),
    cluster_ref text NOT NULL,
    namespace text NOT NULL,
    replicas integer NOT NULL CHECK (replicas BETWEEN 1 AND 5),
    container_port integer NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (application_id, stage)
);
