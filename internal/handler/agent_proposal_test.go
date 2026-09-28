package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AI 员工的提案（AI 经营 M9 任务 4，docs/AI经营-M9设计.md §0 验收 4、§4）。

func TestAgentProposalLifecycle(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"AI 店长","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	before := availableAt(t, cs.NorthStore, cs.DressSKU)

	args := map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU, "delta": 40, "reason": "三天后卖断",
		"evidence": "restock_plan：日均 6 件，可售 4，预计 2026-09-29 卖断", "expected_impact": "覆盖到 10 月中"}
	res, p := mcpCall(t, sess, "propose_inventory_adjust", args)
	if res.IsError || p["status"].(float64) != 10 {
		t.Fatalf("提案：%s %v", mcpText(res), p)
	}
	pid := int64(p["id"].(float64))
	if !strings.Contains(p["title"].(string), "北京门店") || !strings.Contains(p["title"].(string), "补 40 件") {
		t.Errorf("提案标题：%q", p["title"])
	}
	// 同一门店同一 SKU 还有待处理的：再提被拒，并告诉它是哪一条。
	res, _ = mcpCall(t, sess, "propose_inventory_adjust", args)
	if !res.IsError || !strings.Contains(mcpText(res), fmt.Sprintf("#%d", pid)) {
		t.Fatalf("重复提案应被拒并指出 #%d：%q", pid, mcpText(res))
	}
	// 提案不执行：库存没动。
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("提案还没批，库存就变了：%d → %d", before, got)
	}

	// 人批准 → 以 AI 员工的身份执行，结果写回。
	var done api.AgentProposal
	decodeInto(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", pid), "", cs.Token),
		http.StatusOK, "批准", &done)
	if done.Status != 20 || done.Result == nil || (*done.Result)["after_available"].(float64) != float64(before+40) ||
		(*done.Result)["before_available"].(float64) != float64(before) || done.DecidedBy == nil {
		t.Fatalf("批准后：status=%d result=%v", done.Status, done.Result)
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before+40 {
		t.Fatalf("批准后库存 %d，期望 %d", got, before+40)
	}
	// 流水里写着是哪条提案、由 AI 员工执行。
	var reason string
	if err := admin(t).QueryRow(context.Background(), `
		SELECT reason FROM inventory_logs WHERE sku_id = $1 AND store_id = $2 AND biz_type = 5 ORDER BY id DESC LIMIT 1`,
		cs.DressSKU, cs.NorthStore).Scan(&reason); err != nil || !strings.Contains(reason, fmt.Sprintf("AI 提案 #%d", pid)) {
		t.Fatalf("库存流水的原因 %q（err=%v）", reason, err)
	}
	// 再批一次：409 proposal-not-open，库存不再加。
	if p := problemOf(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", pid), "", cs.Token),
		http.StatusConflict); p.Type != problem.TypeProposalNotOpen {
		t.Fatalf("重复批准：%s", p.Type)
	}

	// 驳回：理由回给 AI 员工。
	_, p2 := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.SouthStore, "sku_id": cs.ShirtSKU,
		"delta": 10, "reason": "试试", "evidence": "restock_plan 说广州衬衫要补"})
	pid2 := int64(p2["id"].(float64))
	var rej api.AgentProposal
	decodeInto(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/reject", pid2), `{"reason":"国庆后再说"}`, cs.Token),
		http.StatusOK, "驳回", &rej)
	if rej.Status != 30 || rej.RejectReason == nil || *rej.RejectReason != "国庆后再说" {
		t.Fatalf("驳回后：%+v", rej)
	}
	_, mine := mcpCall(t, sess, "list_my_proposals", map[string]any{"status": 30})
	items, _ := mine["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["reject_reason"] != "国庆后再说" {
		t.Fatalf("list_my_proposals 看不到驳回理由：%v", mine)
	}
	// 后台列表：待处理在前（这里都处理完了），两条都在。
	var list struct {
		Items []api.AgentProposal `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, "/api/v1/admin/agent-proposals", cs.Token), http.StatusOK, "提案列表", &list)
	if len(list.Items) != 2 {
		t.Fatalf("提案列表 %d 条，期望 2", len(list.Items))
	}
}

// 判权两道：提案之后 AI 员工的范围被收窄（改成只管广州门店），批准时按它现在的身份判 → 执行失败，库存不动；
// AI 员工被停用 → 执行失败（agent-disabled）。
func TestAgentProposalExecutesAsTheAgentNow(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	propose := func(store, sku int64) int64 {
		res, p := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": store, "sku_id": sku,
			"delta": 5, "reason": "补货", "evidence": "restock_plan 显示需要补 5 件"})
		if res.IsError {
			t.Fatalf("提案：%s", mcpText(res))
		}
		return int64(p["id"].(float64))
	}
	p1 := propose(cs.NorthStore, cs.DressSKU)
	p2 := propose(cs.NorthStore, cs.ShirtSKU)
	before := availableAt(t, cs.NorthStore, cs.DressSKU)

	wantStatus(t, patchAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id),
		fmt.Sprintf(`{"role":4,"store_ids":[%d]}`, cs.SouthStore), cs.Token), http.StatusOK, "收窄范围")
	var out api.AgentProposal
	decodeInto(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", p1), "", cs.Token),
		http.StatusOK, "批准", &out)
	if out.Status != 40 || out.Result == nil || (*out.Result)["error_type"] != "out-of-scope" {
		t.Fatalf("范围收窄后批准应执行失败（out-of-scope）：status=%d result=%v", out.Status, out.Result)
	}
	if got := availableAt(t, cs.NorthStore, cs.DressSKU); got != before {
		t.Fatalf("执行失败了库存却变了：%d → %d", before, got)
	}

	wantStatus(t, patchAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agents/%d", a.Id), `{"status":2}`, cs.Token), http.StatusOK, "停用")
	decodeInto(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", p2), "", cs.Token),
		http.StatusOK, "批准", &out)
	if out.Status != 40 || (*out.Result)["error_type"] != "agent-disabled" {
		t.Fatalf("AI 员工停用后批准应执行失败（agent-disabled）：%v", out.Result)
	}
}

// 越权提案被拒（与人一样）；过期的提案不能再批。
func TestAgentProposalScopeAndExpiry(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, _ := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.SouthStore, "sku_id": cs.DressSKU,
		"delta": 5, "reason": "越权", "evidence": "想给广州门店补货试试"})
	if !res.IsError || !strings.Contains(mcpText(res), "out-of-scope") {
		t.Fatalf("门店管理员身份的 AI 员工给别的门店提案应 out-of-scope：%q", mcpText(res))
	}
	res, p := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU,
		"delta": 5, "reason": "补货", "evidence": "restock_plan 显示需要补 5 件"})
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	pid := int64(p["id"].(float64))
	if _, err := admin(t).Exec(context.Background(), `UPDATE agent_proposals SET expires_at = now() - interval '1 minute' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExpireProposalsOnce(context.Background(), repository.New(testPool)); err != nil {
		t.Fatal(err)
	}
	var st int
	if err := admin(t).QueryRow(context.Background(), `SELECT status FROM agent_proposals WHERE id = $1`, pid).Scan(&st); err != nil || st != 50 {
		t.Fatalf("过期扫描后状态 %d（err=%v），期望 50", st, err)
	}
	if p := problemOf(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", pid), "", cs.Token),
		http.StatusConflict); p.Type != problem.TypeProposalNotOpen {
		t.Fatalf("过期的提案批准：%s", p.Type)
	}
}

