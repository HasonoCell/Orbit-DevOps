ALTER TABLE releases
    ADD COLUMN rollback_of_release_id uuid,
    ADD CONSTRAINT releases_identity_target_unique UNIQUE (id, deployment_target_id),
    ADD CONSTRAINT releases_rollback_not_self_check
        CHECK (rollback_of_release_id IS NULL OR rollback_of_release_id <> id),
    ADD CONSTRAINT releases_rollback_same_target_fk
        FOREIGN KEY (rollback_of_release_id, deployment_target_id)
        REFERENCES releases (id, deployment_target_id);

CREATE INDEX releases_target_history_idx
    ON releases (deployment_target_id, created_at DESC, id DESC);
