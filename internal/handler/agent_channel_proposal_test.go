package handler_test

// 提案种类 channel_stock_rule（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §4.2–4.5）。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

func mcpProblemStatus(res *mcp.CallToolResult) float64 {
	pr, _ := res.Meta["keel/problem"].(map[string]any)
	s, _ := pr["status"].(float64)
	return s
}

type chRuleRig struct {
	*fakeOrderRig
	sess *mcp.ClientSession
	a    api.AdminAgent
}

func newChRuleRig(t *testing.T) chRuleRig {
	t.Helper()
	r := newFakeOrderRig(t, nil, map[string]any{"commission_bp": 1500})
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithChannels(r.svc)))
	if _, err := r.svc.UpsertStockRule(r.ctx, repository.ChannelStockRule{BindingID: r.b.ID, RatioBP: 8000}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	a := createAgent(t, r.cs.adminShop, `{"name":"渠道 AI","role":2}`)
	sess := mcpConnect(t, r.cs.Host, issueAgentKey(t, r.cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	return chRuleRig{fakeOrderRig: r, sess: sess, a: a}
}

func (r chRuleRig) propose(t *testing.T, changes ...map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	return r.proposeAt(t, r.cs.NorthStore, changes...)
}

func (r chRuleRig) proposeAt(t *testing.T, store int64, changes ...map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	return mcpCall(t, r.sess, "propose_channel_stock_rule", map[string]any{"binding_id": r.b.ID, "store_id": store,
		"changes": changes, "evidence": "channel_allocation_review：挂零 48 小时、日均 2 件"})
}

func (r chRuleRig) approve(t *testing.T, id int64) api.AgentProposal {
	t.Helper()
	var p api.AgentProposal
	decodeInto(t, postWithKey(t, r.cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", id), "", r.cs.Token,
		freshIdemKey()), http.StatusOK, "批准", &p)
	return p
}

func (r chRuleRig) skuRule(t *testing.T, store int64, sku *int64) string {
	t.Helper()
	q := `SELECT ratio_bp || '/' || safety_qty FROM channel_stock_rules WHERE binding_id = $1 AND store_id = $2 AND sku_id IS NULL`
	args := []any{r.b.ID, store}
	if sku != nil {
		q = `SELECT ratio_bp || '/' || safety_qty FROM channel_stock_rules WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`
		args = append(args, *sku)
	}
	var s string
	_ = admin(t).QueryRow(context.Background(), q, args...).Scan(&s)
	return s
}

func change(sku *int64, ratio, safety int, prev map[string]any) map[string]any {
	c := map[string]any{"ratio_bp": ratio, "safety_qty": safety, "prev": prev}
	if sku != nil {
		c["sku_id"] = *sku
	}
	return c
}

func prevRule(ratio, safety int, level string) map[string]any {
	return map[string]any{"ratio_bp": ratio, "safety_qty": safety, "level": level}
}

// 校验、去重、试算、执行生效 + 推送、重放幂等、执行时规则被人改过（Review Focus 2）。
func TestChannelStockRuleProposal(t *testing.T) {
	r := newChRuleRig(t)
	cs := r.cs
	dress := cs.DressSKU
	avail := r.dressStock(t)
	bindingPrev := prevRule(8000, 0, channel.RuleLevelBinding)

	// 校验。
	for _, c := range []struct {
		name   string
		store  int64
		ch     []map[string]any
		status float64
		text   string
	}{
		{"prev 不一致", cs.NorthStore, []map[string]any{change(&dress, 9000, 0, prevRule(7000, 0, "binding"))}, 409, "规则已经变了"},
		{"prev 级别不一致", cs.NorthStore, []map[string]any{change(&dress, 9000, 0, prevRule(8000, 0, "store"))}, 409, "规则已经变了"},
		{"比例 > 10000", cs.NorthStore, []map[string]any{change(&dress, 10001, 0, bindingPrev)}, 422, "ratio_bp"},
		{"安全库存为负", cs.NorthStore, []map[string]any{change(&dress, 9000, -1, bindingPrev)}, 422, "safety_qty"},
		{"门店未映射", cs.SouthStore, []map[string]any{change(&dress, 9000, 0, bindingPrev)}, 422, "没有映射"},
		{"没有改动", cs.NorthStore, []map[string]any{change(&dress, 8000, 0, bindingPrev)}, 422, "没有改动"},
		{"同一格两次", cs.NorthStore, []map[string]any{change(&dress, 9000, 0, bindingPrev), change(&dress, 7000, 0, bindingPrev)}, 422, "两次"},
	} {
		res, _ := r.proposeAt(t, c.store, c.ch...)
		if !res.IsError || mcpProblemStatus(res) != c.status || !strings.Contains(mcpText(res), c.text) {
			t.Errorf("%s：应 %v 且含「%s」，实得 isError=%v %v %q", c.name, c.status, c.text, res.IsError, mcpProblemStatus(res), mcpText(res))
		}
	}
	many := make([]map[string]any, 21)
	for i := range many {
		sku := int64(100000 + i)
		many[i] = change(&sku, 9000, 0, bindingPrev)
	}
	if res, _ := r.propose(t, many...); !res.IsError || mcpProblemStatus(res) != 422 {
		t.Errorf("21 条应 422：%q", mcpText(res))
	}
	// 门店范围的 AI 员工提不了（全店的事）。
	sa := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	ss := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, sa.Id, `{"name":"t"}`).Secret)
	if res, _ := mcpCall(t, ss, "propose_channel_stock_rule", map[string]any{"binding_id": r.b.ID, "store_id": cs.NorthStore,
		"changes": []map[string]any{change(&dress, 9000, 0, bindingPrev)}, "evidence": "channel_allocation_review：挂零"}); !res.IsError ||
		mcpProblemStatus(res) != 403 {
		t.Errorf("门店范围的 AI 员工应 403：%v %q", mcpProblemStatus(res), mcpText(res))
	}
	// binding 停用。
	off, on := repository.ChannelBindingDisabled, repository.ChannelBindingActive
	if _, err := r.svc.UpdateBinding(r.ctx, r.b.ID, service.ChannelBindingUpdate{Status: &off}); err != nil {
		t.Fatal(err)
	}
	if res, _ := r.propose(t, change(&dress, 9000, 0, bindingPrev)); !res.IsError || !strings.Contains(mcpText(res), "没在启用中") {
		t.Errorf("binding 停用应拒：%q", mcpText(res))
	}
	if _, err := r.svc.UpdateBinding(r.ctx, r.b.ID, service.ChannelBindingUpdate{Status: &on}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)

	// 提案 + 试算：连衣裙 80% → 90%、安全库存 1。
	res, p1 := r.propose(t, change(&dress, 9000, 1, bindingPrev))
	if res.IsError {
		t.Fatalf("提案出错：%s", mcpText(res))
	}
	if p1["status"].(float64) != 10 || !strings.Contains(p1["title"].(string), "假渠道") ||
		!strings.Contains(p1["title"].(string), "比例 80%→90%") || p1["store_id"] != nil {
		t.Fatalf("提案应待处理、标题带渠道名与比例变化、没有 store_id：%v", p1)
	}
	pv := p1["payload"].(map[string]any)["preview"].([]any)
	if len(pv) != 1 {
		t.Fatalf("应有一格试算：%v", pv)
	}
	got := pv[0].(map[string]any)
	wantBefore, wantAfter := math.Floor(float64(avail)*0.8), math.Floor(float64(avail)*0.9)-1
	if int64(got["sku_id"].(float64)) != dress || int64(got["available"].(float64)) != avail ||
		got["before_qty"].(float64) != wantBefore || got["after_qty"].(float64) != wantAfter {
		t.Fatalf("试算：可售 %d、对外 %v → %v，实得 %v", avail, wantBefore, wantAfter, got)
	}
	// 去重：同一组格子有待处理的不重复提。
	if res, _ := r.propose(t, change(&dress, 10000, 0, bindingPrev)); !res.IsError || mcpProblemStatus(res) != 409 ||
		!strings.Contains(mcpText(res), fmt.Sprintf("chstock:%d:%d:%d", r.b.ID, cs.NorthStore, dress)) {
		t.Errorf("同一格待处理时应 409 去重：%q", mcpText(res))
	}

	// 批准 → 规则生效、入队推送。
	id1 := int64(p1["id"].(float64))
	p := r.approve(t, id1)
	if p.Status != 20 || p.Result == nil {
		t.Fatalf("应执行成功：%+v", p)
	}
	if got := r.skuRule(t, cs.NorthStore, &dress); got != "9000/1" {
		t.Fatalf("门店 × SKU 级规则应是 9000/1，实得 %q", got)
	}
	r.drain(t)
	if q := adminQueryInt64(t, `SELECT published_qty FROM channel_listings WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`,
		r.b.ID, cs.NorthStore, dress); float64(q) != wantAfter {
		t.Fatalf("推送后对外可售应是 %v，实得 %d", wantAfter, q)
	}
	// 重放（结果没写回、再点批准）：规则已经是目标值 → 当成功，不再写。
	adminExec(t, `UPDATE agent_proposals SET status = 15, result = NULL WHERE id = $1`, id1)
	p = r.approve(t, id1)
	if d, _ := (*p.Result)["detail"].(map[string]any); p.Status != 20 || d["already"] != float64(1) || d["applied"] != float64(0) {
		t.Fatalf("重放应当成功、already = 1：%+v", p.Result)
	}

	// 执行时规则被人改过：门店级 80% → 90% 的提案提了之后，店长在后台把门店级改成 60% → 批准 → 失败、规则保持 60%。
	res, p2 := r.propose(t, change(nil, 9000, 0, bindingPrev))
	if res.IsError {
		t.Fatalf("门店级提案出错：%s", mcpText(res))
	}
	if pv := p2["payload"].(map[string]any)["preview"].([]any); len(pv) != 1 ||
		pv[0].(map[string]any)["before_qty"] != pv[0].(map[string]any)["after_qty"] {
		t.Fatalf("门店级试算：连衣裙有自己的规则，门店级改动不影响它（前后相等）：%v", pv)
	}
	north := cs.NorthStore
	if _, err := r.svc.UpsertStockRule(r.ctx, repository.ChannelStockRule{BindingID: r.b.ID, StoreID: &north, RatioBP: 6000}); err != nil {
		t.Fatal(err)
	}
	p = r.approve(t, int64(p2["id"].(float64)))
	if p.Status != 40 || (*p.Result)["error_type"] != "stale" || !strings.Contains((*p.Result)["error"].(string), "门店级") {
		t.Fatalf("规则被人改过应执行失败（stale）、说明哪一格：%+v", p.Result)
	}
	if got := r.skuRule(t, cs.NorthStore, nil); got != "6000/0" {
		t.Fatalf("门店级规则应保持店长改的 6000/0，实得 %q", got)
	}
}

