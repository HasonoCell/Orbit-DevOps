BEGIN;

-- 即使投影可重建，有真实账号时也不能先删除保护事实再在更早的 down 失败。
-- 空身份 fixture 可回退；真实库应按受控升级/恢复流程处理，不能 Force 绕过。
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM users)
       OR EXISTS (SELECT 1 FROM identity_control WHERE initialized_at IS NOT NULL) THEN
        RAISE EXCEPTION 'effective identity policy cannot roll back initialized accounts';
    END IF;
END;
$$;

DROP VIEW effective_identity_users;
COMMIT;
