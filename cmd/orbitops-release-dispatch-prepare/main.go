// orbitops-release-dispatch-prepare 为已停止全部新旧 API/Release Worker 的本地数据库执行加法迁移与离线准备。
// 它不连接 Redis 或 Kubernetes，不关闭用户进程，也不自动确认停机前提。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/HasonoCell/OrbitOps/internal/platform/database"
	"github.com/HasonoCell/OrbitOps/internal/platform/envconfig"
	"github.com/HasonoCell/OrbitOps/internal/releaseoperation"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func main() {
	offline := flag.Bool("confirm-offline", false, "确认全部新旧 API/Worker 进程及外部执行者已停止")
	batch := flag.String("batch", "", "本次切换的 UUID；中断重跑复用，下一次切换使用新 UUID")
	flag.Parse()
	id, err := uuid.Parse(*batch)
	if !*offline || err != nil || id == uuid.Nil || flag.NArg() != 0 {
		fail("需要 --confirm-offline 和有效的 --batch UUID")
	}
	config, err := envconfig.LoadAPI()
	if err != nil {
		fail("离线准备配置无效")
	}
	if err := database.Migrate(config.DatabaseURL); err != nil {
		fail("数据库迁移失败；未执行准备，请检查迁移状态")
	}
	db, err := sqlx.Open("pgx", config.DatabaseURL)
	if err != nil {
		fail("无法打开准备数据库")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := releaseoperation.New(db).PrepareDispatches(ctx, id)
	if err != nil {
		fail("离线准备未完成；保持服务停止，排查后用同一批次重跑")
	}
	fmt.Printf("批次 %s：待调度 %d，重复批次 %t\n", id, result.Scheduled, result.Replayed)
}

// fail 不透传驱动错误，避免连接串或凭据进入命令输出。
func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
