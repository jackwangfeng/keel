package handler_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// AI 员工的事件（AI 经营 M10 §3，00121）：写事件（售后申请、提案结果、扫描）、拉（list_events / ack_events，
// 按管辖范围过滤）、推（webhook，签名可验、失败重试、只推范围内的）。

func putAgentWebhook(t *testing.T, sh adminShop, agentID int64, body string) api.AgentWebhook {
	t.Helper()
	var w api.AgentWebhook
	decodeInto(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/agents/%d/webhook", agentID), body, sh.Token),
		http.StatusOK, "配 webhook", &w)
	return w
}

// refundOnNorth 在北京门店下单、付款、申请整单仅退款，返回退款单号。
func refundOnNorth(t *testing.T, cs couponShop, tag string) string {
	t.Helper()
	b := cs.newBuyer(t, tag)
	o := cs.twoLineOrder(t, b, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)
	_, lines := cs.lines(t, b, o.OrderNo)
	r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 2}, [2]int64{lines[cs.ShirtSKU].Id, 1}))
	return r.RefundNo
}

func eventIDs(out map[string]any) []int64 {
	items, _ := out["items"].([]any)
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, int64(it.(map[string]any)["id"].(float64)))
	}
	return ids
}

func TestAgentEventsRefundCreatedAndPullByScope(t *testing.T) {
	cs := newCouponShop(t)
	northAI := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	shopAI := createAgent(t, cs.adminShop, `{"name":"全店 AI","role":2}`)
	north := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, northAI.Id, `{"name":"t"}`).Secret)
	shop := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, shopAI.Id, `{"name":"t"}`).Secret)

	// 买家申请售后 → 同一个事务里一条 refund_created（北京门店）。
	refundNo := refundOnNorth(t, cs, "evt")
	var refundEv int64
	var store int64
	var payload []byte
	if err := admin(t).QueryRow(context.Background(), `SELECT id, store_id, payload FROM agent_events
		WHERE merchant_id = $1 AND type = 'refund_created' AND dedupe_key = $2`, cs.MerchantID, "refund_created:"+refundNo).
		Scan(&refundEv, &store, &payload); err != nil {
		t.Fatalf("申请售后之后没有 refund_created 事件：%v", err)
	}
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	if store != cs.NorthStore || p["refund_no"] != refundNo || p["amount_cents"].(float64) <= 0 || p["order_no"] == "" {
		t.Fatalf("refund_created：store=%d payload=%v", store, p)
	}
	// 另两条：广州门店的库存预警、全店的无结果词突增（直接写库，只为验范围过滤）。
	var southEv, shopEv int64
	if err := admin(t).QueryRow(context.Background(), `INSERT INTO agent_events (merchant_id, type, store_id, payload, dedupe_key)
		VALUES ($1, 'stock_low', $2, '{"sku_id":1}', 'evt-south') RETURNING id`, cs.MerchantID, cs.SouthStore).Scan(&southEv); err != nil {
		t.Fatal(err)
	}
	if err := admin(t).QueryRow(context.Background(), `INSERT INTO agent_events (merchant_id, type, payload, dedupe_key)
		VALUES ($1, 'search_zero_spike', '{"query":"泳衣","count":7}', 'evt-shop') RETURNING id`, cs.MerchantID).Scan(&shopEv); err != nil {
		t.Fatal(err)
	}

	// 门店管理员身份：只看得到北京门店的那条；广州的、全店的都看不到。
	res, out := mcpCall(t, north, "list_events", nil)
	if ids := eventIDs(out); res.IsError || len(ids) != 1 || ids[0] != refundEv {
		t.Fatalf("北京店 AI 的 list_events：%s %v", mcpText(res), out)
	}
	// 全店范围：三条都在，按 id 升序。
	_, out = mcpCall(t, shop, "list_events", nil)
	if ids := eventIDs(out); len(ids) != 3 || ids[0] != refundEv || ids[1] != southEv || ids[2] != shopEv {
		t.Fatalf("全店 AI 的 list_events：%v", out)
	}
	// 分页 + 游标：一次一条 → has_more；ack 之后不带 after_id 从游标之后读。
	_, out = mcpCall(t, shop, "list_events", map[string]any{"limit": 1})
	if out["has_more"] != true || int64(out["next_after_id"].(float64)) != refundEv || out["cursor"].(float64) != 0 {
		t.Fatalf("limit=1：%v", out)
	}
	res, ack := mcpCall(t, shop, "ack_events", map[string]any{"up_to_id": southEv})
	if res.IsError || int64(ack["cursor"].(float64)) != southEv {
		t.Fatalf("ack_events：%s %v", mcpText(res), ack)
	}
	_, out = mcpCall(t, shop, "list_events", nil)
	if ids := eventIDs(out); len(ids) != 1 || ids[0] != shopEv || out["has_more"] != false {
		t.Fatalf("ack 之后：%v", out)
	}
	// 游标只进不退；不存在的 id 被拒。
	_, ack = mcpCall(t, shop, "ack_events", map[string]any{"up_to_id": refundEv})
	if int64(ack["cursor"].(float64)) != southEv {
		t.Fatalf("往回 ack 把游标拨回去了：%v", ack)
	}
	res, _ = mcpCall(t, shop, "ack_events", map[string]any{"up_to_id": shopEv + 1000000})
	if !res.IsError || !strings.Contains(mcpText(res), "invalid-request") {
		t.Fatalf("ack 一个不存在的 id 应 invalid-request：%q", mcpText(res))
	}
	// 游标是每名 AI 员工各一份：北京店 AI 的没动。
	_, out = mcpCall(t, north, "list_events", nil)
	if out["cursor"].(float64) != 0 {
		t.Fatalf("北京店 AI 的游标被别人的 ack 动了：%v", out)
	}
}

