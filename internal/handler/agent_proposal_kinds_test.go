package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
)

// M10 的四种提案：AI 员工经 MCP 提 → 人在后台批准 → Keel 以 AI 员工身份执行，结果写回提案。
//   - 限时折扣：活动建好、上线，连衣裙 85 折；
//   - 发券：券模板建好；
//   - 改文案：商品标题改了（执行结果带前后标题）；
//   - 售后审核：待审核的售后单被同意。
//
// 另外：门店管理员身份的 AI 员工提不了全店类提案（营销、商品）；打五折以下的限时折扣在提案层就被拒。
func TestAgentProposalKindsExecuteOnApproval(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"运营 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)
	approve := func(id float64) api.AgentProposal {
		t.Helper()
		var out api.AgentProposal
		decodeInto(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", int64(id)), "",
			cs.Token, freshIdemKey()), http.StatusOK, "批准", &out)
		if out.Status != 20 {
			t.Fatalf("提案 #%d 应已执行（20），实得 %d：%+v", out.Id, out.Status, out.Result)
		}
		return out
	}
	propose := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		res, out := mcpCall(t, sess, tool, args)
		if res.IsError {
			t.Fatalf("%s 出错：%s", tool, mcpText(res))
		}
		return out
	}
	ev := "近 30 天 slow_movers：可售 120 件、日均 0.8，周转 150 天"

	// 限时折扣
	start := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	end := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	fp := propose("propose_flash_price", map[string]any{"name": "换季清仓", "items": []map[string]any{
		{"sku_id": cs.DressSKU, "discount_rate": 850}}, "starts_at": start, "ends_at": end, "evidence": ev})
	if fp["store_id"] != nil || fp["kind"] != "flash_price" {
		t.Fatalf("限时折扣是全店类提案：%v", fp)
	}
	out := approve(fp["id"].(float64))
	pid := int64((*out.Result)["detail"].(map[string]any)["promotion_id"].(float64))
	var st int
	if err := admin(t).QueryRow(context.Background(), `SELECT status FROM promotions WHERE id = $1`, pid).Scan(&st); err != nil || st != 1 {
		t.Fatalf("批准后活动 %d 应已上线：status=%d err=%v", pid, st, err)
	}

	// 发券
	cp := propose("propose_coupon", map[string]any{"name": "回头客 5 元券", "coupon_type": 1, "threshold_cents": 5000,
		"discount_cents": 500, "valid_days": 7, "total_count": 100, "per_user_limit": 1, "evidence": ev})
	out = approve(cp["id"].(float64))
	tid := int64((*out.Result)["detail"].(map[string]any)["coupon_template_id"].(float64))
	var name string
	if err := admin(t).QueryRow(context.Background(), `SELECT name FROM coupon_templates WHERE id = $1`, tid).Scan(&name); err != nil ||
		name != "回头客 5 元券" {
		t.Fatalf("批准后券模板应已建好：%q err=%v", name, err)
	}

	// 发券（固定时段）：只在指定的几天里能用 —— 2026-09-28 演示站店长要「国庆 10/1–10/7 可用」时做不到。
	vs := time.Now().Add(48 * time.Hour).Truncate(time.Hour)
	ve := vs.Add(7 * 24 * time.Hour)
	fx := propose("propose_coupon", map[string]any{"name": "节日满减", "coupon_type": 1, "threshold_cents": 19900,
		"discount_cents": 2000, "valid_start_at": vs.Format(time.RFC3339), "valid_end_at": ve.Format(time.RFC3339),
		"total_count": 200, "per_user_limit": 1, "evidence": ev})
	fid := fx["id"].(float64)
	out = approve(fid)
	ftid := int64((*out.Result)["detail"].(map[string]any)["coupon_template_id"].(float64))
	var mode int16
	var gs, ge time.Time
	if err := admin(t).QueryRow(context.Background(), `SELECT valid_mode, valid_start_at, valid_end_at FROM coupon_templates WHERE id = $1`,
		ftid).Scan(&mode, &gs, &ge); err != nil || mode != 1 || !gs.Equal(vs) || !ge.Equal(ve) {
		t.Fatalf("固定时段的券：valid_mode=%d %v–%v（期望 1 %v–%v） err=%v", mode, gs, ge, vs, ve, err)
	}
	var dueOK bool
	if err := admin(t).QueryRow(context.Background(), `SELECT outcome_due_at = $2::timestamptz + interval '1 day' FROM agent_proposals WHERE id = $1`,
		int64(fid), ve).Scan(&dueOK); err != nil || !dueOK {
		t.Fatalf("固定时段的券应在结束后一天复盘：%v %v", dueOK, err)
	}
	if res, _ := mcpCall(t, sess, "propose_coupon", map[string]any{"name": "两个都给", "coupon_type": 3, "discount_cents": 300,
		"valid_days": 7, "valid_start_at": vs.Format(time.RFC3339), "valid_end_at": ve.Format(time.RFC3339),
		"total_count": 10, "per_user_limit": 1, "evidence": ev}); !res.IsError || !strings.Contains(mcpText(res), "二选一") {
		t.Fatalf("valid_days 与固定时段同时给应被拒：%q", mcpText(res))
	}

	// 改文案
	pc := propose("propose_product_copy", map[string]any{"product_id": cs.DressProduct, "title": "法式碎花连衣裙 夏季",
		"evidence": "search_insights：「碎花裙」近 7 天搜索 40 次、0 结果"})
	out = approve(pc["id"].(float64))
	var title string
	if err := admin(t).QueryRow(context.Background(), `SELECT title FROM products WHERE id = $1`, cs.DressProduct).Scan(&title); err != nil ||
		title != "法式碎花连衣裙 夏季" {
		t.Fatalf("批准后标题应已改：%q err=%v", title, err)
	}
	if (*out.Result)["detail"].(map[string]any)["before_title"] == "" {
		t.Fatal("执行结果应带原标题")
	}

	// 售后审核：买家申请一张退款，AI 建议同意，人批准。
	b := cs.newBuyer(t, "ai-refund")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	rf := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1}))
	rd := propose("propose_refund_decision", map[string]any{"refund_no": rf.RefundNo, "action": "approve",
		"evidence": "未发货、买家 10 分钟内申请，理由是拍错尺码"})
	if int64(rd["store_id"].(float64)) != cs.NorthStore {
		t.Fatalf("售后审核提案应落在订单的履约门店：%v", rd["store_id"])
	}
	approve(rd["id"].(float64))
	if got := buyerRefund(t, cs, b, rf.RefundNo); got.Status == 10 {
		t.Fatalf("批准后售后单不该还是待审核：%+v", got.Status)
	}

	// 打四折：提案层就拒。
	res, _ := mcpCall(t, sess, "propose_flash_price", map[string]any{"name": "太狠", "items": []map[string]any{
		{"sku_id": cs.ShirtSKU, "discount_rate": 400}}, "starts_at": start, "ends_at": end, "evidence": ev})
	if !res.IsError {
		t.Fatal("打四折的限时折扣应在提案层被拒")
	}

	// 门店管理员身份的 AI 员工提不了全店类提案。
	sa := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	sk := issueAgentKey(t, cs.adminShop, sa.Id, `{"name":"test"}`)
	ss := mcpConnect(t, cs.Host, sk.Secret)
	res, _ = mcpCall(t, ss, "propose_coupon", map[string]any{"name": "门店券", "coupon_type": 3, "discount_cents": 300,
		"valid_days": 7, "total_count": 10, "per_user_limit": 1, "evidence": ev})
	if !res.IsError {
		t.Fatal("门店管理员身份的 AI 员工不该能提发券提案")
	}
}

