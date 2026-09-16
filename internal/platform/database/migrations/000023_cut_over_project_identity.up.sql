BEGIN;

-- 历史成员只是待认领责任，不自动生成 User、不参与新授权，原值/时间完整保留。
ALTER TABLE project_members RENAME TO legacy_project_members;
ALTER TABLE legacy_project_members RENAME CONSTRAINT project_members_pkey TO legacy_project_members_pkey;
ALTER INDEX project_members_actor_idx RENAME TO legacy_project_members_actor_idx;
ALTER TABLE legacy_project_members ADD COLUMN claimed_user_id uuid REFERENCES users(id),
    ADD COLUMN claimed_by text, ADD COLUMN claimed_at timestamptz,
    ADD CHECK ((claimed_user_id IS NULL AND claimed_by IS NULL AND claimed_at IS NULL)
        OR (claimed_user_id IS NOT NULL AND claimed_by IS NOT NULL AND claimed_at IS NOT NULL));

-- legacy 尚未认领；frozen 是显式应急缺失状态，不能靠普通启用账号自动解除。
ALTER TABLE projects ADD COLUMN identity_state text NOT NULL DEFAULT 'legacy'
    CHECK (identity_state IN ('legacy', 'governed', 'frozen'));

CREATE TABLE project_members (
    project_id uuid NOT NULL REFERENCES projects(id),
    user_id uuid NOT NULL REFERENCES users(id),
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    created_by text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX project_members_user_idx ON project_members(user_id, project_id);
COMMIT;
