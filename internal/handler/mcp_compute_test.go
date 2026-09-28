package handler_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// slow_movers（AI 经营 M10 计算工具）：北京门店连衣裙从没卖出去过（周转天数 null，排最前），
// 衬衫卖了 5 件（有一个有限的周转天数，排第二）。门店管理员身份的 AI 员工点名要广州门店：
// 与人一样 out-of-scope（判权与 restock_plan 逐字相同）。
func TestMCPSlowMovers(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "slow")
	w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 5, nil), b.Token, "sm-"+uniqueKey())
	var o struct {
		OrderNo      string `json:"order_no"`
		PayableCents int64  `json:"payable_cents"`
	}
	decodeInto(t, w, http.StatusCreated, "下单", &o)
	cs.pay(t, o.OrderNo, o.PayableCents)

	a := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)

	res, out := mcpCall(t, sess, "slow_movers", map[string]any{"store_id": cs.NorthStore})
	if res.IsError {
		t.Fatalf("slow_movers 出错：%s", mcpText(res))
	}
	lines, _ := out["lines"].([]any)
	if len(lines) < 2 {
		t.Fatalf("北京门店两个 SKU 可售都够 min_available，应该都出现：%v", lines)
	}
	for _, l := range lines {
		m := l.(map[string]any)
		if int64(m["store_id"].(float64)) != cs.NorthStore {
			t.Fatalf("门店管理员身份的 AI 员工拿到了别的门店的滞销行：%v", m)
		}
	}
	// 从没卖出去过的排最前：第一条应是连衣裙，days_of_stock 是 null。
	first := lines[0].(map[string]any)
	if int64(first["sku_id"].(float64)) != cs.DressSKU {
		t.Fatalf("从没卖出去过的连衣裙应排最前：%v", lines)
	}
	if _, ok := first["days_of_stock"]; ok {
		t.Fatalf("连衣裙从没卖出去过，days_of_stock 应是 null（省略）：%v", first)
	}
	if first["sold"].(float64) != 0 || first["daily_avg"].(float64) != 0 {
		t.Fatalf("连衣裙不该有销量：%v", first)
	}
	// 衬衫卖了 5 件，应该有一个有限的 days_of_stock。
	var shirt map[string]any
	for _, l := range lines {
		m := l.(map[string]any)
		if int64(m["sku_id"].(float64)) == cs.ShirtSKU {
			shirt = m
		}
	}
	if shirt == nil {
		t.Fatalf("衬衫应该出现在滞销清单里：%v", lines)
	}
	if shirt["sold"].(float64) != 5 {
		t.Fatalf("衬衫已售应为 5：%v", shirt)
	}
	if _, ok := shirt["days_of_stock"]; !ok {
		t.Fatalf("衬衫有销量，days_of_stock 不该是 null：%v", shirt)
	}

	// 门店管理员身份的 AI 员工点名要广州门店：与人一样 out-of-scope。
	res, _ = mcpCall(t, sess, "slow_movers", map[string]any{"store_id": cs.SouthStore})
	if !res.IsError || !strings.Contains(mcpText(res), "out-of-scope") {
		t.Fatalf("点名要广州门店应 out-of-scope：%q", mcpText(res))
	}

	// min_available 收紧到可售数以上：两个 SKU 都不该出现。
	_, out2 := mcpCall(t, sess, "slow_movers", map[string]any{"store_id": cs.NorthStore, "min_available": 1000})
	if lines2, _ := out2["lines"].([]any); len(lines2) != 0 {
		t.Fatalf("min_available 收紧之后不该有结果：%v", lines2)
	}
}

