package repository_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// RLS 与索引（2026-09-30 架构审查）：以应用角色（NOBYPASSRLS）跑 EXPLAIN，断言每一处修复
// 真的让计划用上了对应的索引。
//
// 为什么非得以应用角色跑：RLS 下规划器不许把非 leakproof 的算子排到租户谓词前面，
// 于是 ->>、LIKE、@@ 这些谓词只能当 Filter —— 同一条查询用超级用户 EXPLAIN 走得好好的，
// 线上却是全店扫。拿管理员连接做计划断言，等于在一个没有这个问题的世界里验证修复。
//
// 夹具要有量（每店几千行）并且 ANALYZE：行数太少时规划器本来就爱顺序扫，断言会随统计信息
// 漂移（search_hnsw_test.go 的 ANALYZE 那段写了同一个坑）。

const planFixtureRows = 3000

type planFixture struct {
	a, b     int64
	rootCatA int64 // A 店一个一级类目，下挂子类目
	admin    *pgx.Conn
}

func seedPlanFixture(t *testing.T) planFixture {
	t.Helper()
	ctx := context.Background()
	idA, idB := seedTwoTenants(t)
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	// 后注册先执行：这里的清理跑在 seedTwoTenants 那段之前，先删掉挂在商品 / 门店 / 用户上的行。
	t.Cleanup(func() {
		c := context.Background()
		all := []int64{idA, idB}
		for _, stmt := range []string{
			`DELETE FROM orders              WHERE merchant_id = ANY($1)`,
			`DELETE FROM users               WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_store_stock WHERE merchant_id = ANY($1)`,
			`DELETE FROM skus                WHERE merchant_id = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, all); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s\n%v", sql, err)
		}
	}

	fx := planFixture{a: idA, b: idB, admin: admin}
	for _, m := range []int64{idA, idB} {
		// 类目：20 个一级、每个下挂 20 个二级。path 形如 /id/id/（admin_categories.sql 的格式）。
		exec(`INSERT INTO categories (merchant_id, name, path, level)
		      SELECT $1, 'root' || g, '', 1 FROM generate_series(1, 20) g`, m)
		exec(`UPDATE categories SET path = '/' || id || '/' WHERE merchant_id = $1 AND path = ''`, m)
		exec(`INSERT INTO categories (merchant_id, parent_id, name, path, level)
		      SELECT $1, r.id, 'leaf' || g, '', 2
		        FROM categories r, generate_series(1, 20) g
		       WHERE r.merchant_id = $1 AND r.level = 1 AND r.name LIKE 'root%'`, m)
		exec(`UPDATE categories c SET path = p.path || c.id || '/'
		        FROM categories p
		       WHERE c.merchant_id = $1 AND c.path = '' AND p.id = c.parent_id`, m)

		// 商品：挂在二级类目上，关键词 kw0..kw999 各三件。B 店多一个 A 店没有的词。
		exec(`INSERT INTO products (merchant_id, category_id, title, status, published_at, search_text)
		      SELECT $1, (SELECT id FROM categories WHERE merchant_id = $1 AND level = 2
		                   ORDER BY id OFFSET (g % 400) LIMIT 1),
		             'plan-' || g, 1, now() - make_interval(mins => g),
		             'kw' || (g % 1000) || CASE WHEN $1 = $2 AND g = 7 THEN ' onlyinb' ELSE '' END
		        FROM generate_series(1, $3::int) g`, m, idB, planFixtureRows)

		// 订单：用户 500 个，收货手机号 1500 个，每店 3000 单。
		exec(`INSERT INTO users (merchant_id) SELECT $1 FROM generate_series(1, 500)`, m)
		var store, region int64
		if err := admin.QueryRow(ctx, `SELECT id, region_id FROM stores WHERE merchant_id = $1`, m).
			Scan(&store, &region); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO orders (merchant_id, order_no, user_id, goods_amount_cents, payable_cents,
		                          receiver_snapshot, expire_at, store_id, region_id, store_snapshot)
		      SELECT $1::bigint, 'plan-' || $1::bigint || '-' || g,
		             (SELECT id FROM users WHERE merchant_id = $1::bigint ORDER BY id OFFSET (g % 500) LIMIT 1),
		             100, 100,
		             jsonb_build_object('name', 'x', 'phone', '139' || lpad((g % 1500)::text, 8, '0')),
		             now() + interval '1 day', $2, $3, '{}'
		        FROM generate_series(1, $4::int) g`, m, store, region, planFixtureRows)

		// SKU 与有货标记（商品列表排序用）：约 1% 的商品没有 SKU；2% 没刷过标记（缺行）；
		// 刷过的里 5% 无货。四种情况（有货 / 无货 / 缺行有 SKU / 缺行无 SKU）都在。
		exec(`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents)
		      SELECT $1, p.id, 'plan-' || p.id, 100 FROM products p
		       WHERE p.merchant_id = $1 AND p.title LIKE 'plan-%' AND p.id % 97 <> 0`, m)
		exec(`INSERT INTO product_store_stock (store_id, product_id, merchant_id, in_stock)
		      SELECT $2, p.id, $1, p.id % 20 <> 0 FROM products p
		       WHERE p.merchant_id = $1 AND p.title LIKE 'plan-%' AND p.id % 50 <> 0`, m, store)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM categories WHERE merchant_id = $1 AND level = 1 AND name LIKE 'root%' ORDER BY id LIMIT 1`, idA).
		Scan(&fx.rootCatA); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"categories", "products", "orders", "users", "skus", "product_store_stock"} {
		exec(`ANALYZE ` + tbl)
	}
	return fx
}

// explainAsApp 在 A 店的租户事务里（应用角色）EXPLAIN 一条查询，返回整份计划文本。
// prepare 为真时先 PREPARE 再以强制通用计划 EXPLAIN EXECUTE：pgx 按语句缓存预备语句，
// 线上跑到的可能就是通用计划。
func explainAsApp(t *testing.T, merchant int64, sql string, generic bool, args ...any) string {
	t.Helper()
	ctx := context.Background()
	r := repository.New(pool(t))
	var plan string
	err := r.RawTenantTx(tenant.NewContext(ctx, merchant), func(tx pgx.Tx) error {
		q := `EXPLAIN (COSTS OFF) ` + sql
		if generic {
			if _, err := tx.Exec(ctx, `SET LOCAL plan_cache_mode = force_generic_plan`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `PREPARE plan_probe AS `+sql); err != nil {
				return err
			}
			defer tx.Exec(ctx, `DEALLOCATE plan_probe`)
			ph := make([]string, len(args))
			for i, a := range args {
				ph[i] = fmt.Sprintf("'%v'", a)
			}
			q = `EXPLAIN (COSTS OFF) EXECUTE plan_probe`
			if len(ph) > 0 {
				q += "(" + strings.Join(ph, ", ") + ")"
			}
			args = nil
		}
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			b.WriteString("  " + line + "\n")
		}
		plan = b.String()
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// 后台按手机号找单：收货手机号走 idx_orders_receiver_phone_col（00170 / 00171），
// 而且是在**通用计划**里 —— ByPhone 那条语句没有 IS NULL 分支，参数值未知也进得了 Index Cond。
func TestPlanAdminOrdersByPhoneUsesIndex(t *testing.T) {
	fx := seedPlanFixture(t)
	const byPhone = `SELECT o.id FROM orders o
	  WHERE o.status <> 0
	    AND (o.receiver_phone = $1::text
	         OR o.user_id = (SELECT u.id FROM users u WHERE u.phone = $1::text AND u.deleted_at IS NULL))
	  ORDER BY o.created_at DESC, o.id DESC LIMIT 20`
	plan := explainAsApp(t, fx.a, byPhone, true, "13900000017")
	if !strings.Contains(plan, "idx_orders_receiver_phone_col") {
		t.Errorf("按收货手机号找单没有用上 idx_orders_receiver_phone_col：\n%s", plan)
	}

	// 那一列由触发器维护，应用谁也不写它：插入时跟快照，改快照时跟着改。
	ctx := context.Background()
	var got string
	if err := fx.admin.QueryRow(ctx,
		`SELECT receiver_phone FROM orders WHERE order_no = $1`, fmt.Sprintf("plan-%d-17", fx.a)).
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "13900000017" {
		t.Errorf("receiver_phone = %q，期望与快照一致 13900000017", got)
	}
	if err := fx.admin.QueryRow(ctx,
		`UPDATE orders SET receiver_snapshot = receiver_snapshot || '{"phone":"13700000000"}'
		  WHERE order_no = $1 RETURNING receiver_phone`, fmt.Sprintf("plan-%d-17", fx.a)).
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "13700000000" {
		t.Errorf("改了快照之后 receiver_phone = %q，没跟着走", got)
	}
}

// 类目子树筛选：区间比较 ~>=~ / ~<~ 进了 idx_categories_path 的 Index Cond。
// 形状与 products.sql / reports.sql 里那段逐字一致（两条标量子查询）。
func TestPlanCategorySubtreeUsesPathIndex(t *testing.T) {
	fx := seedPlanFixture(t)
	const subtree = `SELECT c.id FROM categories c
	  WHERE c.deleted_at IS NULL
	    AND c.path ~>=~ (SELECT cc.path FROM categories cc WHERE cc.id = $1::bigint AND cc.deleted_at IS NULL)
	    AND c.path ~<~ (SELECT cc.path || chr(1114111) FROM categories cc
	                     WHERE cc.id = $1::bigint AND cc.deleted_at IS NULL)`
	plan := explainAsApp(t, fx.a, subtree, false, fx.rootCatA)
	if !strings.Contains(plan, "idx_categories_path") || !strings.Contains(plan, "~>=~") {
		t.Errorf("类目子树没有按 path 区间走 idx_categories_path：\n%s", plan)
	}

	// 语义对照：区间取出来的与 LIKE 前缀取出来的是同一批（一级类目 + 它的 20 个子类目）。
	ctx := context.Background()
	r := repository.New(pool(t))
	var byRange, byLike int
	if err := r.RawTenantTx(tenant.NewContext(ctx, fx.a), func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+subtree+`) s`, fx.rootCatA).Scan(&byRange); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM categories c
		  WHERE c.deleted_at IS NULL
		    AND c.path LIKE (SELECT cc.path FROM categories cc WHERE cc.id = $1) || '%'`, fx.rootCatA).Scan(&byLike)
	}); err != nil {
		t.Fatal(err)
	}
	if byRange != 21 || byRange != byLike {
		t.Errorf("子树行数：区间 %d、LIKE %d，期望都是 21", byRange, byLike)
	}
}

