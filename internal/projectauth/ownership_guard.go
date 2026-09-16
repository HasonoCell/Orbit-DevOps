package projectauth

import (
	"context"
	"errors"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// OwnershipGuard 是身份管理消费的窄实现，不依赖 identity.Module，因此可以先构造。
// 它只做 owner 不变量检查；不能用它授予项目访问或将历史 Actor 自动认领为 User。
type OwnershipGuard struct{}

func NewOwnershipGuard() *OwnershipGuard { return &OwnershipGuard{} }

var _ identity.OwnershipGuard = (*OwnershipGuard)(nil)

// RequireRemainingOwner 必须在 identity_control 排他锁下调用，控制锁保护所有安全变更。
// 真实成员的 User 外键才参与治理；legacy/frozen 不变成全局零 owner 前置条件。
func (g *OwnershipGuard) RequireRemainingOwner(ctx context.Context, tx *sqlx.Tx, excluded uuid.UUID) error {
	if g == nil || tx == nil || excluded == uuid.Nil {
		return identity.ErrUnavailable
	}
	// 仅锁本次实际失去有效 owner 的项目；不把其他未认领/应急零 owner 当作全局前置条件。
	var projects []uuid.UUID
	if err := tx.SelectContext(ctx, &projects, `SELECT p.id FROM projects p
		WHERE p.identity_state='governed' AND EXISTS (SELECT 1 FROM project_members pm JOIN effective_identity_users e
			ON pm.user_id = e.id
			WHERE pm.project_id = p.id AND pm.role = 'owner' AND e.id = $1)
		ORDER BY p.id FOR UPDATE OF p`, excluded); err != nil {
		return ownershipDependencyError(err)
	}
	for _, projectID := range projects {
		var hasOther bool
		if err := tx.GetContext(ctx, &hasOther, `SELECT EXISTS (
			SELECT 1 FROM project_members pm JOIN effective_identity_users e ON pm.user_id = e.id
			WHERE pm.project_id = $1 AND pm.role = 'owner' AND e.id <> $2)`, projectID, excluded); err != nil {
			return ownershipDependencyError(err)
		}
		if !hasOther {
			return identity.ErrLastProjectOwner
		}
	}
	return nil
}

func ownershipDependencyError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return identity.ErrUnavailable
}
