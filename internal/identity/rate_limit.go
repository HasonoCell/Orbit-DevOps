package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
)

// limitLocalLogin 使用短独立事务，只记录尝试，不持安全锁或密码哈希槽。
// 被拒绝的尝试也提交计数；多实例共享 PG 原子 UPSERT，不能回滚计数绕过限流。
func (m *Module) limitLocalLogin(ctx context.Context, loginName, sourceIP string) error {
	rules := []struct {
		key     [32]byte
		seconds int
		maximum int
	}{
		{sha256.Sum256([]byte("local-login-account:" + loginName)), 600, 10},
		{sha256.Sum256([]byte("local-login-source:" + sourceIP)), 60, 30},
	}
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return dependencyError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('lock_timeout', '5s', true)`); err != nil {
		return dependencyError(err)
	}
	limited := false
	// 所有请求固定先账号、后来源，不与身份安全事务共持这些计数锁。
	for _, rule := range rules {
		var count int
		if err := tx.GetContext(ctx, &count, `INSERT INTO auth_rate_limits
			(subject_key, window_started_at, attempt_count, expires_at)
			VALUES ($1, statement_timestamp(), 1, statement_timestamp() + make_interval(secs => $2))
			ON CONFLICT (subject_key) DO UPDATE SET
			window_started_at = CASE WHEN auth_rate_limits.expires_at <= statement_timestamp()
			 THEN statement_timestamp() ELSE auth_rate_limits.window_started_at END,
			attempt_count = CASE WHEN auth_rate_limits.expires_at <= statement_timestamp() THEN 1
			 ELSE LEAST(auth_rate_limits.attempt_count::bigint + 1, 2147483647)::integer END,
			expires_at = CASE WHEN auth_rate_limits.expires_at <= statement_timestamp()
			 THEN statement_timestamp() + make_interval(secs => $2) ELSE auth_rate_limits.expires_at END
			RETURNING attempt_count`, rule.key[:], rule.seconds); err != nil {
			return dependencyError(err)
		}
		limited = limited || count > rule.maximum
	}
	if err := tx.Commit(); err != nil {
		return dependencyError(err)
	}
	// 清理是提交后的有界维护，不占用其它请求的计数锁；失败不放宽已提交限制。
	_, _ = m.db.ExecContext(ctx, `WITH expired AS (
		SELECT subject_key FROM auth_rate_limits WHERE expires_at <= clock_timestamp()
		ORDER BY subject_key LIMIT 100 FOR UPDATE SKIP LOCKED
	) DELETE FROM auth_rate_limits r USING expired e WHERE r.subject_key = e.subject_key`)
	if limited {
		return ErrRateLimited
	}
	return ctx.Err()
}