// 提案有了结果：驳回、批准执行、过期各一条 proposal_decided（门店 = 提案的门店）。
func TestAgentEventsProposalDecided(t *testing.T) {
	cs := newCouponShop(t)
	a := createAgent(t, cs.adminShop, `{"name":"AI 店长","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	propose := func(store, sku int64) int64 {
		t.Helper()
		res, p := mcpCall(t, sess, "propose_inventory_adjust", map[string]any{"store_id": store, "sku_id": sku, "delta": 5,
			"reason": "补货", "evidence": "restock_plan 显示需要补 5 件"})
		if res.IsError {
			t.Fatal(mcpText(res))
		}
		return int64(p["id"].(float64))
	}
	rejected := propose(cs.NorthStore, cs.DressSKU)
	wantStatus(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/reject", rejected), `{"reason":"不补"}`, cs.Token),
		http.StatusOK, "驳回")
	executed := propose(cs.NorthStore, cs.ShirtSKU)
	wantStatus(t, post(t, cs.Host, fmt.Sprintf("/api/v1/admin/agent-proposals/%d/approve", executed), "", cs.Token),
		http.StatusOK, "批准")
	expired := propose(cs.SouthStore, cs.DressSKU)
	if _, err := admin(t).Exec(context.Background(), `UPDATE agent_proposals SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExpireProposalsOnce(context.Background(), repository.New(testPool)); err != nil {
		t.Fatal(err)
	}
	// 再扫一次不重复写。
	if _, err := service.ExpireProposalsOnce(context.Background(), repository.New(testPool)); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{rejected: "rejected", executed: "executed", expired: "expired"}
	rows, err := adminSession(t).Query(context.Background(), `SELECT (payload->>'proposal_id')::bigint, payload->>'status', store_id
		FROM agent_events WHERE merchant_id = $1 AND type = 'proposal_decided'`, cs.MerchantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, store int64
		var st string
		if err := rows.Scan(&id, &st, &store); err != nil {
			t.Fatal(err)
		}
		n++
		if want[id] != st {
			t.Errorf("提案 #%d 的事件状态 %q，期望 %q", id, st, want[id])
		}
		if (id == expired) != (store == cs.SouthStore) {
			t.Errorf("提案 #%d 的事件门店 %d", id, store)
		}
	}
	if n != 3 {
		t.Fatalf("proposal_decided 事件 %d 条，期望 3", n)
	}
}

// 扫描：两家店的连衣裙卖空 → 各一条 stock_low；再扫不重复。无结果词近 1 小时 5 次 → 一条 search_zero_spike，再扫不重复。
func TestAgentEventSweepStockLowOnce(t *testing.T) {
	cs := newCouponShop(t)
	setStoreStock(t, cs.adminShop, cs.NorthStore, cs.DressSKU, 0)
	setStoreStock(t, cs.adminShop, cs.SouthStore, cs.DressSKU, 0)
	for i := 0; i < 5; i++ {
		adminExec(t, `INSERT INTO search_logs (merchant_id, query, ranked_ids, trace_id, strategy, stages)
			VALUES ($1, ' 泳衣 ', '{}', $2, 'hybrid', '{keyword}')`, cs.MerchantID, fmt.Sprintf("evt-%s-%d", cs.Suffix, i))
	}
	sweep := service.NewAgentEventSweepService(repository.New(testPool), testInvLocal, nil)
	rep := sweep.SweepMerchant(context.Background(), cs.MerchantID)
	if rep.StockLow < 2 || rep.SearchZeroSpike != 1 {
		t.Fatalf("第一轮扫描：%+v", rep)
	}
	var avail, threshold float64
	if err := admin(t).QueryRow(context.Background(), `SELECT (payload->>'available')::float8, (payload->>'threshold')::float8
		FROM agent_events WHERE merchant_id = $1 AND type = 'stock_low' AND store_id = $2 AND (payload->>'sku_id')::bigint = $3`,
		cs.MerchantID, cs.NorthStore, cs.DressSKU).Scan(&avail, &threshold); err != nil || avail != 0 || avail > threshold {
		t.Fatalf("北京门店连衣裙的 stock_low：available=%v threshold=%v err=%v", avail, threshold, err)
	}
	if rep := sweep.SweepMerchant(context.Background(), cs.MerchantID); rep.StockLow != 0 || rep.SearchZeroSpike != 0 {
		t.Fatalf("第二轮扫描又写了：%+v", rep)
	}
	var n int
	var q string
	if err := admin(t).QueryRow(context.Background(), `SELECT count(*), max(payload->>'query') FROM agent_events
		WHERE merchant_id = $1 AND type = 'search_zero_spike'`, cs.MerchantID).Scan(&n, &q); err != nil || n != 1 || q != "泳衣" {
		t.Fatalf("search_zero_spike：%d 条，词 %q（err=%v）", n, q, err)
	}
}

type hookHit struct {
	event, id, sig string
	body           []byte
}

// webhook：配置（密钥只回一次、地址校验）→ 事件投递到 httptest 服务，签名可验；第一次 500 → 重试成功，
// 两次尝试都有记录；范围外的事件（广州门店）不推给北京店 AI。
func TestAgentWebhookDelivery(t *testing.T) {
	cs := newCouponShop(t)
	northAI := createAgent(t, cs.adminShop, fmt.Sprintf(`{"name":"北京店 AI","role":4,"store_ids":[%d]}`, cs.NorthStore))
	path := fmt.Sprintf("/api/v1/admin/agents/%d/webhook", northAI.Id)

	var mu sync.Mutex
	var hits []hookHit
	fail := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, hookHit{r.Header.Get("X-Keel-Event"), r.Header.Get("X-Keel-Event-Id"), r.Header.Get("X-Keel-Signature"), b})
		if fail {
			fail = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	// 没配时 GET 404；地址校验：非 https（除本机）、带账号密码 → 422。
	wantStatus(t, getAs(t, cs.Host, path, cs.Token), http.StatusNotFound, "没配 webhook")
	for _, bad := range []string{`{"url":"http://hooks.example.com/x"}`, `{"url":"ftp://x.example.com"}`,
		`{"url":"https://u:p@hooks.example.com"}`, `{"url":""}`} {
		wantStatus(t, putAs(t, cs.Host, path, bad, cs.Token), http.StatusUnprocessableEntity, "坏地址 "+bad)
	}
	created := putAgentWebhook(t, cs.adminShop, northAI.Id, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if created.Secret == nil || !strings.HasPrefix(*created.Secret, "kwhs_") || !created.Enabled {
		t.Fatalf("新建 webhook 应回密钥：%+v", created)
	}
	secret := *created.Secret
	// 再 PUT（不轮换）不回密钥；GET 永远不回。
	if again := putAgentWebhook(t, cs.adminShop, northAI.Id, fmt.Sprintf(`{"url":%q}`, srv.URL)); again.Secret != nil || again.Id != created.Id {
		t.Fatalf("改 webhook 不该回密钥：%+v", again)
	}

	// 北京门店的售后申请（范围内）与广州门店的库存预警（范围外）。
	refundOnNorth(t, cs, "hook")
	setStoreStock(t, cs.adminShop, cs.SouthStore, cs.DressSKU, 0)
	if _, err := admin(t).Exec(context.Background(), `UPDATE inventories SET available_qty = 100 WHERE merchant_id = $1 AND store_id = $2`,
		cs.MerchantID, cs.NorthStore); err != nil {
		t.Fatal(err)
	}
	service.NewAgentEventSweepService(repository.New(testPool), testInvLocal, nil).SweepMerchant(context.Background(), cs.MerchantID)

	worker := service.NewAgentWebhookDeliveryService(repository.New(testPool), nil, nil)
	rep, err := worker.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Retried != 1 || rep.Skipped < 1 {
		t.Fatalf("第一轮投递：%+v（期望 1 次退避、广州的跳过）", rep)
	}
	// 退避中的任务拨到现在，再跑一轮。
	if _, err := admin(t).Exec(context.Background(), `UPDATE jobs SET run_after = now() WHERE merchant_id = $1 AND queue = $2 AND status = 0`,
		cs.MerchantID, repository.QueueAgentEventDelivery); err != nil {
		t.Fatal(err)
	}
	if rep, err = worker.Drain(context.Background()); err != nil || rep.Delivered != 1 {
		t.Fatalf("重试：%+v err=%v", rep, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("webhook 收到 %d 次，期望 2（一次 500、一次成功；广州的库存预警不该推）", len(hits))
	}
	for _, h := range hits {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write(h.body)
		if h.sig != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			t.Fatalf("签名对不上：%s", h.sig)
		}
		var body struct {
			ID      int64          `json:"id"`
			Type    string         `json:"type"`
			StoreID *int64         `json:"store_id"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(h.body, &body); err != nil || h.event != "refund_created" || body.Type != "refund_created" ||
			fmt.Sprint(body.ID) != h.id || body.StoreID == nil || *body.StoreID != cs.NorthStore || body.Payload["refund_no"] == nil {
			t.Fatalf("投递内容：headers=%s/%s body=%s", h.event, h.id, h.body)
		}
	}
	// 后台 GET：不含密钥，两次尝试（新的在前：204 成功、500 失败）。
	var got api.AgentWebhook
	decodeInto(t, getAs(t, cs.Host, path, cs.Token), http.StatusOK, "看 webhook", &got)
	if got.Secret != nil || got.RecentDeliveries == nil || len(*got.RecentDeliveries) != 2 {
		t.Fatalf("GET webhook：%+v", got)
	}
	ds := *got.RecentDeliveries
	if ds[0].StatusCode == nil || *ds[0].StatusCode != 204 || ds[0].Attempt != 2 || ds[1].StatusCode == nil ||
		*ds[1].StatusCode != 500 || ds[1].Error == "" {
		t.Fatalf("投递记录：%+v", ds)
	}
	// 轮换：回一把新密钥。删除：之后 GET 404。
	if rot := putAgentWebhook(t, cs.adminShop, northAI.Id, fmt.Sprintf(`{"url":%q,"rotate_secret":true,"enabled":false}`, srv.URL)); rot.Secret == nil || *rot.Secret == secret || rot.Enabled {
		t.Fatalf("轮换：%+v", rot)
	}
	wantStatus(t, deleteAs(t, cs.Host, path, cs.Token), http.StatusNoContent, "删 webhook")
	wantStatus(t, getAs(t, cs.Host, path, cs.Token), http.StatusNotFound, "删了之后")
}
