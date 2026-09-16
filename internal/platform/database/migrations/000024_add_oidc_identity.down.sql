BEGIN;
DO $$ BEGIN
  IF EXISTS(SELECT 1 FROM oidc_transactions)
     OR EXISTS(SELECT 1 FROM external_identities)
     OR EXISTS(SELECT 1 FROM auth_providers)
     OR EXISTS(SELECT 1 FROM auth_sessions WHERE auth_method='oidc') THEN
    RAISE EXCEPTION 'cannot remove OIDC identity schema while OIDC facts exist';
  END IF;
END $$;
DROP VIEW effective_identity_users;
CREATE VIEW effective_identity_users AS
SELECT u.id FROM users u JOIN local_credentials lc ON lc.user_id=u.id
WHERE u.status='active'
  AND lc.password_hash ~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{21}[AQgw]\$[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$';
DROP TABLE oidc_transactions;
DROP INDEX auth_sessions_external_identity_idx;
ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_subject_check;
ALTER TABLE auth_sessions DROP CONSTRAINT auth_sessions_auth_method_check;
ALTER TABLE auth_sessions DROP COLUMN provider_id, DROP COLUMN external_identity_id;
ALTER TABLE auth_sessions ALTER COLUMN primary_authenticated_at SET NOT NULL;
ALTER TABLE auth_sessions ALTER COLUMN auth_version SET NOT NULL;
ALTER TABLE auth_sessions ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE auth_sessions ADD CONSTRAINT auth_sessions_auth_method_check CHECK (auth_method='password');
DROP TABLE external_identities;
DROP TABLE auth_providers;
COMMIT;
