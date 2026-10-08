package database

import (
	"context"
	"errors"
	"net"
	"time"
)

// pingUntilReady 吸收新 Pod 网络规则生效前的短暂拒绝；不重试认证/SQL 错误，也不重放迁移。
// 调用者必须提供有限 deadline；连接和等待共同消耗同一个启动预算。
func pingUntilReady(ctx context.Context, ping func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := ping(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var networkError net.Error
		if !errors.As(err, &networkError) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
