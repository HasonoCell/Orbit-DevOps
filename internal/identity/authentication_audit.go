package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/audit"
	"github.com/google/uuid"
)

// rejectLocalLogin 不相信被声明的 User；只记录系统来源和有限失败类别。
// 审计是独立事实而非业务副作用，事务失败时返回依赖不可用，不伪称已记录。
func (m *Module) rejectLocalLogin(ctx context.Context, reason error) error {
	return m.rejectPasswordAuthentication(ctx, "auth.login_failed", reason)
}

// action 只来自模块内固定命令类别，不接受请求提供的任意审计内容。
func (m *Module) rejectPasswordAuthentication(ctx context.Context, action string, reason error) error {
	code := "invalid_credentials"
	if errors.Is(reason, ErrRateLimited) {
		code = "rate_limited"
	}
	tx, err := m.db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return dependencyError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var now time.Time
	if err := tx.GetContext(ctx, &now, `SELECT clock_timestamp()`); err != nil {
		return dependencyError(err)
	}
	if err := audit.Append(ctx, tx, audit.Entry{
		ActorID: "orbit-devops-authentication", ActorKind: audit.ActorKindSystem,
		Action: action, TargetType: "authentication_attempt", TargetID: uuid.New(),
		Summary: map[string]any{"reason": code, "method": "password"}, CreatedAt: now,
	}); err != nil {
		return dependencyError(err)
	}
	if err := tx.Commit(); err != nil {
		return dependencyError(err)
	}
	return reason
}
