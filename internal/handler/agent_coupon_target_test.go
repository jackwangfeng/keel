package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 券的精准发放（带对照组）：40 个买家都在 40 天前买过一次 → customer_segments 里是 40 个沉睡顾客 →
// AI 提「定向发给沉睡顾客、留 20% 对照」→ 人批准 → 32 人每人一张券、8 人记作对照、都写进
// coupon_target_members → 发券组里 6 人回来下单 → 复盘给出两组下单率与提升（对照组不到 30 人，只给数）。
func TestTargetedCouponWithHoldout(t *testing.T) {
	cs := newCouponShop(t)
	var buyers []couponBuyer
	for i := 0; i < 40; i++ {
		b := cs.newBuyer(t, fmt.Sprintf("lapsed-%d", i))
		cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
		buyers = append(buyers, b)
	}
	adminExec(t, `UPDATE orders SET paid_at = paid_at - interval '40 days', created_at = created_at - interval '40 days'
	               WHERE merchant_id = $1`, cs.MerchantID)

	a := createAgent(t, cs.adminShop, `{"name":"发券 AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, seg := mcpCall(t, sess, "customer_segments", map[string]any{})
	if res.IsError {
		t.Fatalf("customer_segments：%s", mcpText(res))
	}
	sizes := map[string]float64{}
	for _, x := range seg["segments"].([]any) {
		m := x.(map[string]any)
		sizes[m["segment"].(string)] = m["size"].(float64)
	}
	if sizes["lapsed"] != 40 || sizes["one_time"] != 40 {
		t.Fatalf("人群人数不对：%v", sizes)
	}
	if strings.Contains(fmt.Sprint(seg), buyers[0].Phone) {
		t.Fatal("人群工具不该带出手机号")
	}

	args := map[string]any{"name": "老顾客回来看看", "coupon_type": 3, "discount_cents": 500, "valid_days": 7,
		"total_count": 1, "per_user_limit": 1, "target": map[string]any{"segment": "lapsed", "holdout_pct": 20},
		"evidence": "customer_segments：沉睡顾客 40 人，距上次付款平均 40 天"}
	res, pr := mcpCall(t, sess, "propose_coupon", args)
	if res.IsError {
		t.Fatalf("提案：%s", mcpText(res))
	}
	if !strings.Contains(pr["title"].(string), "沉睡顾客") {
		t.Fatalf("标题应写明发给谁：%v", pr["title"])
	}
	// 带 target 的券不能进领券中心；人群太小时拒
	bad := map[string]any{}
	for k, v := range args {
		bad[k] = v
	}
	bad["name"], bad["claimable"] = "进领券中心", true
	if r, _ := mcpCall(t, sess, "propose_coupon", bad); !r.IsError || !strings.Contains(mcpText(r), "claimable") {
		t.Fatalf("带 target 又 claimable 应被拒：%s", mcpText(r))
	}
	bad["claimable"], bad["name"], bad["target"] = false, "高价值", map[string]any{"segment": "high_value"}
	if r, _ := mcpCall(t, sess, "propose_coupon", bad); !r.IsError || !strings.Contains(mcpText(r), "量不出效果") {
		t.Fatalf("人群太小应被拒：%s", mcpText(r))
	}

	id := int64(pr["id"].(float64))
	var p api.AgentProposal
	decodeInto(t, postWithKey(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", id), "", cs.Token, freshIdemKey()),
		http.StatusOK, "批准", &p)
	if p.Status != 20 {
		t.Fatalf("应已执行：%+v", p.Result)
	}
	var treated, control, coupons int64
	if err := admin(t).QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE treated), count(*) FILTER (WHERE NOT treated),
		(SELECT count(*) FROM user_coupons uc JOIN coupon_target_members m ON m.user_coupon_id = uc.id WHERE m.proposal_id = $1)
		FROM coupon_target_members WHERE proposal_id = $1`, id).Scan(&treated, &control, &coupons); err != nil {
		t.Fatal(err)
	}
	if treated != 32 || control != 8 || coupons != 32 {
		t.Fatalf("分组：发券 %d、对照 %d、发出 %d 张（期望 32 / 8 / 32）", treated, control, coupons)
	}

	// 发券组里 6 人回来下单（不一定用券：下单率比的是人回没回来）
	rows, err := admin(t).Query(context.Background(),
		`SELECT u.phone FROM coupon_target_members m JOIN users u ON u.id = m.user_id WHERE m.proposal_id = $1 AND m.treated LIMIT 6`, id)
	if err != nil {
		t.Fatal(err)
	}
	back := map[string]bool{}
	for rows.Next() {
		var ph string
		_ = rows.Scan(&ph)
		back[ph] = true
	}
	rows.Close()
	for _, b := range buyers {
		if back[b.Phone] {
			cs.placePaid(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
		}
	}
	// 窗口走完：执行时间拨回 8 天前，回来下单的那几单也挪进窗口
	adminExec(t, `UPDATE agent_proposals SET executed_at = executed_at - interval '8 days', outcome_due_at = now() - interval '1 minute'
	               WHERE id = $1`, id)
	adminExec(t, `UPDATE orders SET paid_at = paid_at - interval '7 days' WHERE merchant_id = $1 AND paid_at > now() - interval '1 hour'`,
		cs.MerchantID)
	if n, err := service.ReviewProposalOutcomesOnce(context.Background(), repository.New(testPool), localInventory(), nil); err != nil || n < 1 {
		t.Fatalf("复盘：n=%d err=%v", n, err)
	}
	decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d", id), cs.Token), http.StatusOK, "提案详情", &p)
	o := *p.Outcome
	if o["treated_size"] != float64(32) || o["holdout_size"] != float64(8) || o["treated_conversion"] != 0.1875 ||
		o["holdout_conversion"] != float64(0) || o["lift"] != 0.1875 || o["verdict"] != "neutral" {
		t.Fatalf("复盘：%v", o)
	}
}