// 简报（M9 任务 5）：AI 员工写，管理员在后台看得到；门店管理员看不到（全店口径）；参数错被拒。
func TestAgentBriefs(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"AI 店长","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, b := mcpCall(t, sess, "post_brief", map[string]any{"title": "9 月 28 日巡店日报",
		"body": "## 概览\n- 销售额 ¥1,234.00\n<script>alert(1)</script>", "period_start": "2026-09-28", "period_end": "2026-09-28"})
	if res.IsError {
		t.Fatalf("post_brief：%s", mcpText(res))
	}
	bid := int64(b["id"].(float64))
	var got api.AgentBrief
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-briefs/%d", bid), cs.Token), http.StatusOK, "简报详情", &got)
	if got.Title != "9 月 28 日巡店日报" || !strings.Contains(got.Body, "<script>") || got.AgentName != "AI 店长" {
		t.Fatalf("简报详情：%+v", got)
	}
	res, _ = mcpCall(t, sess, "post_brief", map[string]any{"title": "x", "body": "y", "period_start": "2026-09-28", "period_end": "2026-09-01"})
	if !res.IsError || !strings.Contains(mcpText(res), "invalid-request") {
		t.Fatalf("起晚于止的简报应被拒：%q", mcpText(res))
	}
}

// 工具输出里的时刻是店铺当地时间（没设过时区 = Asia/Shanghai，+08:00），不是 UTC。
// 2026-09-28 演示站实跑：AI 店长把 UTC 的 07:57 当成北京时间写进了简报（应是 15:57）。
func TestMCPTimesAreInShopTimezone(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, p := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU,
		"delta": 5, "reason": "补货", "evidence": "restock_plan 显示需要补 5 件"})
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	for _, k := range []string{"created_at", "expires_at"} {
		v, _ := p[k].(string)
		if !strings.HasSuffix(v, "+08:00") {
			t.Fatalf("%s = %q，期望店铺时区（+08:00）", k, v)
		}
	}
	// 店铺时区改成 UTC 后跟着变。
	adminExec(t, `INSERT INTO shop_preferences (merchant_id, timezone) VALUES ($1, 'UTC')
		ON CONFLICT (merchant_id) DO UPDATE SET timezone = EXCLUDED.timezone`, cs.MerchantID)
	_, l := mcpCall(t, sess, "list_my_proposals", map[string]any{})
	items, _ := l["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("list_my_proposals 没有返回提案：%v", l)
	}
	if v, _ := items[0].(map[string]any)["created_at"].(string); !strings.HasSuffix(v, "Z") {
		t.Fatalf("店铺时区 UTC 时 created_at = %q，期望以 Z 结尾", v)
	}
}
