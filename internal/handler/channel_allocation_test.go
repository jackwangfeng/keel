package handler_test

// channel_allocation_review 与只读视图（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §3.3、§4.1）。

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func allocFloat(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }

// 北店：自营卖 3 件连衣裙；假渠道（佣金 15%）接了一张 2 件的单、一张缺货没接成的单；渠道上挂零过 48 小时（规则算 0）
// 与 6 小时（keel 没货）。断言每个渠道的卖出、日均（分母扣挂零）、拒单、净收入，与基线建议。
func TestChannelAllocationReview(t *testing.T) {
	r := newFakeOrderRig(t, nil, map[string]any{"commission_bp": 1500})
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithChannels(r.svc)))
	cs := r.cs
	adminExec(t, `UPDATE skus SET cost_cents = 2000 WHERE id = $1`, cs.DressSKU)
	if _, err := r.svc.UpsertStockRule(r.ctx, repository.ChannelStockRule{BindingID: r.b.ID, RatioBP: 8000}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)

	// 自营 3 件。
	b := cs.newBuyer(t, "alloc")
	var o struct {
		OrderNo      string `json:"order_no"`
		PayableCents int64  `json:"payable_cents"`
	}
	decodeInto(t, createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 3, nil), b.Token, "al-"+uniqueKey()),
		http.StatusCreated, "下单", &o)
	cs.pay(t, o.OrderNo, o.PayableCents)
	// 渠道：接成一张 2 件；一张要的比 keel 有的还多 → 缺货关单。
	r.put(t, "al-1", 1, channel.OrderNew, 2)
	big := int32(r.dressStock(t) + 5)
	r.put(t, "al-2", 1, channel.OrderNew, big)
	if got := adminQueryString(t, `SELECT COALESCE(exception, '') || '|' || COALESCE(order_no, '') FROM channel_orders WHERE id = $1`,
		r.channelOrderID(t, "al-2")); !strings.HasPrefix(got, "缺货：") || !strings.HasSuffix(got, "|") {
		t.Fatalf("夹具不成立：大单应缺货关单，异常|单号 = %q", got)
	}
	// 挂零时段：规则算 0 的 48 小时 + keel 没货的 6 小时（都在 14 天窗口里）。
	adminExec(t, `INSERT INTO channel_listing_zero_spans (merchant_id, binding_id, store_id, sku_id, held, started_at, ended_at)
		VALUES ($1, $2, $3, $4, true, now() - interval '50 hours', now() - interval '2 hours'),
		       ($1, $2, $3, $4, false, now() - interval '10 hours', now() - interval '4 hours')`,
		cs.MerchantID, r.b.ID, cs.NorthStore, cs.DressSKU)
	price := adminQueryInt64(t, `SELECT price_cents FROM sku_prices_by_store WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, cs.DressSKU)
	avail := r.dressStock(t)

	a := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, out := mcpCall(t, sess, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore})
	if res.IsError {
		t.Fatalf("channel_allocation_review 出错：%s", mcpText(res))
	}
	if out["note"] != nil || allocFloat(out, "days") != 14 {
		t.Fatalf("渠道开着、门店接了渠道：不该有 note，days 默认 14：%v", out)
	}
	var dress map[string]any
	for _, s := range out["skus"].([]any) {
		if m := s.(map[string]any); int64(allocFloat(m, "sku_id")) == cs.DressSKU {
			dress = m
		}
	}
	if dress == nil {
		t.Fatalf("自动挑 SKU 应挑到卖过的连衣裙：%v", out["skus"])
	}
	if int64(allocFloat(dress, "available")) != avail || dress["cost_missing"] != false || dress["title"] == "" {
		t.Fatalf("连衣裙的可售 / 成本标记 / 名字不对（可售应 %d）：%v", avail, dress)
	}
	chs := dress["channels"].([]any)
	if len(chs) != 2 {
		t.Fatalf("应有自营 + 假渠道两行：%v", chs)
	}
	self, fake := chs[0].(map[string]any), chs[1].(map[string]any)
	if self["binding_id"] != nil || self["rule"] != nil || allocFloat(self, "sold") != 3 ||
		int64(allocFloat(self, "published_qty")) != avail || int64(allocFloat(self, "unit_net_cents")) != price-2000 {
		t.Fatalf("自营行：卖出 3、对外 = keel 可售 %d、净收入 = 门店价 %d − 成本 2000：%v", avail, price, self)
	}
	if int64(allocFloat(fake, "binding_id")) != r.b.ID || fake["rule_level"] != channel.RuleLevelBinding ||
		allocFloat(fake["rule"].(map[string]any), "ratio_bp") != 8000 {
		t.Fatalf("假渠道行的规则应是渠道级 80%%：%v", fake)
	}
	if allocFloat(fake, "sold") != 2 || allocFloat(fake, "held_zero_hours") != 48 || allocFloat(fake, "empty_zero_hours") != 6 ||
		int64(allocFloat(fake, "stockout_rejects")) != int64(big) {
		t.Fatalf("假渠道：卖出 2、挂零 48 + 6 小时、缺货拒单 %d 件：%v", big, fake)
	}
	// 日均 = 2 ÷ (14 − 54 / 24) = 0.17；净收入 = 渠道价 × 85% − 成本。
	if v := allocFloat(fake, "daily_velocity"); v != math.Round(2/(14-54.0/24)*100)/100 {
		t.Fatalf("假渠道日均 %v，期望分母扣掉挂零的 %v", v, math.Round(2/(14-54.0/24)*100)/100)
	}
	if got, want := int64(allocFloat(fake, "unit_net_cents")), int64(math.Round(float64(price)*0.85))-2000; got != want {
		t.Fatalf("假渠道单件净收入 %d，期望 %d", got, want)
	}
	if want := int64(math.Floor(float64(avail) * 0.8)); int64(allocFloat(fake, "published_qty")) != want {
		t.Fatalf("假渠道对外可售应是 floor(%d × 80%%) = %d：%v", avail, want, fake)
	}
	// 建议：挂零 48 小时且有卖 → 比例 90%；14 天 big 件拒单 → 安全库存 +ceil(big / 2)。
	sug := dress["suggestions"].([]any)
	if len(sug) != 1 {
		t.Fatalf("应有一条给假渠道的建议：%v", sug)
	}
	s := sug[0].(map[string]any)
	if int64(allocFloat(s, "binding_id")) != r.b.ID || int64(allocFloat(s, "sku_id")) != cs.DressSKU ||
		allocFloat(s, "ratio_bp") != 9000 || int64(allocFloat(s, "safety_qty")) != int64(math.Ceil(float64(big)/2)) || s["why"] == "" {
		t.Fatalf("建议不对：%v", s)
	}

	// 没填成本价：标出来。
	adminExec(t, `UPDATE skus SET cost_cents = 0 WHERE id = $1`, cs.DressSKU)
	_, out = mcpCall(t, sess, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore, "sku_ids": []int64{cs.DressSKU}})
	if d := out["skus"].([]any)[0].(map[string]any); d["cost_missing"] != true {
		t.Fatalf("成本价 0 应标 cost_missing：%v", d)
	}

	// keel 自己没货：不给建议（Review Focus 4）。
	adjust(t, r.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -int32(avail))
	r.drain(t)
	_, out = mcpCall(t, sess, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore, "sku_ids": []int64{cs.DressSKU}})
	if d := out["skus"].([]any)[0].(map[string]any); len(d["suggestions"].([]any)) != 0 {
		t.Fatalf("keel 可售 0 不该有建议：%v", d)
	}

	// 入参越界。
	if res, _ := mcpCall(t, sess, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore, "days": 3}); !res.IsError {
		t.Fatal("days = 3 应被拒")
	}

	// 门店范围的 AI 员工：自己的店可以，别的店 out-of-scope（判权同 slow_movers）。
	sa := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	ss := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, sa.Id, `{"name":"t"}`).Secret)
	if res, _ := mcpCall(t, ss, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore}); res.IsError {
		t.Fatalf("门店管理员看自己的店应可以：%s", mcpText(res))
	}
	if res, _ := mcpCall(t, ss, "channel_allocation_review", map[string]any{"store_id": cs.SouthStore}); !res.IsError ||
		!strings.Contains(mcpText(res), "out-of-scope") {
		t.Fatalf("门店管理员看别的店应 out-of-scope：%q", mcpText(res))
	}

	// query_sql 读得到订单来源与渠道视图；视图不含凭据、配置、收货人（Review Focus 5）。
	cols := func(sql string) []string {
		t.Helper()
		res, out := mcpCall(t, sess, "query_sql", map[string]any{"sql": sql})
		if res.IsError {
			t.Fatalf("query_sql %q 出错：%s", sql, mcpText(res))
		}
		var cs []string
		for _, c := range out["columns"].([]any) {
			cs = append(cs, c.(string))
		}
		return cs
	}
	for _, c := range []string{"secrets", "config"} {
		if slices.Contains(cols("SELECT * FROM channel_bindings"), c) {
			t.Fatalf("agent_ro.channel_bindings 不该有 %s 列", c)
		}
	}
	for _, c := range []string{"receiver", "last_payload", "lines", "rider", "exception"} {
		if slices.Contains(cols("SELECT * FROM channel_orders"), c) {
			t.Fatalf("agent_ro.channel_orders 不该有 %s 列", c)
		}
	}
	if got := cols("SELECT * FROM orders LIMIT 1"); !slices.Contains(got, "source") || !slices.Contains(got, "channel_order_id") {
		t.Fatalf("agent_ro.orders 应有 source 与 channel_order_id：%v", got)
	}
	_, n := mcpCall(t, sess, "query_sql", map[string]any{"sql": "SELECT count(*) FROM orders WHERE source = 1 AND status = 20"})
	if rows := n["rows"].([]any); len(rows) != 1 || rows[0].([]any)[0].(float64) != 1 {
		t.Fatalf("按 source 数渠道单（接成的 1 张）：%v", n)
	}
	for _, v := range []string{"channel_stock_rules", "channel_listing_zero_spans"} {
		if res, _ := mcpCall(t, sess, "query_sql", map[string]any{"sql": "SELECT * FROM " + v}); res.IsError {
			t.Fatalf("query_sql 读 %s 出错：%s", v, mcpText(res))
		}
	}
}

// KEEL_CHANNELS 关着（默认的测试引擎不带渠道层）：返回空结果与说明，不报错。
func TestChannelAllocationReviewChannelsOff(t *testing.T) {
	cs := newCouponShop(t)
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}))
	a := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, out := mcpCall(t, sess, "channel_allocation_review", map[string]any{"store_id": cs.NorthStore})
	if res.IsError {
		t.Fatalf("渠道关着不该报错：%s", mcpText(res))
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "没有启用的销售渠道") || len(out["skus"].([]any)) != 0 {
		t.Fatalf("渠道关着应返回空结果与「没有启用的销售渠道」：%v", out)
	}
}
