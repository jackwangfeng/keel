package handler_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
)

// 关键词召回的两处改动（2026-10 性能压测第六节 ③）：
//
//	① SearchProductsByKeyword 先按 ts_rank_cd 截到 N 件、再补价格与图片 —— 前 N 件必须与
//	  「不截断、全部算完再取前 N」逐行相同（含并列分数时按 id 的次序、价格、带价格过滤时）；
//	② 多词查询先 AND、不够一页再 OR 补齐（service/search.go 的 recallByKeyword）。

// kwProduct 是这一组测试往夹具里额外加的商品：只有 search_text、没有向量行，
// 所以它们只可能从关键词那一路进来 —— 向量路的候选面与改动无关，免得它干扰断言。
type kwProduct struct {
	Title, Subtitle string
	Cents           int64
}

// addKeywordProducts 往 merchant 的默认门店加商品（每件一个 SKU、10 件库存），返回 标题 → id。
func addKeywordProducts(t *testing.T, fx searchFixture, merchant int64, items []kwProduct) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	conn := admin(t)
	store, cat := fx.StoreA, fx.CategoryDressA
	if merchant == fx.MerchantB {
		store = fx.StoreB
		if err := conn.QueryRow(ctx, `SELECT id FROM categories WHERE merchant_id = $1 LIMIT 1`, merchant).Scan(&cat); err != nil {
			t.Fatal(err)
		}
	}
	repo := repository.New(testPool)
	ids := make(map[string]int64, len(items))
	for _, it := range items {
		var pid, sid int64
		if err := conn.QueryRow(ctx,
			`INSERT INTO products (merchant_id, category_id, title, subtitle, sales_count, status, published_at)
			 VALUES ($1,$2,$3,$4,0,1,now()) RETURNING id`,
			merchant, cat, it.Title, it.Subtitle).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx,
			`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents, status)
			 VALUES ($1,$2,$3,$4,1) RETURNING id`,
			merchant, pid, fmt.Sprintf("KW-%d", pid), it.Cents).Scan(&sid); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty, warning_qty)
			 VALUES ($1,$2,$3,10,1)`, sid, store, merchant); err != nil {
			t.Fatal(err)
		}
		text := search.ProductText{Title: it.Title, Subtitle: it.Subtitle}
		if err := repo.WithTenant(tenant.NewContext(ctx, merchant), func(tx repository.Tx) error {
			return tx.SetProductSearchText(ctx, pid, text.SearchText())
		}); err != nil {
			t.Fatal(err)
		}
		ids[it.Title] = pid
	}
	return ids
}

// ① 截断提前不改变前 N 件：与「不截断」的参考结果逐行比。
//
// 参考实现是同一条查询带一个远大于命中数的 limit（内层截断形同虚设，等于全部算完再排），
// 再在 Go 里取前 K 件；另外在 Go 里按 (rank DESC, id) 重排一遍参考结果，确认它本身就是
// 那个全序 —— 否则「两边一样」可能只是两边一起错。
//
// 夹具故意造出并列分数（同一段文字的几件商品 ts_rank_cd 相同），并列时次序只能靠 id 决定；
// 内层截断若丢了 id 这个次键，并列那几件在截断边界上会随机进出。
func TestKeywordRecallTruncatesAfterRankingWithoutChangingTopK(t *testing.T) {
	fx := newSearchFixture(t)
	var items []kwProduct
	for i := 0; i < 36; i++ {
		// 「栖木」出现 1–3 次、位置不同 → 分数有高有低；每档 12 件 → 大量并列。
		sub := strings.Repeat("栖木 款式 ", 1+i%3)
		items = append(items, kwProduct{
			Title: fmt.Sprintf("栖木家居 第%02d号", i), Subtitle: sub, Cents: int64(1000 + 137*i),
		})
	}
	addKeywordProducts(t, fx, fx.MerchantA, items)

	ctx := tenant.NewContext(t.Context(), fx.MerchantA)
	repo := repository.New(testPool)
	recall := func(f repository.SearchFilters, limit int32) []repository.SearchHit {
		t.Helper()
		var out []repository.SearchHit
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			var err error
			out, err = tx.SearchProductsByKeyword(ctx, fx.ScopeA(), search.TSQueryOr("栖木"), f, limit)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	minC := int64(3000)
	cases := []struct {
		name string
		f    repository.SearchFilters
	}{
		{"不带筛选（内层截断）", repository.SearchFilters{}},
		{"价格下界（内层不截断）", repository.SearchFilters{MinPriceCents: &minC}},
	}
	const k = 7
	for _, c := range cases {
		ref := recall(c.f, 10000)
		if len(ref) <= k {
			t.Fatalf("[%s] 命中只有 %d 件，不多于 K=%d —— 截断没有发生，这条测试没在检查任何东西", c.name, len(ref), k)
		}
		sorted := slices.Clone(ref)
		slices.SortStableFunc(sorted, func(a, b repository.SearchHit) int {
			if a.Rank != b.Rank {
				if a.Rank > b.Rank {
					return -1
				}
				return 1
			}
			return int(a.ID - b.ID)
		})
		if !slices.EqualFunc(ref, sorted, func(a, b repository.SearchHit) bool { return a.ID == b.ID }) {
			t.Fatalf("[%s] 参考结果本身不是按 (rank DESC, id) 排的", c.name)
		}
		ties := 0
		for i := 1; i < len(ref); i++ {
			if ref[i].Rank == ref[i-1].Rank {
				ties++
			}
		}
		if ties == 0 {
			t.Fatalf("[%s] 参考结果里没有并列分数 —— 「并列时按 id」这一半没在检查", c.name)
		}

		got := recall(c.f, k)
		if len(got) != k {
			t.Fatalf("[%s] 截断到 %d 件，实际返回 %d 件", c.name, k, len(got))
		}
		for i := range got {
			g, r := got[i], ref[i]
			if g.ID != r.ID || g.Rank != r.Rank || g.MinPriceCents != r.MinPriceCents ||
				g.MaxPriceCents != r.MaxPriceCents || g.MainImageUploadID != r.MainImageUploadID {
				t.Errorf("[%s] 第 %d 件与不截断的参考不同：截断 %+v，参考 %+v", c.name, i+1, g, r)
			}
		}
		if c.f.MinPriceCents != nil {
			for _, h := range ref {
				if h.MaxPriceCents < minC {
					t.Errorf("[%s] 价格下界 %d 没生效：%d 的最高价 %d", c.name, minC, h.ID, h.MaxPriceCents)
				}
			}
			if len(ref) == len(items) {
				t.Errorf("[%s] 价格下界一件都没筛掉 —— 「带价格过滤时内层不截断」没在检查", c.name)
			}
		}
	}
}
