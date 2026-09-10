CREATE TABLE delivery_pipelines (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    application_id uuid NOT NULL REFERENCES applications (id),
    name text NOT NULL,
    current_revision integer NOT NULL CHECK (current_revision > 0),
    enabled boolean NOT NULL DEFAULT false,
    activation_generation bigint NOT NULL DEFAULT 0 CHECK (activation_generation >= 0),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (application_id, name),
    UNIQUE (id, current_revision)
);

CREATE TABLE delivery_pipeline_revisions (
    delivery_pipeline_id uuid NOT NULL REFERENCES delivery_pipelines (id),
    revision integer NOT NULL CHECK (revision > 0),
    provider text NOT NULL CHECK (provider IN ('github')),
    endpoint_key text NOT NULL,
    repository_id bigint NOT NULL CHECK (repository_id > 0),
    repository_owner_id bigint NOT NULL CHECK (repository_owner_id > 0),
    repository_full_name text NOT NULL,
    repository_url text NOT NULL,
    git_ref text NOT NULL CHECK (git_ref LIKE 'refs/heads/%'),
    dockerfile_path text NOT NULL,
    context_path text NOT NULL,
    platform text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('build_only', 'auto_release')),
    deployment_target_id uuid REFERENCES deployment_targets (id),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (delivery_pipeline_id, revision),
    CHECK ((mode = 'auto_release') = (deployment_target_id IS NOT NULL))
);

ALTER TABLE delivery_pipelines
    ADD CONSTRAINT delivery_pipelines_current_revision_fk
    FOREIGN KEY (id, current_revision)
    REFERENCES delivery_pipeline_revisions (delivery_pipeline_id, revision)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE webhook_deliveries (
    id uuid PRIMARY KEY,
    endpoint_key text NOT NULL,
    provider_delivery_id text NOT NULL,
    event_type text NOT NULL,
    payload_digest text NOT NULL CHECK (payload_digest ~ '^sha256:[a-f0-9]{64}$'),
    payload_size integer NOT NULL CHECK (payload_size >= 0),
    state text NOT NULL CHECK (state IN ('pending', 'processed', 'ignored', 'quarantined')),
    repository_id bigint,
    repository_owner_id bigint,
    repository_full_name text,
    git_ref text,
    before_commit text,
    after_commit text,
    forced boolean NOT NULL DEFAULT false,
    deleted boolean NOT NULL DEFAULT false,
    reason_code text,
    traceparent text NOT NULL DEFAULT '',
    tracestate text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL,
    processed_at timestamptz,
    UNIQUE (endpoint_key, provider_delivery_id)
);

CREATE INDEX webhook_deliveries_retention_idx
    ON webhook_deliveries (received_at, id)
    WHERE state IN ('processed', 'ignored', 'quarantined');

CREATE TABLE delivery_runs (
    id uuid PRIMARY KEY,
    delivery_pipeline_id uuid NOT NULL REFERENCES delivery_pipelines (id),
    pipeline_revision integer NOT NULL,
    activation_generation bigint NOT NULL CHECK (activation_generation >= 0),
    webhook_delivery_id uuid NOT NULL REFERENCES webhook_deliveries (id),
    source_commit text NOT NULL CHECK (source_commit ~ '^[a-f0-9]{40}([a-f0-9]{24})?$'),
    repository_url text NOT NULL,
    phase text NOT NULL CHECK (phase IN (
        'build_created', 'artifact_ready', 'release_created',
        'completed', 'superseded', 'blocked'
    )),
    phase_version bigint NOT NULL DEFAULT 1 CHECK (phase_version > 0),
    build_id uuid NOT NULL UNIQUE REFERENCES builds (id),
    image_artifact_id uuid UNIQUE REFERENCES image_artifacts (id),
    release_id uuid UNIQUE REFERENCES releases (id),
    reason_code text,
    source_check_attempt_count integer NOT NULL DEFAULT 0 CHECK (source_check_attempt_count >= 0),
    source_check_started_at timestamptz,
    source_check_next_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    finished_at timestamptz,
    UNIQUE (delivery_pipeline_id, pipeline_revision, source_commit),
    FOREIGN KEY (delivery_pipeline_id, pipeline_revision)
        REFERENCES delivery_pipeline_revisions (delivery_pipeline_id, revision),
    CHECK ((phase IN ('completed', 'superseded', 'blocked') AND finished_at IS NOT NULL)
        OR (phase NOT IN ('completed', 'superseded', 'blocked') AND finished_at IS NULL))
);

CREATE INDEX delivery_runs_pipeline_history_idx
    ON delivery_runs (delivery_pipeline_id, created_at DESC, id DESC);
CREATE INDEX delivery_runs_repair_idx
    ON delivery_runs (source_check_next_at, id)
    WHERE phase = 'artifact_ready';

CREATE TABLE internal_event_outbox (
    id uuid PRIMARY KEY,
    topic text NOT NULL CHECK (topic IN (
        'webhook_delivery.received.v1',
        'build_operation.changed.v1',
        'release_operation.changed.v1',
        'delivery_run.reconcile.v1'
    )),
    aggregate_id uuid NOT NULL,
    protocol_version integer NOT NULL DEFAULT 1 CHECK (protocol_version = 1),
    state text NOT NULL CHECK (state IN ('pending', 'published', 'consumed', 'obsolete', 'quarantined')),
    available_at timestamptz NOT NULL,
    next_dispatch_at timestamptz NOT NULL,
    publish_token uuid,
    publish_expires_at timestamptz,
    reservation_count integer NOT NULL DEFAULT 0 CHECK (reservation_count >= 0),
    last_error_code text,
    traceparent text NOT NULL DEFAULT '',
    tracestate text NOT NULL DEFAULT '',
    published_at timestamptz,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK ((publish_token IS NULL) = (publish_expires_at IS NULL))
);

CREATE INDEX internal_event_outbox_due_idx
    ON internal_event_outbox (next_dispatch_at, id)
    WHERE state IN ('pending', 'published');
CREATE INDEX internal_event_outbox_retention_idx
    ON internal_event_outbox (consumed_at, id)
    WHERE state = 'consumed';
