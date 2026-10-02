package handler_test

// 第三期 Task 9：渠道订单对真实开发店的联调（KEEL_SHOPIFY_LIVE=1 才跑，凭据读 ~/.config/keel/shopify-dev，不打印）。
// 用户批准用 Admin API orderCreate 建 test 单（test:true、PAID）：
//
//  1. 开发店下 1 件（DECREMENT_OBEYING_POLICY，Shopify 自己减库存）→ 按回调的样子喂 orders/create → keel 订单 20、
//     来源 1、实付对上、keel 库存 −1、推送没有 CAS 冲突（Review Focus 3）；
//  2. keel 后台发货（顺丰 + KEELTEST 单号）→ 回传 → Shopify 上有带这个单号的 fulfillment、FO 都 CLOSED；
//  3. 第二张单在 Shopify 上取消（restock、退款）→ 喂 orders/cancelled → keel 订单 60、refunded = paid、库存回补。
//
// 结束时（t.Cleanup）：没走完的测试单取消、探查单 #1001 取消（restock:false）、开发店上这个变体的数还原到开始时的值。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

const (
	liveShopLocation = "gid://shopify/Location/87516708954"
	liveProbeOrder   = "gid://shopify/Order/18918965084250" // 只读探查时建的 #1001（test、PAID、BYPASS、未发货）
)

// liveAdmin 是联调用的裸 GraphQL 客户端（建单 / 取消 / 回读 / 还原库存这些适配器不做的事）。
type liveAdmin struct {
	shop, id, secret string
	mu               sync.Mutex
	tok              string
}

func (c *liveAdmin) token(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" {
		return c.tok
	}
	body, _ := json.Marshal(map[string]string{"client_id": c.id, "client_secret": c.secret, "grant_type": "client_credentials"})
	resp, err := http.Post("https://"+c.shop+"/admin/oauth/access_token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("换 token 没连上：%v", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AccessToken == "" {
		t.Fatalf("换 token 失败：HTTP %d", resp.StatusCode)
	}
	c.tok = out.AccessToken
	return c.tok
}

func (c *liveAdmin) gql(t *testing.T, query string, vars map[string]any, out any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest(http.MethodPost, "https://"+c.shop+"/admin/api/"+shopify.APIVersion+"/graphql.json", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shopify-Access-Token", c.token(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GraphQL 没连上：%v", err)
	}
	defer resp.Body.Close()
	var r struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || resp.StatusCode != http.StatusOK || len(r.Errors) > 0 {
		t.Fatalf("GraphQL 出错：HTTP %d %v %+v", resp.StatusCode, err, r.Errors)
	}
	if out != nil {
		if err := json.Unmarshal(r.Data, out); err != nil {
			t.Fatal(err)
		}
	}
}

func mustNoUserErrors(t *testing.T, what string, errs []map[string]any) {
	t.Helper()
	if len(errs) > 0 {
		t.Fatalf("%s 的 userErrors：%v", what, errs)
	}
}