// 自动执行（Review Focus 3）：max_ratio_step_bp = 2000 时 80% → 0 不自动执行、80% → 70% 自动执行；默认 0 不自动执行。
func TestChannelStockRuleAutoPolicy(t *testing.T) {
	r := newChRuleRig(t)
	cs := r.cs
	dress := cs.DressSKU
	polPath := fmt.Sprintf("/api/v1/admin/agents/%d/auto-policies/channel_stock_rule", r.a.Id)
	var pol api.AgentAutoPolicy
	decodeInto(t, putAs(t, cs.Host, polPath,
		`{"enabled":true,"max_units":0,"min_discount_rate":1000,"max_discount_cents":0,"daily_limit":10}`, cs.Token),
		http.StatusOK, "设策略（不给 max_ratio_step_bp）", &pol)
	if pol.MaxRatioStepBp == nil || *pol.MaxRatioStepBp != 0 {
		t.Fatalf("不给 max_ratio_step_bp 应为 0：%+v", pol)
	}
	bindingPrev := prevRule(8000, 0, channel.RuleLevelBinding)
	_, p := r.propose(t, change(&dress, 7000, 0, bindingPrev))
	if p["status"].(float64) != 10 {
		t.Fatalf("max_ratio_step_bp = 0 不该自动执行：%v", p)
	}
	wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/reject", int64(p["id"].(float64))),
		`{"reason":"先不调"}`, cs.Token, freshIdemKey()), http.StatusOK, "驳回")
	if w := putAs(t, cs.Host, polPath, `{"enabled":true,"max_units":0,"min_discount_rate":1000,"max_discount_cents":0,
		"daily_limit":10,"max_ratio_step_bp":10001}`, cs.Token); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("max_ratio_step_bp 越界应 422，实得 %d", w.Code)
	}
	decodeInto(t, putAs(t, cs.Host, polPath, `{"enabled":true,"max_units":0,"min_discount_rate":1000,"max_discount_cents":0,
		"daily_limit":10,"max_ratio_step_bp":2000}`, cs.Token), http.StatusOK, "设策略", &pol)
	if *pol.MaxRatioStepBp != 2000 {
		t.Fatalf("策略应带 max_ratio_step_bp = 2000：%+v", pol)
	}

	_, p = r.propose(t, change(&dress, 0, 0, bindingPrev))
	if p["status"].(float64) != 10 || p["auto_approved"] == true {
		t.Fatalf("80%% → 0 不许自动执行：%v", p)
	}
	wantStatus(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/reject", int64(p["id"].(float64))),
		`{"reason":"不下架"}`, cs.Token, freshIdemKey()), http.StatusOK, "驳回")
	_, p = r.propose(t, change(&dress, 7000, 0, bindingPrev))
	if p["status"].(float64) != 20 || p["auto_approved"] != true {
		t.Fatalf("80%% → 70%% 在上限内应自动执行：%v", p)
	}
	if got := r.skuRule(t, cs.NorthStore, &dress); got != "7000/0" {
		t.Fatalf("自动执行后规则应是 7000/0，实得 %q", got)
	}
}

