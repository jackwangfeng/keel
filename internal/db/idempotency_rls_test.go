package db_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/db"
)

// 00028 那次 schema 决定的行为执行者：idempotency_keys 的平台那一抽屉
// （merchant_id 为 NULL）与各家店的存档互相碰不到。
//
// migrate_test.go 那条形状断言逐字比对策略谓词（db/tenancy.json 的 policy_qual），
// 但形状对不代表行为对 —— 理由与 staff_rls_test.go 文件头那段一字不差。
// 所以这里把两种作用域的读写全跑一遍：
//
//	              读得到平台   读得到本店   读得到别店   写得出平台   写得出别店
//	租户作用域         ✗           ✓           ✗            ✗            ✗
//	平台作用域         ✓           ✗           ✗            ✓            ✗
//
// 最要紧的是「租户作用域写得出平台」那一格：策略写成
// `merchant_id = current_merchant() OR merchant_id IS NULL` 时，USING 同时充当
// WITH CHECK，商家员工就能往平台键空间里预埋一份存档 —— 平台管理员下一次拿那把
// 钥匙重试时，会把一份伪造的响应当成重放收下。
//
// 变异验证（本轮实跑）：把 00028 的策略改成 `... OR merchant_id IS NULL`，
// 「读不到平台存档」「写不出平台存档」「改不动、删不掉平台存档」三条红
// （migrate_test.go 的 policy_qual 逐字断言同时红）。
func TestIdempotencyKeysIsolatePlatformFromTenants(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("idemrls-%d", time.Now().UnixNano())
	// status = 2 的理由见 staff_rls_test.go：多一家活跃商家会让别的包的 Preflight 随机失败。
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2) RETURNING id`,
		suffix+"-a").Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'B',2) RETURNING id`,
		suffix+"-b").Scan(&idB); err != nil {
		t.Fatal(err)
	}

	// 三行存档：平台一行、两家店各一行。scope 用本测试独有的串，
	// 于是下面每一次 SELECT 只数得到本夹具的行。主体都是后台操作员（kind = 2），
	// 平台那一抽屉按 CHECK 只允许这一种。
	scope := suffix
	for _, row := range []struct {
		merchant *int64
		subject  int64
		key      string
	}{{nil, 1, "p"}, {&idA, 2, "a"}, {&idB, 3, "b"}} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO idempotency_keys (scope, merchant_id, subject_kind, subject_id,
			                               idem_key, request_hash, status, expire_at)
			 VALUES ($1, $2, 2, $3, $4, 'h', 1, now() + interval '1 day')`,
			scope, row.merchant, row.subject, row.key); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		if _, err := admin.Exec(c, `DELETE FROM idempotency_keys WHERE scope = $1`, scope); err != nil {
			t.Errorf("清理 idempotency_keys 失败: %v", err)
		}
		if _, err := admin.Exec(c, `DELETE FROM merchants WHERE id = ANY($1)`,
			[]int64{idA, idB}); err != nil {
			t.Errorf("清理 merchants 失败: %v", err)
		}
	})

	app, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)

	// inScope 在一个作用域里开事务跑 fn。租户作用域照 repository.enterTenantScope
	// 的样子**显式关掉** app.platform_scope —— 生产路径就是这么设的。
	inScope := func(t *testing.T, platform bool, merchantID int64, fn func(pgx.Tx) error) error {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if platform {
			_, err = tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`)
		} else {
			_, err = tx.Exec(ctx,
				`SELECT set_config('app.merchant_id',$1,true), set_config('app.platform_scope','off',true)`,
				fmt.Sprint(merchantID))
		}
		if err != nil {
			t.Fatal(err)
		}
		return fn(tx)
	}
	keysIn := func(t *testing.T, platform bool, merchantID int64) []string {
		t.Helper()
		var got []string
		if err := inScope(t, platform, merchantID, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx,
				`SELECT idem_key FROM idempotency_keys WHERE scope = $1 ORDER BY idem_key`, scope)
			if err != nil {
				return err
			}
			got, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	rlsViolation := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s：居然写进去了", what)
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s：被拒了，但拒的不是 RLS（%v）", what, err)
		}
	}
	insert := func(merchant any, key string) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO idempotency_keys (scope, merchant_id, subject_kind, subject_id,
				                               idem_key, request_hash, expire_at)
				 VALUES ($1, $2, 2, 99, $3, 'h', now() + interval '1 day')`, scope, merchant, key)
			return err
		}
	}

	t.Run("阳性对照：夹具三行都在", func(t *testing.T) {
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM idempotency_keys WHERE scope = $1`, scope).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("管理员连接数到 %d 行，期望 3 —— 夹具没建成，下面的断言都在空转", n)
		}
	})

	t.Run("商家员工读不到平台存档，也读不到别店的", func(t *testing.T) {
		if got := keysIn(t, false, idA); len(got) != 1 || got[0] != "a" {
			t.Errorf("A 店看到 %v，期望只有 [a] —— 看到 p 说明策略被放宽成了 "+
				"`OR merchant_id IS NULL`，看到 b 说明租户比较整个没生效", got)
		}
	})

	t.Run("平台会话只读得到平台存档，读不到任何一家店的", func(t *testing.T) {
		if got := keysIn(t, true, 0); len(got) != 1 || got[0] != "p" {
			t.Errorf("平台作用域看到 %v，期望只有 [p] —— 看到 a / b 就是「平台误用某家店的存档」", got)
		}
	})

	t.Run("商家员工写不出平台存档", func(t *testing.T) {
		rlsViolation(t, inScope(t, false, idA, insert(nil, "evil-null")),
			"A 店的员工往平台键空间里预埋一份存档")
	})

	t.Run("商家员工写不到别店名下", func(t *testing.T) {
		rlsViolation(t, inScope(t, false, idA, insert(idB, "evil-b")),
			"A 店的员工往 B 店写存档")
	})

	t.Run("平台会话写不到任何一家店名下", func(t *testing.T) {
		rlsViolation(t, inScope(t, true, 0, insert(idA, "evil-a")),
			"平台作用域往 A 店写存档")
	})

	t.Run("商家员工改不动、删不掉平台存档", func(t *testing.T) {
		// UPDATE / DELETE 走 USING：看不见就碰不到，0 行而不是报错。
		for _, sql := range []string{
			`UPDATE idempotency_keys SET response_body = '{"forged":true}' WHERE scope = $1 AND idem_key = 'p'`,
			`DELETE FROM idempotency_keys WHERE scope = $1 AND idem_key = 'p'`,
		} {
			if err := inScope(t, false, idA, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, sql, scope)
				if err != nil {
					return err
				}
				if tag.RowsAffected() != 0 {
					return fmt.Errorf("%q 碰到了 %d 行平台存档", sql, tag.RowsAffected())
				}
				return nil
			}); err != nil {
				t.Error(err)
			}
		}
	})

	t.Run("省略 merchant_id 时它由作用域决定", func(t *testing.T) {
		// 抢占插入那条 SQL 里没有 merchant_id（scripts/check_query_tenancy.py），
		// 所以这一列只能来自 DEFAULT staff_scope_merchant()。
		for _, tc := range []struct {
			name     string
			platform bool
			merchant int64
			want     *int64
		}{
			{"租户作用域落在本店", false, idA, &idA},
			{"平台作用域落成 NULL", true, 0, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var got *int64
				if err := inScope(t, tc.platform, tc.merchant, func(tx pgx.Tx) error {
					return tx.QueryRow(ctx,
						`INSERT INTO idempotency_keys (scope, subject_kind, subject_id, idem_key,
						                               request_hash, expire_at)
						 VALUES ($1, 2, 42, $2, 'h', now() + interval '1 day')
						 RETURNING merchant_id`, scope, "default-"+tc.name).Scan(&got)
				}); err != nil {
					t.Fatal(err)
				}
				switch {
				case tc.want == nil && got != nil:
					t.Errorf("平台作用域的存档落在了 merchant_id=%d", *got)
				case tc.want != nil && (got == nil || *got != *tc.want):
					t.Errorf("租户作用域的存档落在了 %v，期望 %d", got, *tc.want)
				}
			})
		}
	})

	t.Run("平台那一抽屉里不能有买家", func(t *testing.T) {
		err := inScope(t, true, 0, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO idempotency_keys (scope, subject_kind, subject_id, idem_key,
				                               request_hash, expire_at)
				 VALUES ($1, 1, 42, 'buyer-in-platform', 'h', now() + interval '1 day')`, scope)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("平台作用域写一行买家存档得到 %v，期望 23514（chk_idem_platform_is_staff）", err)
		}
	})
}