// 关键词召回：外层不再逐行算 @@，只拿 keyword_hit_products 返回的 id 去对 products；
// 函数体（以它的 owner 身份）走 GIN idx_products_fts。
//
// 外层是「按 id 逐个回表」还是「哈希半连接」由代价决定：每店 4 万商品时是前者（实测
// Nested Loop + products_pkey），这份 3000 行的夹具上是后者。两者都不再对全店行做
// ts_match_vq，所以这里断言的是「计划里没有 search_vector 的 Filter」，不钉具体连接方式。
func TestPlanKeywordSearchUsesGIN(t *testing.T) {
	fx := seedPlanFixture(t)
	const outer = `SELECT p.id FROM products p
	  WHERE p.deleted_at IS NULL AND p.status = 1
	    AND p.id IN (SELECT keyword_hit_products(to_tsquery('simple', $1::text)))`
	plan := explainAsApp(t, fx.a, outer, false, "kw17")
	if !strings.Contains(plan, "keyword_hit_products") || strings.Contains(plan, "search_vector @@") {
		t.Errorf("外层没有改用 keyword_hit_products 的命中集合，还在逐行匹配：\n%s", plan)
	}

	// 函数体的计划在外层 EXPLAIN 里是个黑盒，以 owner 的身份单独看一次。
	ctx := context.Background()
	tx, err := fx.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE keel_search_definer`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, fmt.Sprint(fx.a)); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN (COSTS OFF)
	  SELECT p.id FROM public.products p
	   WHERE p.merchant_id = public.current_merchant() AND p.search_vector @@ to_tsquery('simple', 'kw17')`)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString("  " + line + "\n")
	}
	rows.Close()
	if !strings.Contains(b.String(), "idx_products_fts") {
		t.Errorf("keyword_hit_products 的函数体没有走 GIN idx_products_fts：\n%s", b.String())
	}

	// 背景：同一个谓词直接写在应用角色的查询里，RLS 下 GIN 进不了计划。
	// 这里只记录不断言 —— 哪天 PostgreSQL 把 ts_match_vq 标成 leakproof，这条就会变，
	// 那时 00172 可以退役。
	direct := explainAsApp(t, fx.a,
		`SELECT p.id FROM products p WHERE p.search_vector @@ to_tsquery('simple', $1::text)`, false, "kw17")
	if strings.Contains(direct, "idx_products_fts") {
		t.Logf("注意：RLS 下直接写 @@ 也用上了 GIN，00172 的前提可能已经不成立：\n%s", direct)
	}
}

