DROP INDEX releases_target_history_idx;

ALTER TABLE releases
    DROP CONSTRAINT releases_rollback_same_target_fk,
    DROP CONSTRAINT releases_rollback_not_self_check,
    DROP CONSTRAINT releases_identity_target_unique,
    DROP COLUMN rollback_of_release_id;