// 复盘（Review Focus 4）：执行时间钉在 8 天前的整点，前后 7 天各造挂零 / 卖出 / 拒单；上调后挂零减少 → positive；
// 窗口里 keel 断货 3 天 → 那一格不计、全部不计 → neutral「缺的是货」。
func TestChannelStockRuleOutcome(t *testing.T) {
	r := newChRuleRig(t)
	cs := r.cs
	dress := cs.DressSKU
	_, p := r.propose(t, change(&dress, 9000, 0, prevRule(8000, 0, channel.RuleLevelBinding)))
	id := int64(p["id"].(float64))
	if ap := r.approve(t, id); ap.Status != 20 {
		t.Fatalf("应执行成功：%+v", ap)
	}
	r.drain(t)
	ex := time.Now().UTC().Truncate(time.Hour).Add(-8 * 24 * time.Hour)
	adminExec(t, `UPDATE agent_proposals SET executed_at = $2, outcome_due_at = now() - interval '1 minute' WHERE id = $1`, id, ex)
	// 前 7 天：规则让它挂零 48 小时、一张缺货拒单；后 7 天：挂零 10 小时、接成一张 2 件的单。
	adminExec(t, `INSERT INTO channel_listing_zero_spans (merchant_id, binding_id, store_id, sku_id, held, started_at, ended_at)
		VALUES ($1, $2, $3, $4, true, $5::timestamptz - interval '60 hours', $5::timestamptz - interval '12 hours'),
		       ($1, $2, $3, $4, true, $5::timestamptz + interval '1 day', $5::timestamptz + interval '34 hours')`,
		cs.MerchantID, r.b.ID, cs.NorthStore, dress, ex)
	r.put(t, "oc-ok", 1, channel.OrderNew, 2)
	adminExec(t, `UPDATE orders SET paid_at = $2::timestamptz + interval '2 days' WHERE channel_order_id = $1`, r.channelOrderID(t, "oc-ok"), ex)
	big := int32(r.dressStock(t) + 5)
	r.put(t, "oc-rej", 1, channel.OrderNew, big)
	adminExec(t, `UPDATE channel_orders SET created_at = $2::timestamptz - interval '1 day' WHERE id = $1`, r.channelOrderID(t, "oc-rej"), ex)

	review := func() map[string]any {
		t.Helper()
		if _, err := service.ReviewProposalOutcomesOnce(context.Background(), repository.New(testPool), localInventory(), nil); err != nil {
			t.Fatal(err)
		}
		var got api.AgentProposal
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d", id), cs.Token), http.StatusOK, "详情", &got)
		if got.Outcome == nil {
			t.Fatal("应已复盘")
		}
		return *got.Outcome
	}
	o := review()
	cells, _ := o["cells"].([]any)
	if o["verdict"] != "positive" || len(cells) != 1 {
		t.Fatalf("上调后挂零 48 → 10 小时应 positive、一格：%v", o)
	}
	c := cells[0].(map[string]any)
	b, a := c["before"].(map[string]any), c["after"].(map[string]any)
	if c["direction"] != "up" || int64(c["sku_id"].(float64)) != dress || b["held_zero_hours"] != float64(48) ||
		a["held_zero_hours"] != float64(10) || b["stockout_rejects"] != float64(big) || a["stockout_rejects"] != float64(0) ||
		b["sold"] != float64(0) || a["sold"] != float64(2) || a["net_cents"].(float64) <= 0 || c["excluded_reason"] != nil {
		t.Fatalf("格子的前后数字不对（拒单 %d 在前、卖出 2 在后）：%v", big, c)
	}

	// 后 7 天里 keel 自己断货 3 天：那一格不计 → neutral「缺的是货」。
	adminExec(t, `INSERT INTO channel_listing_zero_spans (merchant_id, binding_id, store_id, sku_id, held, started_at, ended_at)
		VALUES ($1, $2, $3, $4, false, $5::timestamptz + interval '3 days', $5::timestamptz + interval '6 days')`,
		cs.MerchantID, r.b.ID, cs.NorthStore, dress, ex)
	adminExec(t, `UPDATE agent_proposals SET outcome = NULL, outcome_at = NULL, outcome_due_at = now() - interval '1 minute' WHERE id = $1`, id)
	o = review()
	c = o["cells"].([]any)[0].(map[string]any)
	if o["verdict"] != "neutral" || !strings.Contains(o["explanation"].(string), "缺的是货") ||
		!strings.Contains(fmt.Sprint(c["excluded_reason"]), "缺的是货") {
		t.Fatalf("断货 3 天的格子不计、全部不计应 neutral：%v", o)
	}
}

