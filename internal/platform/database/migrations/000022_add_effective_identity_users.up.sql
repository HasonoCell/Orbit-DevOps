-- 有效登录入口是当前数据库政策事实，不是在线 Session 数或外部提供方的可用性。
-- 只投影 UUID，不将凭据交给 projectauth；OIDC 实现切片再扩展真实的允许入口。
-- 结构必须与当前有界 Argon2id 验证器一致，损坏/不支持的 PHC 不能保住最后管理员。
CREATE VIEW effective_identity_users AS
SELECT u.id
FROM users u
JOIN local_credentials lc ON lc.user_id = u.id
WHERE u.status = 'active'
  AND lc.password_hash ~ '^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{21}[AQgw]\$[A-Za-z0-9+/]{42}[AEIMQUYcgkosw048]$';
