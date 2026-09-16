package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/HasonoCell/Orbit-DevOps/internal/identity"
)

// 这个测试跨真实 PG 投影 Seam，验证安全计数与当前 Strict PHC 验证器的存储契约。
// Hash 只在独立容器内移动，不进入测试输出、文档或文件。
func TestEffectiveUserProjectionRejectsUnsupportedAndNoncanonicalCredentials(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	ctx := context.Background()
	var columns []string
	if err := db.SelectContext(ctx, &columns, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'effective_identity_users' ORDER BY ordinal_position`); err != nil || len(columns) != 1 || columns[0] != "id" {
		t.Fatal("effective identity projection exposed credentials or unexpected authority")
	}
	fixtureSQL(t, db, `CREATE TABLE supported_credential_fixture AS
		SELECT user_id, password_hash FROM local_credentials WHERE user_id = $1`, user.ID)
	assertCount := func(t *testing.T, expected int) {
		t.Helper()
		var count int
		if err := db.GetContext(ctx, &count, `SELECT count(*) FROM effective_identity_users WHERE id = $1`, user.ID); err != nil || count != expected {
			t.Fatal("effective identity count disagreed with the supported credential policy")
		}
	}
	assertCount(t, 1)
	for _, test := range []struct {
		name, expression string
	}{
		{"unsupported memory cost", `replace(password_hash, 'm=19456', 'm=999999999')`},
		{"unsupported iterations", `replace(password_hash, 't=2', 't=99')`},
		{"unsupported parallelism", `replace(password_hash, 'p=1', 'p=99')`},
		{"unsupported algorithm", `replace(password_hash, 'argon2id', 'argon2i')`},
		{"noncanonical key pad bits", `left(password_hash, length(password_hash) - 1) || 'B'`},
		{"noncanonical salt pad bits", `split_part(password_hash, '$', 1) || '$' || split_part(password_hash, '$', 2)
			|| '$' || split_part(password_hash, '$', 3) || '$' || split_part(password_hash, '$', 4)
			|| '$' || left(split_part(password_hash, '$', 5), 21) || 'B$' || split_part(password_hash, '$', 6)`},
		{"extra PHC component", `password_hash || '$extra'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			// SQL 表达式均为源码固定 fixture，不接受运行时输入或拼接任何凭据值。
			fixtureSQL(t, db, `UPDATE local_credentials SET password_hash = (
				SELECT `+test.expression+` FROM supported_credential_fixture) WHERE user_id = $1`, user.ID)
			assertCount(t, 0)
		})
	}
	fixtureSQL(t, db, `UPDATE local_credentials SET password_hash = (
		SELECT password_hash FROM supported_credential_fixture) WHERE user_id = $1`, user.ID)
	assertCount(t, 1)
	fixtureSQL(t, db, `UPDATE local_credentials SET must_change_password = true WHERE user_id = $1`, user.ID)
	assertCount(t, 1)
	fixtureSQL(t, db, `UPDATE users SET status = 'disabled' WHERE id = $1`, user.ID)
	assertCount(t, 0)
	fixtureSQL(t, db, `UPDATE users SET status = 'active' WHERE id = $1`, user.ID)
	fixtureSQL(t, db, `DELETE FROM local_credentials WHERE user_id = $1`, user.ID)
	assertCount(t, 0)
}

func TestPasswordLoginAndEffectivePolicyAgreeOnNoncanonicalPHCWhitespace(t *testing.T) {
	t.Parallel()
	module, db := newIdentity(t)
	user := initializeFixture(t, module)
	ctx := context.Background()
	fixtureSQL(t, db, `UPDATE local_credentials SET password_hash =
		left(password_hash, length(password_hash) - 43) || chr(10) || right(password_hash, 43)
		WHERE user_id = $1`, user.ID)
	var count int
	if err := db.GetContext(ctx, &count, `SELECT count(*) FROM effective_identity_users WHERE id = $1`, user.ID); err != nil || count != 0 {
		t.Fatal("noncanonical credential was included in effective identity policy")
	}
	if _, err := module.LoginLocal(ctx, identity.LocalLoginCommand{
		LoginName: "fixture-admin", Password: fixturePassword, SourceIP: "127.0.0.1",
	}); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("noncanonical PHC was accepted despite not being an effective entry: %v", err)
	}
}