// createTestOrder：test:true、PAID、1 件、带收货地址与 5 美元运费；Shopify 按库存策略自己减库存。
func (c *liveAdmin) createTestOrder(t *testing.T, variant string) (gid, name string) {
	t.Helper()
	var out struct {
		R struct {
			Order *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"order"`
			UserErrors []map[string]any `json:"userErrors"`
		} `json:"orderCreate"`
	}
	c.gql(t, `mutation OrderCreate($order:OrderCreateOrderInput!,$options:OrderCreateOptionsInput){
	  orderCreate(order:$order, options:$options){ order{ id name } userErrors{ field message } } }`, map[string]any{
		"order": map[string]any{"test": true, "financialStatus": "PAID", "note": "keel 联调测试单（自动建、自动收尾）",
			"lineItems": []map[string]any{{"variantId": variant, "quantity": 1}},
			"shippingAddress": map[string]any{"firstName": "Keel", "lastName": "Live", "address1": "1 Main St", "city": "Brooklyn",
				"provinceCode": "NY", "zip": "11201", "countryCode": "US", "phone": "+12025550123"},
			"shippingLines": []map[string]any{{"title": "Standard",
				"priceSet": map[string]any{"shopMoney": map[string]any{"amount": "5.00", "currencyCode": "USD"}}}}},
		"options": map[string]any{"inventoryBehaviour": "DECREMENT_OBEYING_POLICY", "sendReceipt": false, "sendFulfillmentReceipt": false},
	}, &out)
	mustNoUserErrors(t, "orderCreate", out.R.UserErrors)
	if out.R.Order == nil {
		t.Fatal("orderCreate 没回订单")
	}
	return out.R.Order.ID, out.R.Order.Name
}

type liveOrder struct {
	Name          string     `json:"name"`
	CancelledAt   *time.Time `json:"cancelledAt"`
	TaxesIncluded bool       `json:"taxesIncluded"`
	Financial     string     `json:"displayFinancialStatus"`
	Total         struct {
		ShopMoney struct {
			Amount string `json:"amount"`
		} `json:"shopMoney"`
	} `json:"totalPriceSet"`
	Tax struct {
		ShopMoney struct {
			Amount string `json:"amount"`
		} `json:"shopMoney"`
	} `json:"totalTaxSet"`
	FOs struct {
		Nodes []struct {
			Status string `json:"status"`
		} `json:"nodes"`
	} `json:"fulfillmentOrders"`
	Fulfillments []struct {
		Status       string `json:"status"`
		TrackingInfo []struct {
			Company string `json:"company"`
			Number  string `json:"number"`
		} `json:"trackingInfo"`
	} `json:"fulfillments"`
}

func (c *liveAdmin) order(t *testing.T, gid string) liveOrder {
	t.Helper()
	var out struct {
		Order *liveOrder `json:"order"`
	}
	c.gql(t, `query($id:ID!){ order(id:$id){ name cancelledAt taxesIncluded displayFinancialStatus
	  totalPriceSet{ shopMoney{ amount } } totalTaxSet{ shopMoney{ amount } }
	  fulfillmentOrders(first:10){ nodes{ status } }
	  fulfillments(first:10){ status trackingInfo(first:5){ company number } } } }`, map[string]any{"id": gid}, &out)
	if out.Order == nil {
		t.Fatalf("Shopify 上没有订单 %s", gid)
	}
	return *out.Order
}

// cancel：orderCancel 是异步的（回 job），轮询到 cancelledAt 有值。refund 为真时按原支付方式全退。
func (c *liveAdmin) cancel(t *testing.T, gid string, restock, refund bool) {
	t.Helper()
	var out struct {
		R struct {
			Job *struct {
				ID string `json:"id"`
			} `json:"job"`
			UserErrors []map[string]any `json:"orderCancelUserErrors"`
		} `json:"orderCancel"`
	}
	vars := map[string]any{"id": gid, "restock": restock}
	q := `mutation OrderCancel($id:ID!,$restock:Boolean!){ orderCancel(orderId:$id, reason:OTHER, restock:$restock, notifyCustomer:false,
	  staffNote:"keel 联调收尾"){ job{ id } orderCancelUserErrors{ field message code } } }`
	if refund {
		q = `mutation OrderCancel($id:ID!,$restock:Boolean!){ orderCancel(orderId:$id, reason:OTHER, restock:$restock, notifyCustomer:false,
		  staffNote:"keel 联调", refundMethod:{originalPaymentMethodsRefund:true}){ job{ id } orderCancelUserErrors{ field message code } } }`
	}
	c.gql(t, q, vars, &out)
	mustNoUserErrors(t, "orderCancel", out.R.UserErrors)
	deadline := time.Now().Add(60 * time.Second)
	for c.order(t, gid).CancelledAt == nil {
		if time.Now().After(deadline) {
			t.Fatalf("orderCancel %s 等了 60 秒还没生效", gid)
		}
		time.Sleep(time.Second)
	}
}

func (c *liveAdmin) available(t *testing.T, item string) int32 {
	t.Helper()
	var out struct {
		Item struct {
			Level *struct {
				Quantities []struct {
					Quantity int32 `json:"quantity"`
				} `json:"quantities"`
			} `json:"inventoryLevel"`
		} `json:"inventoryItem"`
	}
	c.gql(t, `query($id:ID!,$loc:ID!){ inventoryItem(id:$id){ inventoryLevel(locationId:$loc){ quantities(names:["available"]){ quantity } } } }`,
		map[string]any{"id": item, "loc": liveShopLocation}, &out)
	if out.Item.Level == nil || len(out.Item.Level.Quantities) == 0 {
		t.Fatalf("回读不到 %s 在 Shop location 的数", item)
	}
	return out.Item.Level.Quantities[0].Quantity
}

// liveWebhook 按 Shopify 回调的样子造一条（正文带 id 与 admin_graphql_api_id，签名用 client secret）。
func liveWebhook(kv map[string]string, topic, gid string) (*http.Request, []byte) {
	body, _ := json.Marshal(map[string]any{"id": json.Number(strings.TrimPrefix(gid, "gid://shopify/Order/")), "admin_graphql_api_id": gid})
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	req.Header.Set(shopify.HeaderHMAC, shopify.Sign(kv["SHOPIFY_CLIENT_SECRET"], body))
	req.Header.Set(shopify.HeaderTopic, topic)
	req.Header.Set(shopify.HeaderShop, kv["SHOPIFY_SHOP"])
	req.Header.Set(shopify.HeaderEventID, fmt.Sprintf("keel-live-%s-%d", topic, time.Now().UnixNano()))
	return req, body
}

func TestShopifyOrdersLive(t *testing.T) {
	kv := liveShopifyCreds(t)
	sh := &liveAdmin{shop: kv["SHOPIFY_SHOP"], id: kv["SHOPIFY_CLIENT_ID"], secret: kv["SHOPIFY_CLIENT_SECRET"]}
	cs := newCouponShop(t)
	cleanupChannelOrders(t, cs.MerchantID)
	adapter := shopify.New(shopify.Options{})
	rig := newChannelRigWith(t, adapter)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	drain := func() {
		t.Helper()
		for i := 0; i < 5; i++ {
			if err := rig.svc.Drain(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	sec, _ := json.Marshal(map[string]string{"client_id": kv["SHOPIFY_CLIENT_ID"], "client_secret": kv["SHOPIFY_CLIENT_SECRET"]})
	cfg, _ := json.Marshal(map[string]int64{"default_category_id": cs.ChildCat})
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: shopify.Kind, ExternalAccount: kv["SHOPIFY_SHOP"],
		Name: "开发店", Roles: channel.RoleCatalogSource | channel.RoleOutlet, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: liveShopLocation}); err != nil {
		t.Fatal(err)
	}
	on := repository.ChannelBindingActive
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &on}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for adminQueryInt64(t, `SELECT count(*) FROM channel_merchants WHERE merchant_id = $1 AND enabled`, cs.MerchantID) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("库存服务没记下这家开了渠道")
		}
		time.Sleep(20 * time.Millisecond)
	}
	drain()
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue LIKE 'channel.%' AND status IN (0, 3)`, cs.MerchantID); n != 0 {
		t.Fatalf("拉完商品还有 %d 条渠道任务没做完：%s", n, adminQueryString(t, `SELECT coalesce(string_agg(queue || ': ' ||
			coalesce(last_error, ''), ' | '), '') FROM jobs WHERE merchant_id = $1 AND queue LIKE 'channel.%' AND status IN (0, 3)`, cs.MerchantID))
	}

	// 挑一个跟踪库存、北店（= Shop location）keel 库存 ≥ 2 的规格。
	var sku int64
	var variant, item string
	if err := admin(t).QueryRow(context.Background(), `SELECT l.keel_id, l.external_id, l.extra->>'inventory_item_id'
	   FROM channel_item_links l JOIN inventories i ON i.sku_id = l.keel_id AND i.store_id = $2
	  WHERE l.binding_id = $1 AND l.kind = 2 AND l.extra->>'tracked' = 'true' AND i.available_qty >= 2
	  ORDER BY i.available_qty DESC, l.keel_id LIMIT 1`, b.ID, cs.NorthStore).Scan(&sku, &variant, &item); err != nil {
		t.Fatalf("挑规格（跟踪库存、Shop location ≥ 2 件）：%v", err)
	}
	stock := func() int64 {
		return adminQueryInt64(t, `SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`, cs.NorthStore, sku)
	}
	start := sh.available(t, item)
	keelStart := stock()
	if int64(start) != keelStart {
		t.Fatalf("拉完商品 keel 库存 %d、Shopify 上 %d，应当相等", keelStart, start)
	}
	t.Logf("变体 %s：开始时 Shopify / keel 都是 %d", variant, start)

	// 库存只经 keel 自己的推送还原（不直接改 Shopify 上的数）：没走完的单取消后也喂给 keel，让推送基线跟着平台走。
	var created []string
	var feed func(topic, gid string)
	var waitShop func(want int32, what string)
	t.Cleanup(func() {
		for _, gid := range created {
			o := sh.order(t, gid)
			if o.CancelledAt == nil && len(o.Fulfillments) == 0 {
				sh.cancel(t, gid, true, true)
				feed("orders/cancelled", gid)
				t.Logf("收尾：取消测试单 %s（restock）并喂给 keel", o.Name)
			}
		}
		if _, err := service.NewInventoryOutboxService(repository.New(testPool), rig.local, service.InventoryOutboxConfig{}, nil).
			Drain(context.Background()); err != nil {
			t.Error(err)
		}
		if o := sh.order(t, liveProbeOrder); o.CancelledAt == nil {
			sh.cancel(t, liveProbeOrder, false, false)
			t.Logf("收尾：取消探查单 %s（restock:false）", o.Name)
		}
		drain()
		now := sh.available(t, item)
		if now != start {
			adjust(t, rig.local, cs.MerchantID, cs.NorthStore, sku, start-now)
			waitShop(start, "收尾还原")
		}
		t.Logf("收尾：变体 %s 在 Shopify 上 %d → %d（开始时 %d）", variant, now, sh.available(t, item), start)
	})

	feed = func(topic, gid string) {
		t.Helper()
		req, body := liveWebhook(kv, topic, gid)
		if _, err := rig.svc.Inbound(ctx, b.ID, req, body); err != nil {
			t.Fatal(err)
		}
		drain()
	}
	waitShop = func(want int32, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for q := sh.available(t, item); q != want; q = sh.available(t, item) {
			if time.Now().After(deadline) {
				t.Fatalf("%s：Shopify 上 %d，期望 %d", what, q, want)
			}
			time.Sleep(time.Second)
		}
	}
	listing := func(what string, want int32) {
		t.Helper()
		drain()
		if got := adminQueryString(t, `SELECT published_qty || ':' || coalesce(last_error, '') FROM channel_listings
		   WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, b.ID, cs.NorthStore, sku); got != fmt.Sprintf("%d:", want) {
			t.Fatalf("%s：推送基线:错误 = %q，期望 %d:（Review Focus 3）", what, got, want)
		}
		if q := sh.available(t, item); q != want {
			t.Fatalf("%s：Shopify 上 %d，期望 %d", what, q, want)
		}
	}
	cents := func(s string) int64 {
		var d, c int64
		parts := strings.SplitN(s, ".", 2)
		fmt.Sscan(parts[0], &d)
		if len(parts) == 2 {
			fs := (parts[1] + "00")[:2]
			fmt.Sscan(fs, &c)
		}
		return d*100 + c
	}

	// ---- 1. 下单 → keel 接单扣库存 ----
	gid1, name1 := sh.createTestOrder(t, variant)
	created = append(created, gid1)
	waitShop(start-1, "Shopify 下单后自己减 1")
	feed("orders/create", gid1)
	co1 := adminQueryInt64(t, `SELECT coalesce((SELECT id FROM channel_orders WHERE binding_id = $1 AND external_order_id = $2), 0)`, b.ID, gid1)
	if co1 == 0 {
		t.Fatalf("%s 没进 channel_orders", name1)
	}
	var status, source int16
	var payable, paid int64
	var no string
	if err := admin(t).QueryRow(context.Background(), `SELECT status, source, payable_cents, paid_cents, order_no FROM orders WHERE channel_order_id = $1`,
		co1).Scan(&status, &source, &payable, &paid, &no); err != nil {
		t.Fatalf("%s 没建 keel 订单：%v；渠道单 %s", name1, err, adminQueryString(t,
			`SELECT status || ' ' || coalesce(exception, '') || ' ' || platform_status FROM channel_orders WHERE id = $1`, co1))
	}
	o1 := sh.order(t, gid1)
	want := cents(o1.Total.ShopMoney.Amount)
	if !o1.TaxesIncluded {
		want -= cents(o1.Tax.ShopMoney.Amount)
	}
	if status != 20 || source != 1 || payable != want || paid != want {
		t.Fatalf("%s → keel %s status=%d source=%d payable=%d paid=%d，期望 20/1/%d（Shopify 总价 %s 税 %s 价内税=%v）",
			name1, no, status, source, payable, paid, want, o1.Total.ShopMoney.Amount, o1.Tax.ShopMoney.Amount, o1.TaxesIncluded)
	}
	if got := stock(); got != keelStart-1 {
		t.Fatalf("keel 库存 %d，期望 %d", got, keelStart-1)
	}
	listing("接单后", start-1)
	t.Logf("1. %s（%s，总价 %s 税 %s 价内税=%v）→ keel %s：status 20、实付 %d 分、keel 库存 %d → %d、推送无冲突",
		name1, o1.Financial, o1.Total.ShopMoney.Amount, o1.Tax.ShopMoney.Amount, o1.TaxesIncluded, no, paid, keelStart, stock())

	// ---- 2. keel 后台发货 → Shopify 上看到单号 ----
	tracking := fmt.Sprintf("KEELTEST%d", time.Now().Unix())
	mid := cs.MerchantID
	sctx := auth.NewStaffContext(ctx, auth.StaffIdentity{StaffID: cs.StaffID, MerchantID: &mid, Role: auth.StaffRoleAdmin, Status: auth.StaffStatusActive})
	if _, _, err := service.NewAdminOrderService(repository.New(testPool)).WithChannels(rig.svc).
		Ship(sctx, no, service.ShipRequest{CarrierCode: "sf", TrackingNo: tracking}, "ship-"+uniqueKey()); err != nil {
		t.Fatal(err)
	}
	drain()
	if got := adminQueryString(t, `SELECT status || ':' || coalesce(last_error, '') FROM jobs WHERE queue = $1 AND job_key = $2`,
		service.QueueChannelOrderAction, fmt.Sprintf("act:%d:ship", co1)); got != "2:" {
		t.Fatalf("发货回传任务 状态:错误 = %q，期望 2:", got)
	}
	o1 = sh.order(t, gid1)
	found := false
	for _, f := range o1.Fulfillments {
		for _, ti := range f.TrackingInfo {
			found = found || (ti.Number == tracking && f.Status == "SUCCESS")
		}
	}
	for _, fo := range o1.FOs.Nodes {
		if fo.Status != "CLOSED" {
			t.Fatalf("发货后 FO 状态 %s，期望 CLOSED", fo.Status)
		}
	}
	if !found {
		t.Fatalf("Shopify 上 %s 的 fulfillment = %+v，没有单号 %s", name1, o1.Fulfillments, tracking)
	}
	t.Logf("2. keel 发货（sf / %s）→ Shopify %s：fulfillment %+v，FO 全 CLOSED", tracking, name1, o1.Fulfillments)

	// ---- 3. 第二张单在 Shopify 上取消 → keel 整单退款、回补 ----
	before := stock()
	gid2, name2 := sh.createTestOrder(t, variant)
	created = append(created, gid2)
	waitShop(start-2, "第二张单下单后")
	feed("orders/create", gid2)
	co2 := adminQueryInt64(t, `SELECT id FROM channel_orders WHERE binding_id = $1 AND external_order_id = $2`, b.ID, gid2)
	no2 := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1 AND status = 20`, co2)
	if got := stock(); got != before-1 {
		t.Fatalf("第二张单接单后 keel 库存 %d，期望 %d", got, before-1)
	}
	listing("第二张单接单后", start-2)
	sh.cancel(t, gid2, true, true)
	waitShop(start-1, "Shopify 取消（restock）后")
	o2 := sh.order(t, gid2)
	feed("orders/cancelled", gid2)
	if _, err := service.NewInventoryOutboxService(repository.New(testPool), rig.local, service.InventoryOutboxConfig{}, nil).
		Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	var paid2, refunded2 int64
	if err := admin(t).QueryRow(context.Background(), `SELECT status, paid_cents, refunded_cents FROM orders WHERE order_no = $1`, no2).
		Scan(&status, &paid2, &refunded2); err != nil {
		t.Fatal(err)
	}
	if status != 60 || refunded2 != paid2 || paid2 == 0 {
		t.Fatalf("%s 取消后 keel %s status=%d paid=%d refunded=%d；渠道单 %s", name2, no2, status, paid2, refunded2,
			adminQueryString(t, `SELECT status || ' ' || coalesce(exception, '') || ' ' || platform_status FROM channel_orders WHERE id = $1`, co2))
	}
	if got := stock(); got != before {
		t.Fatalf("取消后 keel 库存 %d，期望回补到 %d", got, before)
	}
	listing("取消回补后", start-1)
	t.Logf("3. %s 在 Shopify 上取消（%s）→ keel %s：status 60、refunded = paid = %d、keel 库存回到 %d、推送无冲突",
		name2, o2.Financial, no2, paid2, stock())
}