// 批准时目标已经变了（售后单已被人工处理）：记成执行失败（40）并写明原因，不是 500、不卡在 15。
// 2026-09-28 破坏性测试：批准 500、提案永久 15，人工驳回 409，同一目标再也提不了。
func TestProposalWhoseTargetChangedFailsInsteadOfSticking(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"售后 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	b := cs.newBuyer(t, "stale-target")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	rf := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1}))
	res, p := mcpCall(t, sess, "propose_refund_decision", map[string]any{"refund_no": rf.RefundNo, "action": "approve",
		"evidence": "list_refunds：未发货、仅退款、理由是拍错尺码"})
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	// 人先处理掉：驳回。
	wantStatus(t, cs.audit(t, rf.RefundNo, `{"action":"reject","reject_reason":"已与买家电话沟通，改为换货"}`), http.StatusOK, "人工驳回")

	var out api.AgentProposal
	decodeInto(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", int64(p["id"].(float64))), "",
		cs.Token, freshIdemKey()), http.StatusOK, "批准一条目标已变的提案", &out)
	if out.Status != 40 || out.Result == nil || (*out.Result)["error"] == nil {
		t.Fatalf("目标已变应记成执行失败 40 并写明原因：status=%d result=%v", out.Status, out.Result)
	}
	// 同一目标可以再提（去重不再把它算作待处理）。
	if res, _ := mcpCall(t, sess, "propose_refund_decision", map[string]any{"refund_no": rf.RefundNo, "action": "approve",
		"evidence": "list_refunds：再提一次，未发货、仅退款"}); res.IsError && strings.Contains(mcpText(res), "同样的待处理提案") {
		t.Fatalf("失败的提案不该挡住同一目标再提：%s", mcpText(res))
	}
}
