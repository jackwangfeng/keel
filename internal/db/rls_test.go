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
	// status = 2（停用）不是随手写的：这两家是为了测隔离而存在的夹具，不是
	// 对外营业的店铺，而 tenant 包的 Preflight 断言的是**整个库**的形态
	// （「配了默认商家时活跃商家只能有一家」）。这段写下时各包共用同一个库，
	// 这里插一家活跃商家会在另一个包里表现为一次随机的断言失败，凶手名字
	// 还出现在别人的错误信息里。现在各包各有自己的库（internal/testdb），
	// 跨包这条路断了；但同一个包里的测试仍共用一个库，所以照旧停用。
	// 隔离测试本身不关心 status —— RLS 的谓词只看 merchant_id。
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1, 'A', 2), ($2, 'B', 2) RETURNING id`,
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

// inventories 的策略本轮从 EXISTS 子查询变成了直接的列比较（迁移 00020）：
// 主键从 sku_id 变成 (sku_id, store_id) 之后「租户归属由主键唯一决定」不再成立
// （有两个父表，两条归属链可以对不上），于是这张表补上了 merchant_id，
// 从 parent-scoped 变成 tenant 类。
//
// **这条测试因此比以前更要紧，不是更不要紧。** migrate_test.go 里那条
// TestTenantPoliciesArePresentAndExact 现在只逐字比对谓词文本
// `(merchant_id = current_merchant())` —— 那是形状检查，不是行为检查，
// 它对「策略挂在了一张列值全填错的表上」完全失明。而新策略的正确性恰恰依赖
// 另一件事：merchant_id 这一列真的被填对了。填它的是列默认值
// DEFAULT current_merchant() 与两条复合外键，而这三样都不在那条形状断言的视野里。
//
// 所以这里用**真实数据**把三个面各走一遍——读、改、插。数据模型 §4 的实测表
// 就是这三行，本测试是那张表的可执行版本。
func TestInventoriesPolicyBlocksCrossTenantAccess(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("invtest-%d", time.Now().UnixNano())
	// status = 2（停用）：这两家是夹具不是营业中的店，理由同上一个测试——
	// tenant.Preflight 断言的是整个库的形态，插一家活跃商家会让别的包随机红。
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2)
		 RETURNING id`, suffix+"-a", suffix+"-b").Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, suffix+"-b").Scan(&idB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{idA, idB}
		for _, stmt := range []string{
			`DELETE FROM inventories WHERE merchant_id = ANY($1)`,
			`DELETE FROM skus       WHERE merchant_id = ANY($1)`,
			`DELETE FROM products   WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories WHERE merchant_id = ANY($1)`,
			`DELETE FROM stores     WHERE merchant_id = ANY($1)`,
			`DELETE FROM regions    WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, ids); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	// 每家一个大区 + 一家默认门店。**这是本轮新加的夹具，不是样板代码**：
	// inventories 现在的主键是 (sku_id, store_id)，一行库存必须挂在一家真实的
	// 门店上，而那家门店必须属于同一个商家（复合外键钉死）。
	// 迁移里那段回填只覆盖迁移那一刻已经存在的商家，这两家是之后插的。
	skuOf := map[int64]int64{}
	storeOf := map[int64]int64{}
	for _, m := range []int64{idA, idB} {
		var regionID, storeID int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO regions (merchant_id, code, name) VALUES ($1,'r','R')
			 RETURNING id`, m).Scan(&regionID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO stores (merchant_id, region_id, code, name, is_default)
			 VALUES ($1,$2,'s','S',TRUE) RETURNING id`, m, regionID).Scan(&storeID); err != nil {
			t.Fatal(err)
		}
		storeOf[m] = storeID
	}
	for _, m := range []int64{idA, idB} {
		var catID, prodID, skuID int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'c','/c/')
			 RETURNING id`, m).Scan(&catID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO products (merchant_id, category_id, title, status, published_at)
			 VALUES ($1,$2,'p',1,now()) RETURNING id`, m, catID).Scan(&prodID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents)
			 VALUES ($1,$2,$3,100) RETURNING id`,
			m, prodID, fmt.Sprintf("%s-%d", suffix, m)).Scan(&skuID); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
			 VALUES ($1, $2, $3, 50)`, skuID, storeOf[m], m); err != nil {
			t.Fatal(err)
		}
		skuOf[m] = skuID
	}

	app, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	// 以商家 A 的身份连着做下面三件事。
	if _, err := app.Exec(ctx,
		`SELECT set_config('app.merchant_id', $1, false)`, fmt.Sprint(idA)); err != nil {
		t.Fatal(err)
	}

	t.Run("读不到别家的库存水位", func(t *testing.T) {
		// 这一条在数据模型 §4 的实测表里原先没写。读得到水位本身就是泄露：
		// 竞对能按分钟采样别家的可售数，直接反推销量。
		var n int
		if err := app.QueryRow(ctx,
			`SELECT count(*) FROM inventories WHERE sku_id = $1`, skuOf[idB]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("商家 A 看得到商家 B 的库存行（%d 行）—— EXISTS 策略的读侧没生效", n)
		}
		// 对照：自己的那一行必须看得见。否则「看不到」可能只是数据没播进去，
		// 上面那条断言会在策略被整个删掉的那天照样绿。
		if err := app.QueryRow(ctx,
			`SELECT count(*) FROM inventories WHERE sku_id = $1`, skuOf[idA]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("商家 A 看不到自己的库存行（%d 行）—— 夹具或策略写反了", n)
		}
	})

	t.Run("改不动别家的库存水位", func(t *testing.T) {
		tag, err := app.Exec(ctx,
			`UPDATE inventories SET available_qty = 1 WHERE sku_id = $1`, skuOf[idB])
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("商家 A 改动了商家 B 的库存（%d 行）", tag.RowsAffected())
		}
		// 真的没改到：绕过 RLS 用管理员连接回读。RowsAffected 为 0 但值变了
		// 是不可能的，但这一步顺带证明了夹具还在。
		var qty int32
		if err := admin.QueryRow(ctx,
			`SELECT available_qty FROM inventories WHERE sku_id = $1`, skuOf[idB]).Scan(&qty); err != nil {
			t.Fatal(err)
		}
		if qty != 50 {
			t.Fatalf("商家 B 的库存变成了 %d，期望 50", qty)
		}
	})

	t.Run("插不进别家 SKU 的库存行", func(t *testing.T) {
		// 这是 WITH CHECK 那一侧。数据模型 §4 记着：无策略时这句
		// `INSERT 0 1` 得手——「主键就是 sku_id 所以没有填错的自由度」
		// 那个旧判据是错的，挡住它的一直是策略。
		//
		// 先删掉 B 的库存行（用管理员连接），否则主键冲突会先于 RLS 报错，
		// 这条断言就变成在测主键。
		if _, err := admin.Exec(ctx,
			`DELETE FROM inventories WHERE sku_id = $1`, skuOf[idB]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(context.Background(),
				`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
				 VALUES ($1, $2, $3, 50)
				 ON CONFLICT (sku_id, store_id) DO NOTHING`,
				skuOf[idB], storeOf[idB], idB); err != nil {
				t.Errorf("恢复夹具失败: %v", err)
			}
		})

		// 显式写 merchant_id = B：不写的话列默认值会填成 A（当前上下文），
		// 语句会挂在复合外键上而不是 RLS 上，这条断言就变成在测外键。
		// 两道都在，而这里要测的是 WITH CHECK 那一道。
		_, err := app.Exec(ctx,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
			 VALUES ($1, $2, $3, 999)`, skuOf[idB], storeOf[idB], idB)
		if err == nil {
			t.Fatal("商家 A 往商家 B 的 SKU 上插进了库存行 —— WITH CHECK 没生效")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("期望 42501（new row violates row-level security policy），实得: %v", err)
		}
	})
}
