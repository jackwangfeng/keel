package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 执行后复盘（00122）：加库存提案批准执行 → 到点（把 outcome_due_at 拨到过去）复盘扫描量一次 →
// outcome 带 verdict 与指标，成绩单按种类计数、明细里有它；AI 员工自己用 my_scorecard 也看得到。
func TestProposalOutcomeAndScorecard(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"复盘 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)

	res, pr := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU,
		"delta": 10, "reason": "补货", "evidence": "restock_plan：日均 2 件，可售 3，预计明天卖断"})
	if res.IsError {
		t.Fatalf("提案出错：%s", mcpText(res))
	}
	id := int64(pr["id"].(float64))
	var p api.AgentProposal
	decodeInto(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", id), "", cs.Token, freshIdemKey()),
		http.StatusOK, "批准", &p)
	if p.Status != 20 || p.ExecutedAt == nil || p.Outcome != nil {
		t.Fatalf("执行后应有 executed_at、还没有 outcome：%+v", p)
	}
	var due bool
	if err := admin(t).QueryRow(context.Background(),
		`SELECT outcome_due_at > now() + interval '6 days' FROM agent_proposals WHERE id = $1`, id).Scan(&due); err != nil || !due {
		t.Fatalf("加库存应 7 天后复盘：%v %v", due, err)
	}
	// 只把 outcome_due_at 拨到过去、窗口（执行后 7 天）还没走完：不算半截数据，推迟回窗口关闭。
	adminExec(t, `UPDATE agent_proposals SET outcome_due_at = now() - interval '1 minute' WHERE id = $1`, id)
	if _, err := service.ReviewProposalOutcomesOnce(context.Background(), repository.New(testPool), localInventory(), nil); err != nil {
		t.Fatal(err)
	}
	var deferred bool
	if err := admin(t).QueryRow(context.Background(), `SELECT outcome IS NULL AND outcome_due_at = executed_at + interval '7 days'
		FROM agent_proposals WHERE id = $1`, id).Scan(&deferred); err != nil || !deferred {
		t.Fatalf("窗口没走完时应推迟到执行后 7 天、不写 outcome：%v %v", deferred, err)
	}

	// 窗口真的走完了（执行时间拨回 8 天前）：量一次。
	adminExec(t, `UPDATE agent_proposals SET executed_at = executed_at - interval '8 days',
		outcome_due_at = now() - interval '1 minute' WHERE id = $1`, id)
	n, err := service.ReviewProposalOutcomesOnce(context.Background(), repository.New(testPool), localInventory(), nil)
	if err != nil || n < 1 {
		t.Fatalf("复盘扫描：n=%d err=%v", n, err)
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d", id), cs.Token), http.StatusOK, "提案详情", &p)
	if p.Outcome == nil || (*p.Outcome)["verdict"] == nil || (*p.Outcome)["sold_qty"] == nil || p.OutcomeAt == nil {
		t.Fatalf("复盘后应有 verdict 与卖出件数：%+v", p.Outcome)
	}

	var sc api.AgentScorecard
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agents/%d/scorecard", a.Id), cs.Token), http.StatusOK, "成绩单", &sc)
	if len(sc.Kinds) != 1 || sc.Kinds[0].Kind != "inventory_adjust" || sc.Kinds[0].Executed != 1 || len(sc.Recent) != 1 ||
		sc.Recent[0].ProposalId != id {
		raw, _ := json.Marshal(sc)
		t.Fatalf("成绩单应有这一条已执行、已复盘的加库存：%s", raw)
	}
	res, mine := mcpCall(t, sess, "my_scorecard", map[string]any{})
	if res.IsError || len(mine["kinds"].([]any)) != 1 {
		t.Fatalf("my_scorecard：%s %v", mcpText(res), mine)
	}
}