// promotion_review（AI 经营 M10 计算工具）：一个限时折扣活动带了 2 单、5 件连衣裙；
// 一张满减券带了 1 单、减了 2000 分。都在只读判权（全店范围）之下，二选一的入参错了要拒。
func TestMCPPromotionReview(t *testing.T) {
	cs := newCouponShop(t)
	promo := cs.livePromotion(t, "复盘活动",
		fmt.Sprintf(`"promotion_type":3,"skus":[{"sku_id":%d,"promo_price_cents":5500}]`, cs.DressSKU))

	b := cs.newBuyer(t, "review")
	var totalPaid, totalUnits int64
	for _, qty := range []int{2, 3} {
		w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, qty, nil), b.Token, "pr-"+uniqueKey())
		var o struct {
			OrderNo      string `json:"order_no"`
			PayableCents int64  `json:"payable_cents"`
		}
		decodeInto(t, w, http.StatusCreated, "下单", &o)
		cs.pay(t, o.OrderNo, o.PayableCents)
		totalPaid += o.PayableCents
		totalUnits += int64(qty)
	}

	// 券模板独立于活动：走衬衫（不是活动参与 SKU），免得两个分支的订单互相沾边。
	tpl := cs.createTemplate(t, full100minus20(0, 1))
	wantStatus(t, cs.patchTemplate(t, tpl.Id, `{"claimable":true}`), http.StatusOK, "设为可领")
	uc := cs.mustClaim(t, b, tpl.Id)
	w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.ShirtSKU, 2, &uc.Id), b.Token, "cp-"+uniqueKey())
	var co struct {
		OrderNo        string `json:"order_no"`
		PayableCents   int64  `json:"payable_cents"`
		DiscountCents  int64  `json:"discount_cents"`
	}
	decodeInto(t, w, http.StatusCreated, "下单（带券）", &co)
	cs.pay(t, co.OrderNo, co.PayableCents)

	a := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)

	res, out := mcpCall(t, sess, "promotion_review", map[string]any{"promotion_id": promo.Id})
	if res.IsError {
		t.Fatalf("promotion_review（活动）出错：%s", mcpText(res))
	}
	p, _ := out["promotion"].(map[string]any)
	if p == nil {
		t.Fatalf("应返回 promotion 分支：%v", out)
	}
	if p["shop_wide"] != false {
		t.Fatalf("限时折扣不是全店口径：%v", p)
	}
	cur, _ := p["current"].(map[string]any)
	if cur == nil || int64(cur["order_count"].(float64)) != 2 {
		t.Fatalf("当前窗口应有 2 单：%v", cur)
	}
	if int64(cur["sales_cents"].(float64)) != totalPaid {
		t.Fatalf("当前窗口销售额应是两单实付合计 %d：%v", totalPaid, cur)
	}
	if int64(cur["units_sold"].(float64)) != totalUnits {
		t.Fatalf("当前窗口参与 SKU 销量应是 %d：%v", totalUnits, cur)
	}
	prev, _ := p["previous"].(map[string]any)
	if prev == nil || int64(prev["order_count"].(float64)) != 0 {
		t.Fatalf("活动刚建，前一个窗口不该有订单：%v", prev)
	}

	res, out = mcpCall(t, sess, "promotion_review", map[string]any{"coupon_template_id": tpl.Id})
	if res.IsError {
		t.Fatalf("promotion_review（券）出错：%s", mcpText(res))
	}
	c, _ := out["coupon"].(map[string]any)
	if c == nil {
		t.Fatalf("应返回 coupon 分支：%v", out)
	}
	if int64(c["claimed_count"].(float64)) != 1 || int64(c["used_count"].(float64)) != 1 {
		t.Fatalf("这张券领了 1 张、核销了 1 张：%v", c)
	}
	if c["use_rate"].(float64) != 1 {
		t.Fatalf("核销率应是 1：%v", c)
	}
	if int64(c["order_count"].(float64)) != 1 || int64(c["sales_cents"].(float64)) != co.PayableCents {
		t.Fatalf("这张券带来 1 单，销售额应是它的实付 %d：%v", co.PayableCents, c)
	}
	if int64(c["discount_cents"].(float64)) != co.DiscountCents {
		t.Fatalf("这张券让出的优惠应是 %d：%v", co.DiscountCents, c)
	}

	// 二选一的入参：都不给、都给，都是 invalid-request。
	res, _ = mcpCall(t, sess, "promotion_review", map[string]any{})
	if !res.IsError || !strings.Contains(mcpText(res), "invalid-request") {
		t.Fatalf("promotion_id 与 coupon_template_id 都不给应 invalid-request：%q", mcpText(res))
	}
	res, _ = mcpCall(t, sess, "promotion_review",
		map[string]any{"promotion_id": promo.Id, "coupon_template_id": tpl.Id})
	if !res.IsError || !strings.Contains(mcpText(res), "invalid-request") {
		t.Fatalf("两个都给应 invalid-request：%q", mcpText(res))
	}

	// 门店管理员身份的 AI 员工看不到全店经营信息：role-forbidden。
	sa := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	storeSess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, sa.Id, `{"name":"t"}`).Secret)
	res, _ = mcpCall(t, storeSess, "promotion_review", map[string]any{"promotion_id": promo.Id})
	if !res.IsError || !strings.Contains(mcpText(res), "role-forbidden") {
		t.Fatalf("门店管理员身份看活动 / 券复盘应 role-forbidden：%q", mcpText(res))
	}
}
