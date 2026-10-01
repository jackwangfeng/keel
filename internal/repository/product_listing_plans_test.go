package repository_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 买家商品列表的性能修复（2026-10-01，性能压测 docs/性能压测-2026-10.md 六 ② ⑦ ⑧）：
// 小类目按 idx_products_listing_category 取页、深分页只对这一页算价格、「只看有货」计数走部分索引。
//
// 与 rls_index_plans_test.go 的区别：那边的计划断言是对着手抄的查询形状跑的；这里直接从
// db/queries/products.sql 里按名字取出 sqlc 的那条语句（sqlc.arg 换成 $n），断言的就是线上那一句 ——
// 改了 SQL 而计划退化，这里会红，不用记得回来同步一份抄本。

// listingQuery 从 db/queries/products.sql 取出名为 name 的那条语句，sqlc.arg(x) / sqlc.narg(x) 按首次出现的
// 次序换成 $1、$2……，返回语句与参数名的次序。
func listingQuery(t *testing.T, name string) (string, []string) {
	t.Helper()
	src, err := os.ReadFile("../../db/queries/products.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	head := "-- name: " + name + " "
	i := strings.Index(s, head)
	if i < 0 {
		t.Fatalf("products.sql 里没有 %s", name)
	}
	body := s[i+len(head):]
	body = body[strings.Index(body, "\n")+1:]
	if j := strings.Index(body, "\n-- name: "); j >= 0 {
		body = body[:j]
	}
	body = strings.TrimRight(strings.TrimSpace(body), ";")

	var names []string
	pos := map[string]int{}
	re := regexp.MustCompile(`sqlc\.n?arg\((\w+)\)`)
	body = re.ReplaceAllStringFunc(body, func(m string) string {
		n := re.FindStringSubmatch(m)[1]
		if _, ok := pos[n]; !ok {
			names = append(names, n)
			pos[n] = len(names)
		}
		return "$" + strconv.Itoa(pos[n])
	})
	return body, names
}

// argsOf 按 listingQuery 给的次序排参数。数组写成 PostgreSQL 的字面量（'{1,2}'），
// 这样同一组参数既能直接绑定，也能拼进 EXPLAIN EXECUTE（explainAsApp 的通用计划那条路）。
func argsOf(t *testing.T, names []string, vals map[string]any) []any {
	t.Helper()
	out := make([]any, len(names))
	for i, n := range names {
		v, ok := vals[n]
		if !ok {
			t.Fatalf("缺参数 %s", n)
		}
		out[i] = v
	}
	return out
}

func arrayLiteral(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// subtreeOf 以应用角色取一个类目的子树（走 repository 那一条，顺带验证它）。
func subtreeOf(t *testing.T, merchant, category int64) []int64 {
	t.Helper()
	ctx := context.Background()
	r := repository.New(pool(t))
	var ids []int64
	if err := r.WithTenant(tenant.NewContext(ctx, merchant), func(tx repository.Tx) error {
		var err error
		ids, err = tx.CategorySubtreeIDs(ctx, category)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

// 小类目：按类目取页的那条语句在**通用计划**里也是逐个类目走 idx_products_listing_category，
// 没有回到「沿上架时间索引扫全店、逐行判类目」；计数同样。
func TestPlanListingByCategoryUsesCategoryIndex(t *testing.T) {
	fx := seedPlanFixture(t)
	sc := defaultScope(t, fx.a)
	ids := subtreeOf(t, fx.a, fx.rootCatA)
	if len(ids) != 21 {
		t.Fatalf("一级类目的子树 %d 个，期望 21（自己 + 20 个子类目）", len(ids))
	}
	leaf := ids[len(ids)-1:]

	vals := map[string]any{
		"category_ids": arrayLiteral(leaf), "store_id": sc.StoreID, "region_id": sc.RegionID,
		"in_stock": true, "page_limit": 20, "page_offset": 0,
	}
	for _, name := range []string{"ListProductsByStockInCategories", "CountProductsInCategories",
		"CountProductsInStockInCategories"} {
		q, names := listingQuery(t, name)
		for _, generic := range []bool{false, true} {
			plan := explainAsApp(t, fx.a, q, generic, argsOf(t, names, vals)...)
			if !strings.Contains(plan, "idx_products_listing_category") {
				t.Errorf("%s（通用计划=%v）没有走 idx_products_listing_category：\n%s", name, generic, plan)
			}
			if strings.Contains(plan, "idx_products_listing_published") {
				t.Errorf("%s（通用计划=%v）还在沿上架时间索引扫全店：\n%s", name, generic, plan)
			}
		}
	}

	// Wide 那条刻意走上架时间索引（category_id + 0 进不了类目索引，products.sql「类目筛选」）。
	q, names := listingQuery(t, "ListProductsByStockInWideCategories")
	vals["category_ids"] = arrayLiteral(ids)
	plan := explainAsApp(t, fx.a, q, true, argsOf(t, names, vals)...)
	if !strings.Contains(plan, "idx_products_listing_published") {
		t.Errorf("Wide 那条没有沿 idx_products_listing_published 按序取：\n%s", plan)
	}
}

// 「只看有货」计数：从 product_store_stock 的部分索引出发、仅索引扫描，不再按主键位图扫描回表。
func TestPlanInStockCountUsesPartialIndex(t *testing.T) {
	fx := seedPlanFixture(t)
	sc := defaultScope(t, fx.a)
	q, names := listingQuery(t, "CountProductsInStock")
	for _, generic := range []bool{false, true} {
		plan := explainAsApp(t, fx.a, q, generic,
			argsOf(t, names, map[string]any{"store_id": sc.StoreID, "region_id": sc.RegionID})...)
		if !strings.Contains(plan, "Index Only Scan using idx_product_store_stock_in_stock") {
			t.Errorf("CountProductsInStock（通用计划=%v）没有走 idx_product_store_stock_in_stock 的仅索引扫描：\n%s",
				generic, plan)
		}
	}
}

// 深分页：价格（skus）与主图（product_images）只对这一页的行各查一次，OFFSET 跳过的行不算。
// 改之前 OFFSET 980 时这两处 loops=1000。
func TestPlanListingPricesOnlyThePage(t *testing.T) {
	fx := seedPlanFixture(t)
	sc := defaultScope(t, fx.a)
	ids := subtreeOf(t, fx.a, fx.rootCatA)
	ctx := context.Background()
	r := repository.New(pool(t))
	const limit, offset = 5, 100
	loops := regexp.MustCompile(`loops=(\d+)`)

	for name, vals := range map[string]map[string]any{
		"ListProductsByStock":                 {},
		"ListProductsByStockInCategories":     {"category_ids": arrayLiteral(ids)},
		"ListProductsByStockInWideCategories": {"category_ids": arrayLiteral(ids)},
	} {
		vals["store_id"], vals["region_id"] = sc.StoreID, sc.RegionID
		vals["in_stock"], vals["page_limit"], vals["page_offset"] = true, limit, offset
		q, names := listingQuery(t, name)
		var plan []string
		if err := r.RawTenantTx(tenant.NewContext(ctx, fx.a), func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) `+q,
				argsOf(t, names, vals)...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				plan = append(plan, line)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		checked := 0
		for _, line := range plan {
			if !strings.Contains(line, " on skus ") && !strings.Contains(line, " on product_images ") {
				continue
			}
			checked++
			m := loops.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if n, _ := strconv.Atoi(m[1]); n > limit {
				t.Errorf("%s：%s —— 对 OFFSET 跳过的行也算了价格 / 主图\n%s", name, strings.TrimSpace(line),
					strings.Join(plan, "\n"))
			}
		}
		if checked == 0 {
			t.Errorf("%s 的计划里找不到 skus / product_images 的扫描，断言落空：\n%s", name, strings.Join(plan, "\n"))
		}
	}
}

// 按类目筛的分页：per-category 与 wide 两种取法翻完全部页，都与单句
// ORDER BY 有货 DESC, published_at DESC, id DESC（加类目条件）逐行一致；「只看有货」与两条计数也对得上。
// 页大小 7 不整除，让边界页落在有货段中间。
func TestListProductsByCategoryMatchesSingleSort(t *testing.T) {
	fx := seedPlanFixture(t)
	ctx := context.Background()
	sc := defaultScope(t, fx.a)
	r := repository.New(pool(t))
	tctx := tenant.NewContext(ctx, fx.a)
	ids := subtreeOf(t, fx.a, fx.rootCatA)

	// 让无货段靠前：这棵子树里最新的 30 件里每三件标一件无货。
	if _, err := fx.admin.Exec(ctx, `UPDATE product_store_stock SET in_stock = false
	    WHERE merchant_id = $1 AND product_id IN (
	      SELECT id FROM products WHERE merchant_id = $1 AND category_id = ANY($2)
	       ORDER BY published_at DESC LIMIT 30) AND product_id % 3 = 0`, fx.a, ids); err != nil {
		t.Fatal(err)
	}

	single := func(cats []int64, inStockOnly bool) []int64 {
		var want []int64
		if err := r.RawTenantTx(tctx, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT p.id FROM products p
			 WHERE p.deleted_at IS NULL AND p.status = 1 AND p.category_id = ANY($3)
			   AND NOT EXISTS (SELECT 1 FROM region_product_overrides ro
			                    WHERE ro.region_id = $2 AND ro.product_id = p.id AND ro.status = 0)
			   AND NOT EXISTS (SELECT 1 FROM store_product_overrides so
			                    WHERE so.store_id = $1 AND so.product_id = p.id AND so.status = 0)
			   AND (NOT $4 OR COALESCE((SELECT pss.in_stock FROM product_store_stock pss
			                             WHERE pss.store_id = $1 AND pss.product_id = p.id), FALSE))
			 ORDER BY COALESCE((SELECT pss.in_stock FROM product_store_stock pss
			                     WHERE pss.store_id = $1 AND pss.product_id = p.id), FALSE) DESC,
			          p.published_at DESC NULLS LAST, p.id DESC`, sc.StoreID, sc.RegionID, cats, inStockOnly)
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
		return want
	}

	// Rows / StoreRows 决定走哪条（perCategoryCheaper）：不给件数 → per-category；
	// 给一个「这批类目占全店一半」的件数 → 类目多（21 个）时 wide 更便宜。
	all := single(ids, false)
	forWide := repository.ListingFilter{Categories: ids, Rows: 1500, StoreRows: 3000}
	if repository.PerCategoryCheaper(forWide, 7, 0) {
		t.Fatal("夹具没能让 wide 被选中，下面那一半测不到它")
	}
	for _, tc := range []struct {
		name string
		f    repository.ListingFilter
		want []int64
	}{
		{"per-category / 一级类目", repository.ListingFilter{Categories: ids}, all},
		{"wide / 一级类目", forWide, all},
		{"叶子类目", repository.ListingFilter{Categories: ids[1:2]}, single(ids[1:2], false)},
	} {
		for _, inStockOnly := range []bool{false, true} {
			want := tc.want
			if inStockOnly {
				want = single(tc.f.Categories, true)
			}
			const size = 7
			var got []int64
			for off := int64(0); ; off += size {
				var page []repository.Product
				if err := r.WithTenant(tctx, func(tx repository.Tx) error {
					var err error
					if inStockOnly {
						page, err = tx.ListProductsInStock(ctx, sc, tc.f, size, off)
					} else {
						page, err = tx.ListProducts(ctx, sc, tc.f, size, off)
					}
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
			label := fmt.Sprintf("%s（只看有货=%v）", tc.name, inStockOnly)
			if len(want) == 0 {
				t.Fatalf("%s：单句排序一件都没有，夹具不对", label)
			}
			if len(got) != len(want) {
				t.Fatalf("%s：分页取到 %d 件，单句排序是 %d 件", label, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s：第 %d 件（第 %d 页）分页取得 %d，单句排序是 %d", label, i, i/size+1, got[i], want[i])
				}
			}

			// 计数与列表同一组条件。
			if err := r.WithTenant(tctx, func(tx repository.Tx) error {
				count := tx.CountProducts
				if inStockOnly {
					count = tx.CountProductsInStock
				}
				n, err := count(ctx, sc, tc.f)
				if err == nil && n != int64(len(want)) {
					t.Errorf("%s：计数 %d，列表翻完是 %d 件", label, n, len(want))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 不存在的类目：子树是非 nil 的空切片 → 空列表、计数 0（不是回退成全部商品）。
	none := subtreeOf(t, fx.a, -1)
	if none == nil || len(none) != 0 {
		t.Fatalf("不存在的类目子树 = %#v，期望非 nil 的空切片", none)
	}
	if err := r.WithTenant(tctx, func(tx repository.Tx) error {
		page, err := tx.ListProducts(ctx, sc, repository.ListingFilter{Categories: none}, 20, 0)
		if err != nil {
			return err
		}
		n, err := tx.CountProducts(ctx, sc, repository.ListingFilter{Categories: none})
		if len(page) != 0 || n != 0 {
			t.Errorf("不存在的类目取到 %d 件、计数 %d，期望都是 0", len(page), n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// 别家的类目：子树在 RLS 下是空的（B 店看不到 A 店的类目）。
	if got := subtreeOf(t, fx.b, fx.rootCatA); len(got) != 0 {
		t.Errorf("B 店取到了 A 店类目的子树 %v", got)
	}
}

// perCategoryCheaper 的几个取向（数字是 10 万商品压测库上的形状）。
func TestPerCategoryCheaper(t *testing.T) {
	big := []int64{6, 7, 8, 9, 10, 11} // 一级类目 + 5 个子类目，1 万件 / 全店 10 万
	for _, tc := range []struct {
		name          string
		f             repository.ListingFilter
		limit, offset int64
		want          bool
	}{
		{"叶子类目恒走 per-category", repository.ListingFilter{Categories: []int64{7}, Rows: 20000, StoreRows: 100000}, 20, 980, true},
		{"件数不知道走 per-category", repository.ListingFilter{Categories: big}, 20, 980, true},
		{"小的一级类目（几件）", repository.ListingFilter{Categories: big, Rows: 5, StoreRows: 100000}, 20, 0, true},
		{"大类目第一页走 wide", repository.ListingFilter{Categories: big, Rows: 10000, StoreRows: 100000}, 20, 0, false},
		{"大类目深页走 wide", repository.ListingFilter{Categories: big, Rows: 10000, StoreRows: 100000}, 20, 980, false},
		{"占比 1% 的一级类目第一页走 per-category", repository.ListingFilter{Categories: big, Rows: 1000, StoreRows: 100000}, 20, 0, true},
	} {
		if got := repository.PerCategoryCheaper(tc.f, tc.limit, tc.offset); got != tc.want {
			t.Errorf("%s：perCategoryCheaper = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

// 深页（offset+limit 超过 jitOffWindow）在这个事务里关掉 JIT，浅页不动（不多发那条语句）。
// 为什么要关：深页、按类目筛的写法估算代价越过 jit_above_cost，每次先花 ≈200 ms 编译、执行只要 12 ms
// （product.go 的 jitOffWindow）。
func TestListProductsDeepPageTurnsOffJIT(t *testing.T) {
	fx := seedPlanFixture(t)
	ctx := context.Background()
	sc := defaultScope(t, fx.a)
	r := repository.New(pool(t))
	for _, tc := range []struct {
		offset int64
		off    bool
	}{{0, false}, {980, true}} {
		if err := r.RawAndTenantTx(tenant.NewContext(ctx, fx.a), func(raw pgx.Tx, tx repository.Tx) error {
			var before, after string
			if err := raw.QueryRow(ctx, `SHOW jit`).Scan(&before); err != nil {
				return err
			}
			if _, err := tx.ListProducts(ctx, sc, repository.ListingFilter{}, 20, tc.offset); err != nil {
				return err
			}
			if err := raw.QueryRow(ctx, `SHOW jit`).Scan(&after); err != nil {
				return err
			}
			if tc.off && after != "off" {
				t.Errorf("offset %d：取页之后 jit = %s，期望 off", tc.offset, after)
			}
			if !tc.off && after != before {
				t.Errorf("offset %d：浅页也动了 jit（%s → %s）", tc.offset, before, after)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