// 自动执行策略（00130）：给「加库存」开策略（单笔 ≤ 20 件、24 小时 ≤ 1 条）后，15 件的提案当场执行（auto_approved）；
// 同一天第二条超出条数上限、30 件的超出单笔上限，都照常进待处理。售后审核的策略写不进去。
func TestAutoPolicyExecutesWithinLimits(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"放手 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)
	polPath := fmt.Sprintf("/api/v1/admin/agents/%d/auto-policies/", a.Id)
	var pol api.AgentAutoPolicy
	decodeInto(t, putAs(t, cs.Host, polPath+"inventory_adjust",
		`{"enabled":true,"max_units":20,"min_discount_rate":1000,"max_discount_cents":0,"daily_limit":1}`, cs.Token),
		http.StatusOK, "设策略", &pol)
	w := putAs(t, cs.Host, polPath+"refund_decision",
		`{"enabled":true,"max_units":0,"min_discount_rate":1000,"max_discount_cents":0,"daily_limit":5}`, cs.Token)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("售后审核不许自动执行，应 422，实得 %d", w.Code)
	}
	propose := func(store, sku int64, delta int) map[string]any {
		t.Helper()
		res, out := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": store, "sku_id": sku,
			"delta": delta, "reason": "补货", "evidence": "restock_plan：日均 3 件，可售 2"})
		if res.IsError {
			t.Fatalf("提案出错：%s", mcpText(res))
		}
		return out
	}
	p1 := propose(cs.NorthStore, cs.DressSKU, 15)
	if p1["status"].(float64) != 20 || p1["auto_approved"] != true || p1["decided_by"] != nil {
		t.Fatalf("在上限内应当场执行：%v", p1)
	}
	p2 := propose(cs.NorthStore, cs.ShirtSKU, 10)
	if p2["status"].(float64) != 10 || p2["auto_approved"] == true {
		t.Fatalf("24 小时的条数用完，应进待处理：%v", p2)
	}
	decodeInto(t, putAs(t, cs.Host, polPath+"inventory_adjust",
		`{"enabled":true,"max_units":20,"min_discount_rate":1000,"max_discount_cents":0,"daily_limit":10}`, cs.Token),
		http.StatusOK, "放宽条数", &pol)
	p3 := propose(cs.SouthStore, cs.DressSKU, 30)
	if p3["status"].(float64) != 10 {
		t.Fatalf("超出单笔上限应进待处理：%v", p3)
	}
	var list struct {
		Items []api.AgentAutoPolicy `json:"items"`
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agents/%d/auto-policies", a.Id), cs.Token),
		http.StatusOK, "策略列表", &list)
	if len(list.Items) != 4 {
		t.Fatalf("四种可自动执行的种类都应列出：%+v", list.Items)
	}
}

// 公开的 AI 经营日志（00132）：默认关（404）；管理员打开后免登录可读，只有标题 / 状态 / 结论，不带证据全文。
func TestPublicAILog(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"公开 AI","role":2}`)
	k := issueAgentKey(t, cs.adminShop, a.Id, `{"name":"test"}`)
	sess := mcpConnect(t, cs.Host, k.Secret)
	res, _ := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": cs.NorthStore, "sku_id": cs.DressSKU,
		"delta": 5, "reason": "补货", "evidence": "内部证据：供应商报价 12 元，不该公开"})
	if res.IsError {
		t.Fatal(mcpText(res))
	}
	if w := getAs(t, cs.Host, "/api/v1/ai-log", ""); w.Code != http.StatusNotFound {
		t.Fatalf("默认关，应 404，实得 %d", w.Code)
	}
	wantStatus(t, putAs(t, cs.Host, "/api/v1/admin/ai-log/settings", `{"enabled":true}`, cs.Token), http.StatusOK, "打开")
	w := getAs(t, cs.Host, "/api/v1/ai-log", "")
	wantStatus(t, w, http.StatusOK, "公开日志")
	if body := w.Body.String(); !strings.Contains(body, "补 5 件") || strings.Contains(body, "供应商报价") {
		t.Fatalf("公开日志应有提案标题、不带证据：%s", body)
	}
}
