BEGIN;

-- 内部身份与登录名分离；业务历史的文本 Actor 保持原样。
CREATE TABLE users (
    id uuid PRIMARY KEY,
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 128),
    status text NOT NULL CHECK (status IN ('active', 'disabled')),
    platform_role text NOT NULL CHECK (platform_role IN ('platform_admin', 'user')),
    auth_version bigint NOT NULL DEFAULT 1 CHECK (auth_version > 0),
    admitted_by text NOT NULL,
    admitted_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE local_credentials (
    user_id uuid PRIMARY KEY REFERENCES users (id),
    login_name text NOT NULL UNIQUE
        CHECK (login_name ~ '^[a-z0-9][a-z0-9._@+-]{2,127}$'),
    password_hash text NOT NULL CHECK (char_length(password_hash) BETWEEN 32 AND 512),
    must_change_password boolean NOT NULL,
    updated_at timestamptz NOT NULL
);

-- 本切片先建立本地 Session；OIDC 切片再以 CHECK/FK 扩展主体与方法。
CREATE TABLE auth_sessions (
    id uuid PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    user_id uuid NOT NULL REFERENCES users (id),
    auth_version bigint NOT NULL CHECK (auth_version > 0),
    auth_method text NOT NULL CHECK (auth_method = 'password'),
    primary_authenticated_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz,
    CHECK (last_seen_at >= created_at)
);
CREATE INDEX auth_sessions_user_idx ON auth_sessions (user_id);
CREATE INDEX auth_sessions_expiry_idx ON auth_sessions (expires_at);

-- 每个派生主体只保留当前窗口；用户名/IP 不以明文保存在限流事实中。
CREATE TABLE auth_rate_limits (
    subject_key bytea PRIMARY KEY CHECK (octet_length(subject_key) = 32),
    window_started_at timestamptz NOT NULL,
    attempt_count integer NOT NULL CHECK (attempt_count > 0),
    expires_at timestamptz NOT NULL CHECK (expires_at > window_started_at)
);
CREATE INDEX auth_rate_limits_expiry_idx ON auth_rate_limits (expires_at);

-- 此行必须存在；业务共享锁与安全变更排他锁都在资源排他锁之前取得。
CREATE TABLE identity_control (
    id smallint PRIMARY KEY CHECK (id = 1),
    initialized_at timestamptz
);
INSERT INTO identity_control (id) VALUES (1);

-- 已初始化标记不是管理员计数，正常更新不得将它清空或重设。
CREATE FUNCTION preserve_identity_initialization() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.initialized_at IS NOT NULL
       AND NEW.initialized_at IS DISTINCT FROM OLD.initialized_at THEN
        RAISE EXCEPTION 'identity initialization is permanent';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER identity_initialization_is_permanent
    BEFORE UPDATE ON identity_control
    FOR EACH ROW EXECUTE FUNCTION preserve_identity_initialization();

COMMIT;
