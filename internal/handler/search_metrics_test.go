package handler_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// scripts/search_metrics.sql（make search-metrics）的口径闸门。
//
// 读的是**那份文件本身**，把 psql 变量 :'period' 换成 $1，在测试库上跑，逐格比对。
// 数据直接用管理员连接插进 search_logs，而不是经 /search 造：这里要验的是
// 统计口径（分母是谁、第 11 名的点击算不算 CTR@10、降级那一次归哪一桶），
// 每一格的期望值都得是手算得出来的，而经真实检索造出来的 ranked_ids 取决于夹具与排序。
//
// 放在 handler 包里只是为了复用 newSearchFixture 造两家带清理的店。

type metricsRow struct {
	Merchant, Strategy, Stages                string
	Searches                                  int64
	ZeroRate, CTR10, CartRate, OrderRate, MRR *float64
}

func runSearchMetrics(t *testing.T, period string) []metricsRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "scripts", "search_metrics.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	if n := strings.Count(sql, ":'period'"); n != 1 {
		t.Fatalf("search_metrics.sql 里 :'period' 出现 %d 次，这条测试按「恰好一次」换参数", n)
	}
	sql = strings.Replace(sql, ":'period'", "$1", 1)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	rows, err := admin.Query(ctx, sql, period)
	if err != nil {
		t.Fatalf("跑 search_metrics.sql 失败：%v", err)
	}
	defer rows.Close()
	var out []metricsRow
	for rows.Next() {
		var r metricsRow
		if err := rows.Scan(&r.Merchant, &r.Strategy, &r.Stages, &r.Searches,
			&r.ZeroRate, &r.CTR10, &r.CartRate, &r.OrderRate, &r.MRR); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSearchMetricsScriptComputesTheDocumentedRates(t *testing.T) {
	fx := newSearchFixture(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	var codeA, codeB string
	if err := admin.QueryRow(ctx, `SELECT code FROM merchants WHERE id = $1`, fx.MerchantA).Scan(&codeA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT code FROM merchants WHERE id = $1`, fx.MerchantB).Scan(&codeB); err != nil {
		t.Fatal(err)
	}

	full := []string{"vector", "keyword", "rrf", "business"}
	degraded := []string{"keyword", "rrf", "business"}
	twelve := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	n := 0
	insert := func(merchant int64, strategy string, stages []string, ranked []int64,
		clicked, carted, ordered *int64, age string) {
		t.Helper()
		n++
		if _, err := admin.Exec(ctx, `
			INSERT INTO search_logs (merchant_id, query, ranked_ids, recall_ids, trace_id, strategy,
			                         stages, clicked_id, carted_id, ordered_id, created_at)
			VALUES ($1, 'q', $2, $2, $3, $4, $5, $6, $7, $8, now() - $9::interval)`,
			merchant, ranked, fmt.Sprintf("%032x", int64(merchant)*1000+int64(n)), strategy, stages,
			clicked, carted, ordered, age); err != nil {
			t.Fatal(err)
		}
	}
	p := func(v int64) *int64 { return &v }

	// A 店默认策略、双路都跑成：五次检索。
	insert(fx.MerchantA, "rrf-biz-v1", full, []int64{1, 2, 3}, p(1), p(1), p(1), "1 hour") // 第 1 名被点，加购、下单
	insert(fx.MerchantA, "rrf-biz-v1", full, []int64{1, 2, 3}, p(3), p(2), nil, "1 hour")  // 第 3 名被点，加购另一件
	insert(fx.MerchantA, "rrf-biz-v1", full, twelve, p(12), nil, nil, "1 hour")            // 第 12 名：不算 CTR@10
	insert(fx.MerchantA, "rrf-biz-v1", full, []int64{1, 2}, nil, nil, nil, "1 hour")       // 有结果、没点
	insert(fx.MerchantA, "rrf-biz-v1", full, []int64{}, nil, nil, nil, "1 hour")           // 无结果
	// 同一策略、引擎挂了（向量路没跑）：必须是另一桶。
	insert(fx.MerchantA, "rrf-biz-v1", degraded, []int64{5}, p(5), nil, nil, "1 hour")
	// rrf-v1：一次无结果 —— 三个率的分母为 0，应当是 NULL 而不是 0 或除零错。
	insert(fx.MerchantA, "rrf-v1", []string{"vector", "keyword", "rrf"}, []int64{}, nil, nil, nil, "1 hour")
	// 窗口之外：不该被算进任何一桶。
	insert(fx.MerchantA, "rrf-biz-v1", full, []int64{1}, p(1), p(1), p(1), "30 days")
	// B 店：自己一桶，不混进 A。
	insert(fx.MerchantB, "rrf-biz-v1", full, []int64{9}, nil, nil, nil, "1 hour")

	got := map[string]metricsRow{}
	for _, r := range runSearchMetrics(t, "7 days") {
		if r.Merchant == codeA || r.Merchant == codeB {
			got[r.Merchant+"|"+r.Strategy+"|"+r.Stages] = r
		}
	}

	f := func(v float64) *float64 { return &v }
	want := map[string]metricsRow{
		codeA + "|rrf-biz-v1|vector+keyword+rrf+business": {Searches: 5,
			ZeroRate: f(0.2), CTR10: f(0.5), CartRate: f(0.5), OrderRate: f(0.25),
			MRR: f(math.Round((1+1.0/3+1.0/12)/3*1e4) / 1e4)},
		codeA + "|rrf-biz-v1|keyword+rrf+business": {Searches: 1,
			ZeroRate: f(0), CTR10: f(1), CartRate: f(0), OrderRate: f(0), MRR: f(1)},
		codeA + "|rrf-v1|vector+keyword+rrf": {Searches: 1, ZeroRate: f(1)},
		codeB + "|rrf-biz-v1|vector+keyword+rrf+business": {Searches: 1,
			ZeroRate: f(0), CTR10: f(0), CartRate: f(0), OrderRate: f(0)},
	}
	if len(got) != len(want) {
		t.Fatalf("两家店统计出 %d 桶，期望 %d：%+v", len(got), len(want), got)
	}
	fmtp := func(p *float64) string {
		if p == nil {
			return "NULL"
		}
		return fmt.Sprint(*p)
	}
	same := func(a, b *float64) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return math.Abs(*a-*b) < 1e-9
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("缺一桶 %s", k)
			continue
		}
		if g.Searches != w.Searches {
			t.Errorf("%s searches = %d，期望 %d（窗口之外那一行不该算进来）", k, g.Searches, w.Searches)
		}
		for _, c := range []struct {
			name      string
			got, want *float64
		}{
			{"zero_rate", g.ZeroRate, w.ZeroRate}, {"ctr10", g.CTR10, w.CTR10},
			{"cart_rate", g.CartRate, w.CartRate}, {"order_rate", g.OrderRate, w.OrderRate},
			{"mrr_click", g.MRR, w.MRR},
		} {
			if !same(c.got, c.want) {
				t.Errorf("%s %s = %s，期望 %s", k, c.name, fmtp(c.got), fmtp(c.want))
			}
		}
	}
}
