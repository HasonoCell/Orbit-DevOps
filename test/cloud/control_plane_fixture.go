// 本地控制面演练专用；非交互初始化只访问测试数据库的公开身份模块。
// 不进入生产镜像。无参数时故意失败，用来验证坏 API 镜像的独立回退。
package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
	"github.com/HasonoCell/Orbit-DevOps/internal/projectauth"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "initialize-admin" {
		os.Exit(1)
	}
	if err := initialize(); err != nil {
		// 不回显数据库连接配置或密码。
		_, _ = os.Stderr.WriteString("LOCAL_IDENTITY_FIXTURE_FAILED\n")
		os.Exit(1)
	}
}

func initialize() error {
	password, err := io.ReadAll(io.LimitReader(os.Stdin, 513))
	if err != nil {
		return err
	}
	defer clear(password)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sqlx.ConnectContext(ctx, "pgx", os.Getenv("ORBIT_DEVOPS_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	module, err := identity.New(db, projectauth.NewOwnershipGuard())
	if err != nil {
		return err
	}
	_, err = module.InitializeAdmin(ctx, identity.InitializeAdminCommand{
		LoginName: "local-rehearsal", DisplayName: "Local Rehearsal",
		Password: string(password), MaintenanceRef: "local-control-plane-rehearsal",
	})
	return err
}
