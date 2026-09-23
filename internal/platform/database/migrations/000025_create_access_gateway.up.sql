-- Host 在清理完成前保留域名声明；同一集群的域名不能被另一个项目并发抢占。
CREATE TABLE access_secret_bindings (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    cluster_ref text NOT NULL,
    namespace text NOT NULL,
    hostname text NOT NULL,
    secret_name text NOT NULL,
    state text NOT NULL CHECK (state IN ('active', 'revoked')),
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (id, project_id, cluster_ref, namespace, hostname),
    UNIQUE (project_id, cluster_ref, namespace, hostname, secret_name)
);

CREATE TABLE access_hosts (
    id uuid PRIMARY KEY,
    project_id uuid NOT NULL REFERENCES projects (id),
    cluster_ref text NOT NULL,
    namespace text NOT NULL,
    hostname text NOT NULL,
    tls_mode text NOT NULL CHECK (tls_mode IN ('http_only', 'managed', 'existing_secret')),
    issuer_policy_key text,
    secret_binding_id uuid,
    lifecycle text NOT NULL CHECK (lifecycle IN ('active', 'deleting')),
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (cluster_ref, hostname),
    FOREIGN KEY (secret_binding_id, project_id, cluster_ref, namespace, hostname)
        REFERENCES access_secret_bindings (id, project_id, cluster_ref, namespace, hostname),
    CHECK (
        (tls_mode = 'http_only' AND issuer_policy_key IS NULL AND secret_binding_id IS NULL) OR
        (tls_mode = 'managed' AND issuer_policy_key IS NOT NULL AND secret_binding_id IS NULL) OR
        (tls_mode = 'existing_secret' AND issuer_policy_key IS NULL AND secret_binding_id IS NOT NULL)
    )
);

CREATE INDEX access_hosts_project_idx ON access_hosts (project_id, created_at, id);

CREATE TABLE access_routes (
    id uuid PRIMARY KEY,
    host_id uuid NOT NULL REFERENCES access_hosts (id),
    deployment_target_id uuid NOT NULL REFERENCES deployment_targets (id),
    path_prefix text NOT NULL,
    lifecycle text NOT NULL CHECK (lifecycle IN ('active', 'deleting')),
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (host_id, path_prefix)
);

CREATE INDEX access_routes_target_idx ON access_routes (deployment_target_id);

-- 一行代表一个 Project 在当前受控集群/Namespace 的完整 Gateway 期望集合。
CREATE TABLE project_gateway_sync (
    project_id uuid NOT NULL REFERENCES projects (id),
    cluster_ref text NOT NULL,
    namespace text NOT NULL,
    desired_revision bigint NOT NULL CHECK (desired_revision >= 0),
    applied_revision bigint NOT NULL CHECK (applied_revision >= 0),
    state text NOT NULL CHECK (state IN ('pending', 'applying', 'applied', 'retrying', 'attention_required', 'deleting')),
    lease_token uuid,
    lease_expires_at timestamptz,
    next_attempt_at timestamptz NOT NULL,
    last_error_code text,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, cluster_ref, namespace),
    CHECK ((lease_token IS NULL) = (lease_expires_at IS NULL))
);

CREATE INDEX project_gateway_sync_due_idx ON project_gateway_sync (next_attempt_at, project_id)
    WHERE state IN ('pending', 'retrying', 'applying', 'deleting');

ALTER TABLE internal_event_outbox DROP CONSTRAINT internal_event_outbox_topic_check;
ALTER TABLE internal_event_outbox ADD CONSTRAINT internal_event_outbox_topic_check CHECK (topic IN (
    'webhook_delivery.received.v1',
    'build_operation.changed.v1',
    'release_operation.changed.v1',
    'delivery_run.reconcile.v1',
    'project_gateway.reconcile.v1'
));
