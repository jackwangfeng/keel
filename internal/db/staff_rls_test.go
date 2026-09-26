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

// staff 的隔离必须用**行为**验，不能只查系统目录。
//
// 理由比别的表更硬：这张表的策略不是 §2 那条标准的列比较，而是
// `merchant_id IS NOT DISTINCT FROM staff_scope_merchant()`（00017）。
// migrate_test.go 那条形状断言逐字比对这个谓词，但形状对不代表行为对 ——
// 一个把 staff_scope_merchant() 改成 `RETURN NULL` 的实现，形状断言照样绿
// （谓词的文本一个字没变），而它的行为是「谁也看不见自己的员工，所有人都
// 看得见平台级操作员」。
//
// 所以这里把四个象限全跑一遍，读写各一次：
//
//	              看得见平台级   看得见本店   看得见别店   插得出平台级   插得出别店
//	租户作用域         ✗           ✓           ✗            ✗             ✗
//	平台作用域         ✓           ✗           ✗            ✓             ✗
//
// 「插得出平台级」那一格是本轮最要紧的一条：把策略写成
// `merchant_id = current_merchant() OR merchant_id IS NULL` 时，上面读侧的
// 三格全都还是对的（商家看不见平台级？不对 —— 那种写法下他看得见），
// 而写侧那一格会从 ✗ 变成 ✓ —— **任何商家管理员都能给自己造一个平台管理员**。
// 一条没有报错、没有日志、读侧几乎看不出来的提权路径。
func TestStaffScopeIsolatesPlatformFromTenants(t *testing.T) {
	ctx := context.Background()
	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("staffrls-%d", time.Now().UnixNano())
	// status = 2（停用）的理由与 rls_test.go 里那一段一字不差：四个包共用一个库，
	// 这里多一家活跃商家会在 tenant 包里表现成一次随机的 Preflight 失败。
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2) RETURNING id`,
		suffix+"-a", suffix+"-b").Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, suffix+"-b").Scan(&idB); err != nil {
		t.Fatal(err)
	}

	// 三行 staff：平台级一行，两家店各一行。播种走管理员连接（绕过 RLS）。
	emailP, emailA, emailB := suffix+"-p@x", suffix+"-a@x", suffix+"-b@x"
	var staffP, staffA int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO staff (merchant_id, email, role) VALUES (NULL, $1, 1) RETURNING id`,
		emailP).Scan(&staffP); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO staff (merchant_id, email, role) VALUES ($1, $2, 1) RETURNING id`,
		idA, emailA).Scan(&staffA); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO staff (merchant_id, email, role) VALUES ($1, $2, 1)`,
		idB, emailB); err != nil {
		t.Fatal(err)
	}
	// 两行 token，各挂一边，用来验 staff_tokens 的 parent-scope 策略。
	if _, err := admin.Exec(ctx,
		`INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
		 VALUES ($1, $2, 3, now() + interval '1 day'),
		        ($3, $4, 3, now() + interval '1 day')`,
		staffP, suffix+"-hp", staffA, suffix+"-ha"); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c := context.Background()
		if _, err := admin.Exec(c, `DELETE FROM staff WHERE email LIKE $1`, suffix+"%"); err != nil {
			t.Errorf("清理 staff 失败: %v", err)
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

	// emailsIn 在一个作用域里跑一次 SELECT，返回本夹具那三行里看得见的邮箱。
	//
	// 作用域用事务里的 set_config(..., true) 设 —— 与 repository.WithTenant /
	// WithPlatform 走的是同一个机制。fn 里返回的错误原样带出来。
	emailsIn := func(t *testing.T, platform bool, merchantID int64) []string {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if platform {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`); err != nil {
				t.Fatal(err)
			}
		} else if _, err := tx.Exec(ctx,
			`SELECT set_config('app.merchant_id',$1,true)`, fmt.Sprint(merchantID)); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query(ctx,
			`SELECT email FROM staff WHERE email = ANY($1) ORDER BY email`,
			[]string{emailP, emailA, emailB})
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var e string
			if err := rows.Scan(&e); err != nil {
				t.Fatal(err)
			}
			got = append(got, e)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("阳性对照：夹具里三行都真的写进去了", func(t *testing.T) {
		// 没有这一段，下面每一条「只看得见一行」在夹具根本没插进去时同样是绿的。
		var n int
		if err := admin.QueryRow(ctx,
			`SELECT count(*) FROM staff WHERE email = ANY($1)`,
			[]string{emailP, emailA, emailB}).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("管理员连接只数到 %d 行 staff，期望 3 —— 夹具没建成，下面的断言都在空转", n)
		}
	})

	t.Run("租户作用域只看得见本店的，平台级那行也看不见", func(t *testing.T) {
		if got := emailsIn(t, false, idA); len(got) != 1 || got[0] != emailA {
			t.Errorf("A 店看到 %v，期望只有 [%s]——"+
				"看到平台级那一行说明策略被放宽成了 `OR merchant_id IS NULL`，"+
				"看到 B 店那一行说明租户比较整个没生效", got, emailA)
		}
		if got := emailsIn(t, false, idB); len(got) != 1 || got[0] != emailB {
			t.Errorf("B 店看到 %v，期望只有 [%s]", got, emailB)
		}
	})

	t.Run("平台作用域只看得见平台级那行，两家店的都看不见", func(t *testing.T) {
		got := emailsIn(t, true, 0)
		if len(got) != 1 || got[0] != emailP {
			t.Errorf("平台作用域看到 %v，期望只有 [%s]——"+
				"看到商家的行说明平台作用域退化成了 BYPASSRLS，那比它该有的权限宽得多", got, emailP)
		}
	})

	// 写侧四条。它们是这张表最要紧的断言：读侧被放宽还看得出来，
	// 写侧被放宽（USING 同时充当 WITH CHECK）就是一条提权路径。
	insertAs := func(t *testing.T, platform bool, merchantID int64, sql string, args ...any) error {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if platform {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`); err != nil {
				t.Fatal(err)
			}
		} else if _, err := tx.Exec(ctx,
			`SELECT set_config('app.merchant_id',$1,true)`, fmt.Sprint(merchantID)); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql, args...)
		return err
	}

	// rlsViolation 确认这条错误是**策略**拒的（42501），不是别的什么拒的。
	//
	// 这一步不能省：唯一约束、外键、NOT NULL 都会让 INSERT 失败，而
	// 「因为别的原因失败了」与「策略拦住了」在 err != nil 这个判据下同形。
	// 本仓库抓到过同一类空转（permission denied 与 RLS 拒绝共用 42501 那一条）。
	rlsViolation := func(t *testing.T, err error, what string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s：居然写进去了", what)
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s：被拒了，但拒的不是 RLS（%v）——"+
				"期望 42501 new row violates row-level security policy", what, err)
		}
	}

	t.Run("商家作用域插不出平台级 staff", func(t *testing.T) {
		err := insertAs(t, false, idA,
			`INSERT INTO staff (merchant_id, email, role) VALUES (NULL, $1, 1)`, suffix+"-evil@x")
		rlsViolation(t, err, "A 店的管理员给自己造一个平台管理员")
	})

	t.Run("商家作用域插不到别家名下", func(t *testing.T) {
		err := insertAs(t, false, idA,
			`INSERT INTO staff (merchant_id, email, role) VALUES ($1, $2, 1)`, idB, suffix+"-evil2@x")
		rlsViolation(t, err, "A 店的管理员往 B 店塞一个管理员")
	})

	t.Run("平台作用域插不到任何一家店名下", func(t *testing.T) {
		err := insertAs(t, true, 0,
			`INSERT INTO staff (merchant_id, email, role) VALUES ($1, $2, 1)`, idA, suffix+"-evil3@x")
		rlsViolation(t, err, "平台作用域往 A 店塞一个管理员")
	})

	t.Run("省略 merchant_id 时它由作用域决定，不由调用方决定", func(t *testing.T) {
		// DEFAULT staff_scope_merchant() 是「不接受前端传入」那条纪律的落点
		// （数据模型 §14 认证流程 ④）。这里验它真的落在两种作用域各自的值上。
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
				tx, err := app.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				if tc.platform {
					_, err = tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`)
				} else {
					_, err = tx.Exec(ctx,
						`SELECT set_config('app.merchant_id',$1,true)`, fmt.Sprint(tc.merchant))
				}
				if err != nil {
					t.Fatal(err)
				}
				var got *int64
				if err := tx.QueryRow(ctx,
					`INSERT INTO staff (email, role) VALUES ($1, 2) RETURNING merchant_id`,
					fmt.Sprintf("%s-def-%v@x", suffix, tc.platform)).Scan(&got); err != nil {
					t.Fatalf("插入失败: %v", err)
				}
				switch {
				case tc.want == nil && got != nil:
					t.Errorf("平台作用域插出来的 merchant_id 是 %d，期望 NULL", *got)
				case tc.want != nil && (got == nil || *got != *tc.want):
					t.Errorf("租户作用域插出来的 merchant_id 是 %v，期望 %d", got, *tc.want)
				}
			})
		}
	})

	// 这一组的因果关系做过变异验证，结论要写下来，因为它反直觉：
	//
	//   · 只把 staff_tokens 的策略里那句 staff_scope_merchant() 比较去掉
	//     （谓词剩 `EXISTS (SELECT 1 FROM staff s WHERE s.id = ...)`）→ **仍然绿**。
	//     原因：PostgreSQL 对策略表达式里引用到的表**同样施加 RLS**，
	//     于是那个子查询里的 staff 已经被 staff 自己的策略过滤过了。
	//   · 只把 staff 的策略放成 USING (true)、staff_tokens 那句留着 →
	//     下面这一组**仍然绿**（上面那几组 staff 的断言则全红）。
	//   · 两条一起放开 → 下面这一组才红。
	//
	// 也就是说两条策略在这件事上**各自都够**，互为冗余。这里如实记下来，
	// 免得下一个人以为下面这几行断言与 staff_tokens 的谓词一一对应 ——
	// 本仓库反复抓到的正是这种「断言存在但因果关系在别处」。
	// 两条都保留是刻意的：staff 的策略哪天被人动了（那是一次显式改动，
	// 上面那几组会红），staff_tokens 这一条仍然独立成立。
	t.Run("staff_tokens 跟着父表走", func(t *testing.T) {
		hashesIn := func(platform bool, merchantID int64) []string {
			tx, err := app.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if platform {
				_, err = tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`)
			} else {
				_, err = tx.Exec(ctx,
					`SELECT set_config('app.merchant_id',$1,true)`, fmt.Sprint(merchantID))
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := tx.Query(ctx,
				`SELECT token_hash FROM staff_tokens WHERE token_hash = ANY($1) ORDER BY token_hash`,
				[]string{suffix + "-hp", suffix + "-ha"})
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err != nil {
					t.Fatal(err)
				}
				got = append(got, h)
			}
			return got
		}
		if got := hashesIn(false, idA); len(got) != 1 || got[0] != suffix+"-ha" {
			t.Errorf("A 店看到 token %v，期望只有自己那一条——"+
				"看到平台那条意味着一串平台级会话 token 能在商家的租户上下文里被查出来", got)
		}
		if got := hashesIn(true, 0); len(got) != 1 || got[0] != suffix+"-hp" {
			t.Errorf("平台作用域看到 token %v，期望只有平台那一条", got)
		}
		if got := hashesIn(false, idB); len(got) != 0 {
			t.Errorf("B 店看到 token %v，期望一条都没有", got)
		}
	})

	t.Run("平台作用域碰别的租户表要报错而不是返回空集", func(t *testing.T) {
		// 平台作用域刻意不设 app.merchant_id（00017 文件头）。于是平台路径
		// 误读业务表时是一条说人话的错，而不是一个看上去很像「这家店没有商品」
		// 的空结果集。失败方向是关闭的，且看得见。
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_scope','on',true)`); err != nil {
			t.Fatal(err)
		}
		var n int
		err = tx.QueryRow(ctx, `SELECT count(*) FROM products`).Scan(&n)
		if err == nil {
			t.Fatalf("平台作用域读到了 %d 行 products —— 它不该有任何一家店的数据", n)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("期望 insufficient_privilege(42501)，实际: %v", err)
		}
	})
}
