package pgerrors

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUniqueConstraint 仅识别指定的 PostgreSQL 唯一约束，避免将其他写入失败误判为业务冲突。
func IsUniqueConstraint(err error, constraint string) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "23505" && databaseError.ConstraintName == constraint
}
