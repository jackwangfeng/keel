package handler_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 跨商家隔离的回归（2026-09-28 补）：AI 员工密钥、以及当天新加的几样（多收款退回、简报更正、MCP 店铺时区）
// 都必须只在自己那家店里生效。租户由 Host 定，数据由行级安全兜底；这些测试钉的是「接口层没有哪一步绕过了它」。

// AI 员工的接入密钥只在签发它的那家店有效：拿到别家店的域名上，whoami 与 MCP 都是 401（与密钥不存在同一个响应）。
func TestAgentKeyOnlyWorksInItsOwnShop(t *testing.T) {
	a, b := newAdminShop(t), newAdminShop(t)
	agent := createAgent(t, a, `{"name":"A 店 AI","role":2}`)
	key := issueAgentKey(t, a, agent.Id, `{"name":"k"}`).Secret

	wantStatus(t, getAs(t, a.Host, "/api/v1/agent/whoami", key), http.StatusOK, "本店 whoami")
	wantStatus(t, getAs(t, b.Host, "/api/v1/agent/whoami", key), http.StatusUnauthorized, "别家店 whoami")

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shop_overview","arguments":{}}}`
	w := postJSON(t, b.Host, "/api/v1/mcp", body, key, map[string]string{"Accept": "application/json, text/event-stream"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("A 店的密钥在 B 店调 MCP 应 401，实得 %d：%s", w.Code, w.Body.String())
	}
}

// 后台「多收款退回」只列本店的：B 店的管理员看不到 A 店的退回单。
func TestPaymentReturnsAreScopedToTheShop(t *testing.T) {
	a, b := newCouponShop(t), newCouponShop(t)
	buyer := a.newBuyer(t, "pr-iso")
	o := a.twoLineOrder(t, buyer, nil)
	a.pay(t, o.OrderNo, o.PayableCents)
	again := payload(o.OrderNo, "iso-again-"+o.OrderNo, o.PayableCents)
	wantStatus(t, notifyPaymentSigned(t, a.Host, "wechat", again, sign(couponWebhookSecret(a.adminShop), []byte(again))),
		http.StatusOK, "A 店重复付款")

	wa := getAs(t, a.Host, "/api/v1/admin/payment-returns", a.Token)
	if wa.Code != http.StatusOK || !strings.Contains(wa.Body.String(), o.OrderNo) {
		t.Fatalf("A 店自己应看得到：%d %s", wa.Code, wa.Body.String())
	}
	wb := getAs(t, b.Host, "/api/v1/admin/payment-returns", b.Token)
	if wb.Code != http.StatusOK || strings.Contains(wb.Body.String(), o.OrderNo) {
		t.Fatalf("B 店不该看到 A 店的退回单：%d %s", wb.Code, wb.Body.String())
	}
	// A 店的管理员令牌拿到 B 店的域名上：不认。
	if w := getAs(t, b.Host, "/api/v1/admin/payment-returns", a.Token); w.Code == http.StatusOK {
		t.Fatalf("A 店的后台会话在 B 店域名上不该 200：%s", w.Body.String())
	}
}

// 简报更正只能指向本店（且自己写）的简报：指向别家店的简报按「不存在」拒，不泄露那份简报存在与否。
func TestBriefCorrectionCannotReachAnotherShop(t *testing.T) {
	a, b := newAdminShop(t), newAdminShop(t)
	aa := createAgent(t, a, `{"name":"A 店 AI","role":2}`)
	ba := createAgent(t, b, `{"name":"B 店 AI","role":2}`)
	sa := mcpConnect(t, a.Host, issueAgentKey(t, a, aa.Id, `{"name":"k"}`).Secret)
	sessB := mcpConnect(t, b.Host, issueAgentKey(t, b, ba.Id, `{"name":"k"}`).Secret)
	brief := map[string]any{"title": "B 店日报", "body": "今天卖了一件", "period_start": "2026-09-28", "period_end": "2026-09-28"}
	res, bBrief := mcpCall(t, sessB, "post_brief", brief)
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	res, _ = mcpCall(t, sa, "post_brief", map[string]any{"title": "更正", "body": "改一下", "period_start": "2026-09-28",
		"period_end": "2026-09-28", "corrects_brief_id": bBrief["id"]})
	if !res.IsError || !strings.Contains(mcpText(res), "不存在") {
		t.Fatalf("A 店的 AI 更正 B 店的简报应按不存在拒：%q", mcpText(res))
	}
}

// MCP 输出的时刻按**各自店铺**的时区改写：A 店设了 UTC、B 店用默认（Asia/Shanghai），两边互不影响。
func TestMCPTimezoneIsPerShop(t *testing.T) {
	a, b := newCouponShop(t), newCouponShop(t)
	adminExec(t, `INSERT INTO shop_preferences (merchant_id, timezone) VALUES ($1, 'UTC')
		ON CONFLICT (merchant_id) DO UPDATE SET timezone = EXCLUDED.timezone`, a.MerchantID)
	check := func(cs couponShop, suffix string) {
		t.Helper()
		ag := createAgent(t, cs.adminShop, `{"name":"AI","role":2}`)
		sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, ag.Id, `{"name":"k"}`).Secret)
		res, p := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU,
			"delta": 5, "reason": "补货", "evidence": "restock_plan 显示需要补 5 件"})
		if res.IsError {
			t.Fatal(mcpText(res))
		}
		if v, _ := p["created_at"].(string); !strings.HasSuffix(v, suffix) {
			t.Fatalf("%s 店的 created_at = %q，期望以 %s 结尾", fmt.Sprint(cs.MerchantID), v, suffix)
		}
	}
	check(a, "Z")
	check(b, "+08:00")
}
