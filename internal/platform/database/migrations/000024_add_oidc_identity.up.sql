BEGIN;

CREATE TABLE auth_providers (
    id text PRIMARY KEY CHECK (id ~ '^[a-z][a-z0-9-]{1,62}$'),
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 128),
    issuer text NOT NULL UNIQUE,
    allow_insecure_loopback boolean NOT NULL DEFAULT false,
    client_id text NOT NULL CHECK (char_length(client_id) BETWEEN 1 AND 256),
    client_secret_ref text NOT NULL CHECK (char_length(client_secret_ref) BETWEEN 1 AND 128),
    enabled boolean NOT NULL DEFAULT false,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CHECK (issuer ~ '^https://'
        OR (allow_insecure_loopback AND issuer ~ '^http://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?(/|$)'))
);
CREATE UNIQUE INDEX auth_providers_single_enabled_idx ON auth_providers ((true)) WHERE enabled;

CREATE TABLE external_identities (
    id uuid PRIMARY KEY,
    provider_id text NOT NULL REFERENCES auth_providers(id),
    subject text NOT NULL CHECK (char_length(subject) BETWEEN 1 AND 512),
    status text NOT NULL CHECK (status IN ('pending', 'rejected', 'linked')),
    user_id uuid REFERENCES users(id),
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 256),
    email text,
    email_verified boolean NOT NULL DEFAULT false,
    decision_by text,
    decision_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE(provider_id, subject),
    CHECK ((status = 'linked' AND user_id IS NOT NULL) OR (status IN ('pending','rejected') AND user_id IS NULL)),
    CHECK ((decision_by IS NULL AND decision_at IS NULL) OR (decision_by IS NOT NULL AND decision_at IS NOT NULL))
);
CREATE UNIQUE INDEX external_identities_user_provider_idx ON external_identities(user_id, provider_id) WHERE user_id IS NOT NULL;
CREATE INDEX external_identities_admission_idx ON external_identities(status, created_at, id);

ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_auth_method_check;
ALTER TABLE auth_sessions ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE auth_sessions ALTER COLUMN auth_version DROP NOT NULL;
ALTER TABLE auth_sessions ALTER COLUMN primary_authenticated_at DROP NOT NULL;
ALTER TABLE auth_sessions ADD COLUMN external_identity_id uuid REFERENCES external_identities(id),
    ADD COLUMN provider_id text REFERENCES auth_providers(id),
    ADD CONSTRAINT auth_sessions_auth_method_check CHECK (auth_method IN ('password','oidc')),
    ADD CONSTRAINT auth_sessions_subject_check CHECK (
      (auth_method='password' AND user_id IS NOT NULL AND auth_version IS NOT NULL
       AND external_identity_id IS NULL AND provider_id IS NULL)
      OR
      (auth_method='oidc' AND user_id IS NULL AND auth_version IS NULL
       AND external_identity_id IS NOT NULL AND provider_id IS NOT NULL)
    );
CREATE INDEX auth_sessions_external_identity_idx ON auth_sessions(external_identity_id);

CREATE TABLE oidc_transactions (
    id uuid PRIMARY KEY,
    state_hash bytea NOT NULL UNIQUE CHECK (octet_length(state_hash)=32),
    browser_hash bytea NOT NULL CHECK (octet_length(browser_hash)=32),
    nonce_hash bytea NOT NULL CHECK (octet_length(nonce_hash)=32),
    provider_id text NOT NULL REFERENCES auth_providers(id),
    mode text NOT NULL CHECK (mode IN ('login','bind','reauth')),
    original_user_id uuid REFERENCES users(id),
    original_session_id uuid REFERENCES auth_sessions(id),
    verifier_ciphertext bytea NOT NULL,
    verifier_nonce bytea NOT NULL CHECK (octet_length(verifier_nonce)=12),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    consumed_at timestamptz,
    CHECK ((mode='login' AND original_user_id IS NULL AND original_session_id IS NULL)
       OR (mode IN ('bind','reauth') AND original_user_id IS NOT NULL AND original_session_id IS NOT NULL))
);
CREATE INDEX oidc_transactions_expiry_idx ON oidc_transactions(expires_at);

DROP VIEW effective_identity_users;
CREATE VIEW effective_identity_users AS
SELECT u.id
FROM users u
WHERE u.status='active' AND (
  EXISTS (SELECT 1 FROM local_credentials lc WHERE lc.user_id=u.id
    AND lc.password_hash ~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{21}[AQgw]\$[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$')
  OR EXISTS (SELECT 1 FROM external_identities ei JOIN auth_providers ap ON ap.id=ei.provider_id
    WHERE ei.user_id=u.id AND ei.status='linked' AND ap.enabled)
);

COMMIT;
