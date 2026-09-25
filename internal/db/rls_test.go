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

// 查系统表只能证明「RLS 装上了」，证明不了「RLS 拦得住」。这两件事之间隔着一个
// 连接用什么角色的问题：超级用户无条件绕过 RLS，于是系统表全绿、库照漏。
//
// 所以这里不查 pg_class，直接用应用角色把两个租户的数据读一遍，断言看到的行数。
// 它同时钉住设计文档那条「未设租户上下文时报错而非放行」——在超级用户连接上
// 这条是不成立的，下一个人照文档写测试会撞红，然后最省事的做法就是把它删掉。
func TestRLSIsolatesTenants(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 播种走管理员连接：它绕过 RLS，可以一次把两个租户的数据都写进去。
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	// 用 t.Cleanup 而不是 defer 关连接：t.Cleanup 里还要用这条连接删数据，
	// 而 defer 在测试函数返回时就跑，早于所有 t.Cleanup。写成 defer 的话删除
	// 会在一条已关闭的连接上静默失败，把测试数据留在库里。
	// Cleanup 是后进先出，所以这条先注册、最后执行。
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("rlstest-%d", time.Now().UnixNano())
	codeA, codeB := suffix+"-a", suffix+"-b"

	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name) VALUES ($1, 'A'), ($2, 'B') RETURNING id`,
		codeA, codeB).Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, codeB).Scan(&idB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{idA, idB}
		// 删不掉要出声：悄悄失败的清理会把测试数据一直攒在开发库里。
		if _, err := admin.Exec(c, `DELETE FROM categories WHERE merchant_id = ANY($1)`, ids); err != nil {
			t.Errorf("清理 categories 失败: %v", err)
		}
		if _, err := admin.Exec(c, `DELETE FROM merchants WHERE id = ANY($1)`, ids); err != nil {
			t.Errorf("清理 merchants 失败: %v", err)
		}
	})

	if _, err := admin.Exec(ctx,
		`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'cat-A','/a/'), ($2,'cat-B','/b/')`,
		idA, idB); err != nil {
		t.Fatal(err)
	}

	// 应用连接：Connect 已经确认过它绕不过 RLS。
	app, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)

	t.Run("设了租户只看得到自己的", func(t *testing.T) {
		for _, tc := range []struct {
			tenant int64
			want   string
		}{{idA, "cat-A"}, {idB, "cat-B"}} {
			// SET 不吃绑定参数，用 set_config；第三参数 false = 会话级而非事务级。
			if _, err := app.Exec(ctx,
				`SELECT set_config('app.merchant_id', $1, false)`,
				fmt.Sprint(tc.tenant)); err != nil {
				t.Fatal(err)
			}
			rows, err := app.Query(ctx,
				`SELECT name FROM categories WHERE merchant_id = ANY($1) ORDER BY name`,
				[]int64{idA, idB})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					t.Fatal(err)
				}
				names = append(names, n)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(names) != 1 || names[0] != tc.want {
				t.Fatalf("租户 %d 查到 %v，期望只有 [%s]——RLS 没拦住", tc.tenant, names, tc.want)
			}
		}
	})

	t.Run("没设租户要报错而不是放行", func(t *testing.T) {
		if _, err := app.Exec(ctx, `RESET app.merchant_id`); err != nil {
			t.Fatal(err)
		}
		var n int
		err := app.QueryRow(ctx, `SELECT count(*) FROM categories`).Scan(&n)
		if err == nil {
			t.Fatalf("没设 app.merchant_id 却查到了 %d 行——应该报错而不是放行", n)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("期望 insufficient_privilege(42501)，实际: %v", err)
		}
	})
}

// 应用角色必须绕不过 RLS。这条断言独立于上面的行为测试：行为测试在角色被悄悄
// 提权成超级用户之后会全绿（因为它读的是自己那一份数据，超级用户也读得到），
// 只有直接查角色属性才抓得住提权。
func TestAppRoleCannotBypassRLS(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	conn, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	var who string
	var super, bypass bool
	if err := conn.QueryRow(ctx,
		`SELECT current_user, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&who, &super, &bypass); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("应用角色 %q: rolsuper=%v rolbypassrls=%v，两者都必须为 false", who, super, bypass)
	}
	t.Logf("应用角色 %q: rolsuper=%v rolbypassrls=%v", who, super, bypass)
}
