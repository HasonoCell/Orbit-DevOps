BEGIN;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM users)
       OR EXISTS (SELECT 1 FROM identity_control WHERE initialized_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM project_members)
       OR EXISTS (SELECT 1 FROM legacy_project_members WHERE claimed_user_id IS NOT NULL)
       OR EXISTS (SELECT 1 FROM projects WHERE identity_state <> 'legacy') THEN
        RAISE EXCEPTION 'real project identities cannot be rolled back';
    END IF;
END $$;
DROP TABLE project_members;
ALTER TABLE projects DROP COLUMN identity_state;
ALTER TABLE legacy_project_members DROP COLUMN claimed_user_id, DROP COLUMN claimed_by, DROP COLUMN claimed_at;
ALTER TABLE legacy_project_members RENAME TO project_members;
ALTER TABLE project_members RENAME CONSTRAINT legacy_project_members_pkey TO project_members_pkey;
ALTER INDEX legacy_project_members_actor_idx RENAME TO project_members_actor_idx;
COMMIT;