// A 店搜不到 B 店的商品，哪怕关键词只有 B 店有 —— keyword_hit_products 是 SECURITY DEFINER，
// 这条测试守的是「函数体里的 merchant_id = current_merchant() 与外层 RLS 两道都在」。
func TestKeywordSearchIsolatedAcrossTenants(t *testing.T) {
	fx := seedPlanFixture(t)
	ctx := context.Background()
	r := repository.New(pool(t))
	search := func(m int64) []repository.SearchHit {
		t.Helper()
		sc := defaultScope(t, m)
		var hits []repository.SearchHit
		if err := r.WithTenant(tenant.NewContext(ctx, m), func(tx repository.Tx) error {
			var err error
			hits, err = tx.SearchProductsByKeyword(ctx, sc, "onlyinb", repository.SearchFilters{}, 50)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return hits
	}
	if hits := search(fx.a); len(hits) != 0 {
		t.Errorf("A 店搜到了只有 B 店才有的词：%+v", hits)
	}
	if hits := search(fx.b); len(hits) != 1 {
		t.Errorf("阳性对照：B 店搜自己的词应当命中 1 件，实得 %d —— 上一条断言因此说明不了什么", len(hits))
	}

	// 函数本身也只吐本店的 id（外层 RLS 之外的第一道）。
	var n int
	if err := r.RawTenantTx(tenant.NewContext(ctx, fx.a), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM keyword_hit_products(to_tsquery('simple', 'onlyinb'))`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("keyword_hit_products 在 A 店的事务里返回了 %d 个 B 店的 id", n)
	}

	// 没有租户上下文时报错（fail-closed），而不是返回全平台的命中。
	c, err := pgx.Connect(ctx, db.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := c.QueryRow(ctx,
		`SELECT count(*) FROM keyword_hit_products(to_tsquery('simple', 'onlyinb'))`).Scan(&n); err == nil {
		t.Errorf("没设 app.merchant_id 也调通了，返回 %d 行", n)
	}
}

// keel_search_definer 必须窄：不能登录、没有任何成员、只读得到 products 的三列。
// 它带 BYPASSRLS，这三条任何一条松了，它就是一条绕过 RLS 的通道。
func TestKeywordDefinerRoleIsNarrow(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	var canLogin, super bool
	if err := admin.QueryRow(ctx,
		`SELECT rolcanlogin, rolsuper FROM pg_roles WHERE rolname = 'keel_search_definer'`).
		Scan(&canLogin, &super); err != nil {
		t.Fatal(err)
	}
	if canLogin || super {
		t.Errorf("keel_search_definer: rolcanlogin=%v rolsuper=%v，两者都必须为 false", canLogin, super)
	}
	var members int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_auth_members m
	    JOIN pg_roles r ON r.oid = m.roleid WHERE r.rolname = 'keel_search_definer'`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 0 {
		t.Errorf("keel_search_definer 有 %d 个成员 —— 成员能 SET ROLE 过去拿到 BYPASSRLS", members)
	}
	var tables int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.role_table_grants
	    WHERE grantee = 'keel_search_definer'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("keel_search_definer 有 %d 条整表授权，期望 0（只该有 products 的列级 SELECT）", tables)
	}
	rows, err := admin.Query(ctx, `SELECT table_name || '.' || column_name || ':' || privilege_type
	    FROM information_schema.column_privileges
	   WHERE grantee = 'keel_search_definer' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, s)
	}
	rows.Close()
	want := "products.id:SELECT,products.merchant_id:SELECT,products.search_vector:SELECT"
	if got := strings.Join(cols, ","); got != want {
		t.Errorf("keel_search_definer 的列级授权 = %s，期望 %s", got, want)
	}
}

// 商品列表「有货在前」：有货段按 idx_products_listing_published 的顺序取、LIMIT 先生效，
// 计划里没有对全店行的 Sort（00064 那条索引重新提供顺序）。
func TestPlanProductListingUsesPublishedIndex(t *testing.T) {
	fx := seedPlanFixture(t)
	sc := defaultScope(t, fx.a)
	const inStockPage = `SELECT p.id FROM products p
	 WHERE p.deleted_at IS NULL AND p.status = 1
	   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
	                    WHERE ro.region_id = $2::bigint AND ro.product_id = p.id AND ro.status = 0)
	   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
	                    WHERE so.store_id = $1::bigint AND so.product_id = p.id AND so.status = 0)
	   AND COALESCE((SELECT pss.in_stock FROM product_store_stock pss
	                  WHERE pss.store_id = $1::bigint AND pss.product_id = p.id),
	                EXISTS (SELECT 1 FROM skus sk WHERE sk.product_id = p.id AND sk.status = 1 AND sk.deleted_at IS NULL))
	       = true
	 ORDER BY p.published_at DESC NULLS LAST, p.id DESC
	 LIMIT 20 OFFSET 0`
	plan := explainAsApp(t, fx.a, inStockPage, true, sc.StoreID, sc.RegionID)
	if !strings.Contains(plan, "idx_products_listing_published") {
		t.Errorf("商品列表的有货段没有走 idx_products_listing_published：\n%s", plan)
	}
	if strings.Contains(plan, "Sort Key") {
		t.Errorf("商品列表的有货段还在排序 —— 索引没有提供顺序，LIMIT 先不了：\n%s", plan)
	}
}

// 两段拼出来的顺序与原来那句 ORDER BY 有货 DESC, published_at DESC, id DESC 逐行一致，
// 页大小取一个不整除的 7，让边界页落在有货段中间；再翻过末尾，看最后一页与越界页。
func TestListProductsStockOrderMatchesSingleSort(t *testing.T) {
	fx := seedPlanFixture(t)
	ctx := context.Background()
	sc := defaultScope(t, fx.a)
	r := repository.New(pool(t))
	tctx := tenant.NewContext(ctx, fx.a)

	// 让无货段靠前一些：把最新的 30 件里的 10 件标成无货，边界页才会出现在前几页里。
	if _, err := fx.admin.Exec(ctx, `UPDATE product_store_stock SET in_stock = false
	    WHERE merchant_id = $1 AND product_id IN (
	      SELECT id FROM products WHERE merchant_id = $1 AND title LIKE 'plan-%'
	       ORDER BY published_at DESC LIMIT 30) AND product_id % 3 = 0`, fx.a); err != nil {
		t.Fatal(err)
	}

	var want []int64
	if err := r.RawTenantTx(tctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT p.id FROM products p
		 WHERE p.deleted_at IS NULL AND p.status = 1
		   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
		                    WHERE ro.region_id = $2 AND ro.product_id = p.id AND ro.status = 0)
		   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
		                    WHERE so.store_id = $1 AND so.product_id = p.id AND so.status = 0)
		 ORDER BY COALESCE((SELECT pss.in_stock FROM product_store_stock pss
		                     WHERE pss.store_id = $1 AND pss.product_id = p.id),
		                   FALSE) DESC, -- 缺行按无货（2026-10-01，products.sql 文件头）
		          p.published_at DESC NULLS LAST, p.id DESC`, sc.StoreID, sc.RegionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			want = append(want, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}

	const size = 7
	var got []int64
	for off := int64(0); ; off += size {
		var page []repository.Product
		if err := r.WithTenant(tctx, func(tx repository.Tx) error {
			var err error
			page, err = tx.ListProducts(ctx, sc, repository.ListingFilter{}, size, off)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			got = append(got, p.ID)
		}
		if len(page) < size {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("分页取到 %d 件，单句排序是 %d 件", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 件（第 %d 页）：分两段取得 %d，单句排序是 %d", i, i/size+1, got[i], want[i])
		}
	}

	// 越界页：一行都不该有（有货段取 0 行 → 数有货段 → 无货段起点也越界）。
	if err := r.WithTenant(tctx, func(tx repository.Tx) error {
		page, err := tx.ListProducts(ctx, sc, repository.ListingFilter{}, size, int64(len(want)+size))
		if err == nil && len(page) != 0 {
			t.Errorf("越界页取到 %d 件", len(page))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
