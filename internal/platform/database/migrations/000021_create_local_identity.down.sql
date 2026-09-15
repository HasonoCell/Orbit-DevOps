BEGIN;

-- down 仅用于空身份 fixture，不能通过删账号回退到共享 Actor。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users)
       OR EXISTS (SELECT 1 FROM identity_control WHERE initialized_at IS NOT NULL) THEN
        RAISE EXCEPTION 'cannot roll back initialized identity data';
    END IF;
END;
$$;

DROP TABLE auth_rate_limits;
DROP TABLE auth_sessions;
DROP TABLE local_credentials;
DROP TABLE users;
DROP TABLE identity_control;
DROP FUNCTION preserve_identity_initialization();

COMMIT;
