package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/keel/keel/internal/problem"
)

// 每个 MCP 工具都真调一次（店里有一笔已付款订单、一条提案、一份简报），返回值必须通过它声明的 outputSchema ——
// SDK 在返回前按 outputSchema 校验，不符就是协议错误（mcpCall 当场 Fatal）。
// 这条测试守的是「声明的形状与真实返回一致」：接入方照着 outputSchema 写的解析不会在某个空字段上崩。
func TestMCPEveryToolReturnsItsDeclaredShape(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)

	b := cs.newBuyer(t, "schema")
	var ord struct {
		OrderNo string `json:"order_no"`
	}
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil), b.Token, "sc-"+uniqueKey()),
		http.StatusCreated, "下单", &ord)
	if _, err := admin(t).Exec(context.Background(),
		`UPDATE orders SET status = 20, paid_at = now() WHERE order_no = $1`, ord.OrderNo); err != nil {
		t.Fatal(err)
	}
	// 可售压到 3，restock_plan 才有行、propose 才有意义。
	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 3)

	calls := []struct {
		tool string
		args map[string]any
	}{
		{"shop_overview", map[string]any{"period": "last_7_days"}},
		{"sales_trend", map[string]any{"period": "last_7_days"}},
		{"product_ranking", map[string]any{"period": "last_7_days"}},
		{"store_comparison", map[string]any{"period": "last_7_days"}},
		{"inventory_alerts", map[string]any{}},
		{"search_insights", map[string]any{"period": "last_7_days"}},
		{"restock_plan", map[string]any{"all": true}},
		{"propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU, "delta": 10,
			"reason": "补货", "evidence": "restock_plan：日均 1，可售 3"}},
		{"list_my_proposals", map[string]any{}},
		{"post_brief", map[string]any{"title": "日报", "body": "今天卖了一件", "period_start": "2026-09-27",
			"period_end": "2026-09-27"}},
		{"list_stores", map[string]any{}},
		{"list_products", map[string]any{}},
		{"get_product", map[string]any{"product_id": cs.DressProduct}},
		{"list_refunds", map[string]any{}},
		{"propose_flash_price", map[string]any{"name": "清仓", "items": []map[string]any{{"sku_id": cs.ShirtSKU, "discount_rate": 900}},
			"starts_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"ends_at":   time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339), "evidence": "slow_movers：周转 120 天"}},
		{"propose_coupon", map[string]any{"name": "新券", "coupon_type": 3, "discount_cents": 300, "valid_days": 7,
			"total_count": 10, "per_user_limit": 1, "evidence": "promotion_review：上次券核销率 40%"}},
		{"propose_product_copy", map[string]any{"product_id": cs.ShirtProduct, "subtitle": "纯棉透气",
			"evidence": "search_insights：「纯棉」近 7 天 30 次低点击"}},
	}
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// propose_refund_decision 要一张待审核的售后单，另在 agent_proposal_kinds_test.go 里调；这里只算数。
	if len(tools.Tools) != len(calls)+1 {
		t.Fatalf("工具清单有 %d 个，这条测试调了 %d 个 —— 新工具要加进来", len(tools.Tools), len(calls))
	}
	for _, c := range calls {
		res, _ := mcpCall(t, sess, c.tool, c.args)
		if res.IsError {
			t.Errorf("%s 出错：%s", c.tool, mcpText(res))
		}
	}
	// 出错时 _meta["keel/problem"] 带机器可读的 problem：同一门店同一 SKU 再提一次 → 409。
	res, _ := mcpCall(t, sess, "propose_inventory_adjust", calls[7].args)
	pr, _ := res.Meta["keel/problem"].(map[string]any)
	if !res.IsError || pr == nil || pr["type"] != problem.TypeInvalidRequest || pr["status"] != float64(http.StatusConflict) ||
		pr["title"] == "" {
		t.Errorf("重复提案应 isError 且 _meta.keel/problem 为 409 invalid-request，实得 isError=%v meta=%v", res.IsError, res.Meta)
	}
	for _, tl := range tools.Tools {
		if tl.OutputSchema == nil {
			t.Errorf("%s 没有 outputSchema", tl.Name)
		}
	}
}

// 工具清单（名字、说明、输入与输出的 schema）的快照：docs/AI接口-工具清单.json。
//
// 这是对接入方的兼容承诺（docs/AI接口.md「兼容承诺」）的守门：任何改动都会让这条测试失败，
// 改的人必须显式重生成快照（KEEL_UPDATE_MCP_SNAPSHOT=1），并在 diff 里让审阅者看到改了什么 ——
// 只许加（新工具、新的可选入参、新的出参字段），删字段、改类型、改名要起新工具。
func TestMCPToolListSnapshot(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"快照 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(tools.Tools, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "..", "docs", "AI接口-工具清单.json")
	if os.Getenv("KEEL_UPDATE_MCP_SNAPSHOT") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读快照：%v（第一次跑用 KEEL_UPDATE_MCP_SNAPSHOT=1 生成）", err)
	}
	if string(want) != string(got) {
		t.Fatal(fmt.Sprintf("MCP 工具清单与快照 %s 不一致。确认改动是兼容的（只加不删）后，用 "+
			"KEEL_UPDATE_MCP_SNAPSHOT=1 重新生成并提交，同时更新 docs/AI接口.md 的变更记录", path))
	}
}
