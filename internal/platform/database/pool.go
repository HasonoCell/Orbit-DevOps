package database

import (
	"context"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

var ErrUnavailable = errors.New("database_unavailable")

// PoolConfig 是单进程连接预算；消费者并发与其他进程的连接数仍需单独计入。
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
	PingTimeout     time.Duration
}

func DefaultPool(maxOpen int) PoolConfig {
	return PoolConfig{MaxOpenConns: maxOpen, MaxIdleConns: 4, ConnMaxIdleTime: 5 * time.Minute,
		ConnMaxLifetime: 30 * time.Minute, PingTimeout: 3 * time.Second}
}

func (c PoolConfig) Validate() error {
	if c.MaxOpenConns <= 0 || c.MaxIdleConns < 0 || c.MaxIdleConns > c.MaxOpenConns ||
		c.ConnMaxIdleTime <= 0 || c.ConnMaxLifetime <= 0 || c.PingTimeout <= 0 {
		return errors.New("invalid database pool configuration")
	}
	return nil
}

// Open 在返回前配置有限连接池并用独立预算检测连接；失败时关闭池，不泄漏原始 DSN 错误。
// 返回的池由装配入口拥有，必须在所有业务处理器与心跳结束后关闭。
func Open(ctx context.Context, databaseURL string, config PoolConfig) (*sqlx.DB, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	db, err := sqlx.Open("pgx", databaseURL)
	if err != nil {
		return nil, ErrUnavailable
	}
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxIdleTime(config.ConnMaxIdleTime)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)
	check, cancel := context.WithTimeout(ctx, config.PingTimeout)
	defer cancel()
	if err := db.PingContext(check); err != nil {
		_ = db.Close()
		return nil, errors.Join(ErrUnavailable, check.Err())
	}
	return db, nil
}