// KEEL_CHANNELS 关着：propose_channel_stock_rule 拒收（409）。
func TestChannelStockRuleProposalChannelsOff(t *testing.T) {
	cs := newCouponShop(t)
	useEngine(t, app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}))
	a := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, _ := mcpCall(t, sess, "propose_channel_stock_rule", map[string]any{"binding_id": 1, "store_id": cs.NorthStore,
		"changes":  []map[string]any{change(&cs.DressSKU, 9000, 0, prevRule(8000, 0, "binding"))},
		"evidence": "channel_allocation_review：挂零 48 小时"})
	if !res.IsError || mcpProblemStatus(res) != 409 || !strings.Contains(mcpText(res), "没有启用的销售渠道") {
		t.Fatalf("渠道关着应 409「没有启用的销售渠道」：%v %q", mcpProblemStatus(res), mcpText(res))
	}
}

// 审查修复 2：执行提案与后台改规则按 binding 串行（channel_bindings 那一行 FOR NO KEY UPDATE）。
// 一个事务拿着这把锁时，提案执行要等它提交；等到之后读到的是人刚改的规则 → 失败（stale），不覆盖人的修改。
func TestChannelStockRuleExecSerializesWithRuleWrites(t *testing.T) {
	r := newChRuleRig(t)
	cs := r.cs
	dress := cs.DressSKU
	_, p := r.propose(t, change(&dress, 9000, 0, prevRule(8000, 0, channel.RuleLevelBinding)))
	id := int64(p["id"].(float64))

	conn := admin(t)
	tx, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM channel_bindings WHERE id = $1 FOR NO KEY UPDATE`, r.b.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan api.AgentProposal, 1)
	go func() {
		var ap api.AgentProposal
		w := postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", id), "", cs.Token, freshIdemKey())
		_ = json.Unmarshal(w.Body.Bytes(), &ap)
		done <- ap
	}()
	select {
	case ap := <-done:
		t.Fatalf("有事务拿着 binding 的锁时提案执行没有等：%+v", ap)
	case <-time.After(500 * time.Millisecond):
	}
	// 人在这个事务里把连衣裙改成 60%，提交。
	if _, err := tx.Exec(context.Background(), `INSERT INTO channel_stock_rules (merchant_id, binding_id, store_id, sku_id, ratio_bp, safety_qty)
		VALUES ($1, $2, $3, $4, 6000, 0)`, cs.MerchantID, r.b.ID, cs.NorthStore, dress); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ap api.AgentProposal
	select {
	case ap = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("锁放开 10 秒了提案执行还没回来")
	}
	if ap.Status != 40 || ap.Result == nil || (*ap.Result)["error_type"] != "stale" {
		t.Fatalf("等到锁之后应读到人改的规则、执行失败（stale）：%+v", ap)
	}
	if got := r.skuRule(t, cs.NorthStore, &dress); got != "6000/0" {
		t.Fatalf("规则应保持人改的 6000/0，实得 %q", got)
	}

	// 后台改规则也拿同一把锁。
	tx2, err := conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(context.Background())
	if _, err := tx2.Exec(context.Background(), `SELECT 1 FROM channel_bindings WHERE id = $1 FOR NO KEY UPDATE`, r.b.ID); err != nil {
		t.Fatal(err)
	}
	upserted := make(chan error, 1)
	go func() {
		_, e := r.svc.UpsertStockRule(r.ctx, repository.ChannelStockRule{BindingID: r.b.ID, RatioBP: 7000})
		upserted <- e
	}()
	select {
	case e := <-upserted:
		t.Fatalf("有事务拿着 binding 的锁时后台改规则没有等（err=%v）", e)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx2.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e := <-upserted; e != nil {
		t.Fatal(e)
	}
}
