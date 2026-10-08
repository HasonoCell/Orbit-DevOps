// orbit-devops-migrate 是独立 forward-only 部署入口；不加载集群、队列或浏览器配置。
package main

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/HasonoCell/Orbit-DevOps/internal/platform/database"
	migratedb "github.com/golang-migrate/migrate/v4/database"
	"github.com/jackc/pgx/v5/pgconn"
)

func main() {
	url := os.Getenv("ORBIT_DEVOPS_DATABASE_URL")
	if url == "" || len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "需要 ORBIT_DEVOPS_DATABASE_URL，且不接受迁移方向或强制参数")
		os.Exit(1)
	}
	if err := database.Migrate(url); err != nil {
		// 驱动错误可能包含连接信息；故障不自动 force 或 down，部署者通过受控入口排查。
		fmt.Fprintf(os.Stderr, "数据库前向迁移失败（%s）；保持服务停止并检查迁移状态\n", failureCategory(err))
		os.Exit(1)
	}
	fmt.Println("数据库前向迁移完成")
}

// failureCategory 只暴露稳定类别或 SQLSTATE，不打印可能包含 DSN、密码和查询参数的驱动消息。
func failureCategory(err error) string {
	for err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) {
			return "sqlstate=" + pgError.Code
		}
		var networkError net.Error
		if errors.As(err, &networkError) {
			return "network_unavailable"
		}
		var migrationError *migratedb.Error
		if errors.As(err, &migrationError) {
			err = migrationError.OrigErr
			continue
		}
		err = errors.Unwrap(err)
	}
	return "migration_failed"
}
