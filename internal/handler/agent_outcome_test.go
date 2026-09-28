package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	adminExec(t, `UPDATE agent_proposals SET outcome_due_at = now() - interval '1 minute' WHERE id = $1`, id)

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
