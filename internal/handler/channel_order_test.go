package handler_test

// 第三期 Task 4：渠道单收单与接单 SAGA（打 Shopify 模拟平台 + 真实适配器 + 真库；版本守卫与接单类能力用假适配器）。
// Review Focus 1、2、3、4、7、8 各有一条。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/channeltest"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// cleanupChannelOrders 要排在 newCouponShop 的清理之前（t.Cleanup 后进先出）：channel_orders 与 orders 互相引用。
func cleanupChannelOrders(t *testing.T, merchantID int64) {
	t.Cleanup(func() {
		adminExec(t, `UPDATE orders SET channel_order_id = NULL WHERE merchant_id = $1`, merchantID)
		adminExec(t, `DELETE FROM channel_order_requests WHERE merchant_id = $1`, merchantID)
		adminExec(t, `DELETE FROM channel_orders WHERE merchant_id = $1`, merchantID)
	})
}

type orderRig struct {
	*shopifyRig
	variant, item string
	sku           int64
}

// newOrderRig：一件单规格商品（19.90，模拟店北店 location 上 levels 件）接进 keel、binding 启用。
func newOrderRig(t *testing.T, split bool, levels int32) *orderRig {
	t.Helper()
	r := newShopifyRigOpts(t, map[string]any{}, split)
	adminExec(t, `UPDATE channel_bindings SET config = jsonb_build_object('default_category_id', $1::bigint) WHERE id = $2`, r.cs.ChildCat, r.b.ID)
	cleanupChannelOrders(t, r.cs.MerchantID)
	pg := r.sim.AddProduct(shopifytest.Product{Title: "渠道 T 恤", Variants: []shopifytest.Variant{
		{SKU: fmt.Sprintf("CO-%d", r.b.ID), Price: "19.90", Options: map[string]string{"颜色": "红"}, Tracked: true,
			Levels: map[string]int32{r.loc: levels}}}})
	r.activate(t)
	v := r.sim.VariantIDs(pg)[0]
	o := &orderRig{shopifyRig: r, variant: v, item: r.sim.InventoryItem(v), sku: r.keelSKU(t, v)}
	if got := o.stock(t, o.sku); got != int64(levels) {
		t.Fatalf("首拉后 keel 库存 %d，期望 %d", got, levels)
	}
	return o
}

// orderWebhook 投一条模拟店造的订单回调并跑完。
func (r *orderRig) orderWebhook(t *testing.T, orderGID, topic string) {
	t.Helper()
	req, raw := r.sim.WebhookFor(orderGID, topic)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *orderRig) channelOrderID(t *testing.T, externalID string) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT id FROM channel_orders WHERE binding_id = $1 AND external_order_id = $2`, r.b.ID, externalID)
}

func keelOrdersOf(t *testing.T, channelOrderID int64) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1`, channelOrderID)
}

func (r *orderRig) twoPieces() string {
	return r.sim.AddOrder(shopifytest.OrderSpec{Location: r.loc,
		Lines: []shopifytest.OrderLineSpec{{Variant: r.variant, Qty: 2}}})
}

// 正常单：金额对上（keel 实付 = 平台总价 − 税）、没有买家、来源 1、收货人进快照；后台 status=20 看得到，买家看不到。
// 同一条里 Review Focus 3：接单后推送基线跟着减，没有一次多余的推送（更没有冲突）。
func TestChannelOrderBecomesPaidKeelOrder(t *testing.T) {
	r := newOrderRig(t, false, 7)
	pushes := r.sim.Calls("SetQty")
	gid := r.sim.AddOrder(shopifytest.OrderSpec{Location: r.loc, Lines: []shopifytest.OrderLineSpec{{Variant: r.variant, Qty: 2}},
		Shipping: "5.00", Discount: "3.00", Tax: "2.50", Address: &shopifytest.Address{Name: "Jane Doe", Phone: "+12025550123",
			Address1: "1 Main St", City: "Brooklyn", Province: "NY", Zip: "11201", CountryCode: "US"}})
	if q, _ := r.sim.Available(r.item, r.loc); q != 5 {
		t.Fatalf("模拟店卖出 2 件后 available %d，期望 5", q)
	}
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)

	var (
		status, source                          int16
		userNull                                bool
		payable, paid, goods, freight, discount int64
		receiver, orderNo                       string
	)
	if err := admin(t).QueryRow(context.Background(), `SELECT status, source, user_id IS NULL, payable_cents, paid_cents,
	        goods_amount_cents, freight_cents, discount_cents, receiver_snapshot->>'receiver_name', order_no
	   FROM orders WHERE channel_order_id = $1`, co).Scan(&status, &source, &userNull, &payable, &paid, &goods, &freight,
		&discount, &receiver, &orderNo); err != nil {
		t.Fatal(err)
	}
	// 平台总价 = 39.80 + 5.00 − 3.00 + 2.50 = 44.30；keel 实付不含税 = 41.80。
	if status != 20 || source != 1 || !userNull || payable != 4180 || paid != 4180 || goods != 3980 || freight != 500 ||
		discount != 300 || receiver != "Jane Doe" {
		t.Fatalf("keel 订单 status=%d source=%d user空=%v payable=%d paid=%d goods=%d freight=%d discount=%d 收货人=%q",
			status, source, userNull, payable, paid, goods, freight, discount, receiver)
	}
	if got := adminQueryString(t, `SELECT status || ':' || coalesce(order_no, '') || ':' || (amounts->>'tax')
	                                 FROM channel_orders WHERE id = $1`, co); got != "3:"+orderNo+":250" {
		t.Fatalf("渠道单 状态:单号:税 = %q", got)
	}
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("keel 库存 %d，期望 7 − 2 = 5", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_order_paid'`, orderNo); n != 1 {
		t.Fatalf("门店的「新订单待发货」%d 条，期望 1", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND audience = 1`, orderNo); n != 0 {
		t.Fatalf("渠道单发了 %d 条买家通知", n)
	}

	// Review Focus 3：基线 7 → 5，keel 算出来也是 5，不用推；模拟店上就是 keel 的对外可售数。
	r.drain(t)
	if n := r.sim.Calls("SetQty"); n != pushes {
		t.Fatalf("接单之后推了 %d 次（基线没跟着减，CAS 会冲突）", n-pushes)
	}
	if q, _ := r.sim.Available(r.item, r.loc); q != 5 {
		t.Fatalf("模拟店 available %d，期望 keel 的 5", q)
	}
	if got := adminQueryString(t, `SELECT published_qty || ':' || coalesce(last_error, '') FROM channel_listings
	                                WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, r.b.ID, r.cs.NorthStore, r.sku); got != "5:" {
		t.Fatalf("推送基线:错误 = %q，期望 5:", got)
	}
	// 之后 keel 再卖 1 件：一次正常的 CAS（changeFrom 5 → 4），不冲突。
	adjust(t, r.local, r.cs.MerchantID, r.cs.NorthStore, r.sku, -1)
	deadline := time.Now().Add(15 * time.Second)
	for q, _ := r.sim.Available(r.item, r.loc); q != 4; q, _ = r.sim.Available(r.item, r.loc) {
		if time.Now().After(deadline) {
			t.Fatalf("keel 减 1 后模拟店仍是 %d", q)
		}
		r.drain(t)
		time.Sleep(20 * time.Millisecond)
	}
	if got := adminQueryString(t, `SELECT coalesce(last_error, '') FROM channel_listings
	                                WHERE binding_id = $1 AND store_id = $2 AND sku_id = $3`, r.b.ID, r.cs.NorthStore, r.sku); got != "" {
		t.Fatalf("推送记了错误 %q", got)
	}

	var page aoOrderPage
	decodeInto(t, getAs(t, r.cs.Host, "/api/v1/admin/orders?page_size=100&status=20", r.cs.Token), http.StatusOK, "后台订单列表", &page)
	found := false
	for _, it := range page.Items {
		found = found || it.OrderNo == orderNo
	}
	if !found {
		t.Fatalf("后台 status=20 的列表里没有渠道单 %s", orderNo)
	}
	b := r.cs.newBuyer(t, "co")
	if w := getAs(t, r.cs.Host, "/api/v1/orders", b.Token); w.Code != http.StatusOK || strings.Contains(w.Body.String(), orderNo) {
		t.Fatalf("买家 GET /orders（%d）看到了渠道单：%s", w.Code, w.Body.String())
	}
}

// Review Focus 1：同一张单的三条回调并发处理 → 一张 keel 订单、库存只扣一次。
func TestChannelOrderConcurrentWebhooksMakeOneOrder(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid := r.twoPieces()
	for _, topic := range []string{"orders/create", "orders/paid", "orders/updated"} {
		req, raw := r.sim.WebhookFor(gid, topic)
		if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
			t.Fatal(err)
		}
	}
	// 三个 worker 同时取：每条事件各自回读、各自进 applyChannelOrder。
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	start := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- r.svc.Drain(r.ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	r.drain(t)
	co := r.channelOrderID(t, gid)
	if n := keelOrdersOf(t, co); n != 1 {
		t.Fatalf("三条回调建了 %d 张 keel 订单", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM inventory_logs WHERE merchant_id = $1 AND sku_id = $2 AND biz_type = 1`,
		r.cs.MerchantID, r.sku); n != 1 {
		t.Fatalf("扣了 %d 次库存", n)
	}
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("keel 库存 %d，期望 5", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatal("那张 keel 订单不是已支付")
	}
}

// Review Focus 8：没付款（AUTHORIZED）不接单；之后 orders/paid → 接单。
func TestChannelOrderWaitsForPayment(t *testing.T) {
	r := newOrderRig(t, false, 7)
	gid := r.sim.AddOrder(shopifytest.OrderSpec{Location: r.loc, Financial: "AUTHORIZED",
		Lines: []shopifytest.OrderLineSpec{{Variant: r.variant, Qty: 1}}})
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("没付款就建了 %d 张 keel 订单", n)
	}
	if st := adminQueryInt64(t, `SELECT status FROM channel_orders WHERE id = $1`, co); st != 1 {
		t.Fatalf("渠道单状态 %d，期望 1 待付款", st)
	}
	r.sim.SetFinancial(gid, "PAID")
	r.orderWebhook(t, gid, "orders/paid")
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("付款后已支付的 keel 订单 %d 张，期望 1", n)
	}
	if got := r.stock(t, r.sku); got != 6 {
		t.Fatalf("keel 库存 %d，期望 6", got)
	}
}

// Review Focus 4：keel 没货 → 不出现 20 的订单、草稿关到 90、库存不动、渠道单标「缺货」、门店收通知、
// Shopify（不需接单）不入队任何动作；补货后「重试」成单、不重复建单。
func TestChannelOrderStockoutClosesThenRetrySucceeds(t *testing.T) {
	r := newOrderRig(t, false, 7)
	adjust(t, r.local, r.cs.MerchantID, r.cs.NorthStore, r.sku, -6) // keel 只剩 1
	r.drain(t)
	gid := r.twoPieces()
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)

	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status <> 90`, co); n != 0 {
		t.Fatalf("缺货还有 %d 张没关的 keel 订单", n)
	}
	if n := keelOrdersOf(t, co); n != 1 {
		t.Fatalf("keel 订单 %d 张，期望 1 张关掉的留痕", n)
	}
	closedNo := adminQueryString(t, `SELECT order_no FROM orders WHERE channel_order_id = $1`, co)
	if got := r.stock(t, r.sku); got != 1 {
		t.Fatalf("缺货关单后 keel 库存 %d，期望不动的 1", got)
	}
	exc := adminQueryString(t, `SELECT coalesce(exception, '') || '|' || coalesce(order_no, '') FROM channel_orders WHERE id = $1`, co)
	if !strings.HasPrefix(exc, "缺货：") || !strings.Contains(exc, "要 2 件") || !strings.HasSuffix(exc, "|") {
		t.Fatalf("渠道单异常|单号 = %q，期望「缺货：… 要 2 件…」且单号清空", exc)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM notifications WHERE order_no = $1 AND kind = 'merchant_channel_order_exception'
	                             AND store_id = $2`, closedNo, r.cs.NorthStore); n != 1 {
		t.Fatalf("门店的缺货通知 %d 条，期望 1", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = $2`, r.cs.MerchantID,
		service.QueueChannelOrderAction); n != 0 {
		t.Fatalf("Shopify 不需要接单 / 不能拒单，却入队了 %d 个动作", n)
	}
	// 同一张单再来一条回调（版本没变）：不重复建单、不重复缺货。
	r.orderWebhook(t, gid, "orders/updated")
	if n := keelOrdersOf(t, co); n != 1 {
		t.Fatalf("有异常时又建了单：%d 张", n)
	}

	adjust(t, r.local, r.cs.MerchantID, r.cs.NorthStore, r.sku, 5) // 补到 6
	if err := r.svc.RetryChannelOrder(r.ctx, co); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("重试后已支付的 keel 订单 %d 张，期望 1", n)
	}
	if n := keelOrdersOf(t, co); n != 2 {
		t.Fatalf("keel 订单共 %d 张，期望关掉的 1 张 + 成单的 1 张", n)
	}
	if got := adminQueryString(t, `SELECT status || ':' || coalesce(exception, '') FROM channel_orders WHERE id = $1`, co); got != "3:" {
		t.Fatalf("重试后渠道单 状态:异常 = %q", got)
	}
	if got := r.stock(t, r.sku); got != 4 {
		t.Fatalf("keel 库存 %d，期望 6 − 2 = 4", got)
	}
	if err := r.svc.RetryChannelOrder(r.ctx, co); !errors.Is(err, service.ErrChannelOrderNotRetryable) {
		t.Fatalf("已成单再重试：%v，期望 ErrChannelOrderNotRetryable", err)
	}
}

// Review Focus 7：行没链到 keel SKU、订单分到没映射的 location → 不建单、原因写清；补了映射后重试成单。
func TestChannelOrderUnmappedLineOrLocation(t *testing.T) {
	r := newOrderRig(t, false, 7)
	extra := adminQueryString(t, `SELECT extra::text FROM channel_item_links WHERE binding_id = $1 AND kind = 2 AND external_id = $2`, r.b.ID, r.variant)
	adminExec(t, `DELETE FROM channel_item_links WHERE binding_id = $1 AND kind = 2 AND external_id = $2`, r.b.ID, r.variant)

	gid := r.twoPieces()
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("行没映射却建了 %d 张 keel 订单", n)
	}
	exc := adminQueryString(t, `SELECT coalesce(exception, '') FROM channel_orders WHERE id = $1`, co)
	if !strings.Contains(exc, "第 1 行") || !strings.Contains(exc, r.variant) {
		t.Fatalf("异常 %q 没写清是哪一行", exc)
	}
	if err := r.svc.LinkSKU(r.ctx, repository.ChannelItemLink{BindingID: r.b.ID, KeelID: r.sku, ExternalID: r.variant,
		Extra: json.RawMessage(extra)}); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.RetryChannelOrder(r.ctx, co); err != nil {
		t.Fatal(err)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("补映射重试后已支付的 keel 订单 %d 张", n)
	}

	loc2 := r.sim.AddLocation("Warehouse 2")
	gid2 := r.sim.AddOrder(shopifytest.OrderSpec{Location: loc2, Lines: []shopifytest.OrderLineSpec{{Variant: r.variant, Qty: 1}}})
	r.orderWebhook(t, gid2, "orders/create")
	co2 := r.channelOrderID(t, gid2)
	if n := keelOrdersOf(t, co2); n != 0 {
		t.Fatalf("门店没映射却建了 %d 张 keel 订单", n)
	}
	if exc := adminQueryString(t, `SELECT coalesce(exception, '') FROM channel_orders WHERE id = $1`, co2); !strings.Contains(exc, loc2) {
		t.Fatalf("异常 %q 没写清是哪个 location", exc)
	}
	if err := r.svc.UpsertStoreLink(r.ctx, repository.ChannelStoreLink{BindingID: r.b.ID, StoreID: r.cs.SouthStore, ExternalStoreID: loc2}); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
	setStoreStock(t, r.cs.adminShop, r.cs.SouthStore, r.sku, 10)
	if err := r.svc.RetryChannelOrder(r.ctx, co2); err != nil {
		t.Fatal(err)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20 AND store_id = $2`,
		co2, r.cs.SouthStore); n != 1 {
		t.Fatalf("补门店映射重试后，南店已支付的 keel 订单 %d 张", n)
	}
}

// 拆分形态（库存独立进程，库存分支经 HTTP）跑一遍正常单。
func TestChannelOrderSplitInventory(t *testing.T) {
	r := newOrderRig(t, true, 7)
	gid := r.twoPieces()
	r.orderWebhook(t, gid, "orders/create")
	co := r.channelOrderID(t, gid)
	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
		t.Fatalf("拆分形态下已支付的 keel 订单 %d 张", n)
	}
	if got := r.stock(t, r.sku); got != 5 {
		t.Fatalf("拆分形态下 keel 库存 %d，期望 5", got)
	}
}

// —— 假适配器：版本守卫（Review Focus 2）与要接单的渠道

type fakeOrderRig struct {
	channelRig
	cs  couponShop
	ctx context.Context
	b   repository.ChannelBinding
	n   int
}

func newFakeOrderRig(t *testing.T, caps func(*channel.Caps), config map[string]any) *fakeOrderRig {
	t.Helper()
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	if caps != nil {
		caps(&rig.fake.CapsValue)
	}
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	t.Cleanup(func() { adminExec(t, `DELETE FROM channel_merchants WHERE merchant_id = $1`, cs.MerchantID) })
	cleanupChannelOrders(t, cs.MerchantID)
	if config == nil {
		config = map[string]any{}
	}
	cfg, _ := json.Marshal(config)
	b, err := rig.svc.CreateBinding(ctx, service.ChannelBindingCreate{Channel: channeltest.Kind, ExternalAccount: "shop-o",
		Name: "假渠道", Roles: channel.RoleOutlet, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := json.Marshal(channeltest.Secrets{WebhookSecret: "k"})
	if err := rig.svc.SetSecrets(ctx, b.ID, sec); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.UpsertStoreLink(ctx, repository.ChannelStoreLink{BindingID: b.ID, StoreID: cs.NorthStore, ExternalStoreID: "loc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.LinkSKU(ctx, repository.ChannelItemLink{BindingID: b.ID, KeelID: cs.DressSKU, ExternalID: "var-1"}); err != nil {
		t.Fatal(err)
	}
	on := repository.ChannelBindingActive
	if _, err := rig.svc.UpdateBinding(ctx, b.ID, service.ChannelBindingUpdate{Status: &on}); err != nil {
		t.Fatal(err)
	}
	r := &fakeOrderRig{channelRig: rig, cs: cs, ctx: ctx, b: b}
	r.drain(t)
	return r
}

func (r *fakeOrderRig) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if err := r.svc.Drain(r.ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// put 在假渠道上放一张连衣裙单（60 元 × qty）并投一条回调。
func (r *fakeOrderRig) put(t *testing.T, id string, version int64, st channel.OrderStatus, qty int32) {
	t.Helper()
	r.fake.PutOrder(channel.ChannelOrder{ExternalOrderID: id, ExternalOrderName: "F-" + id, ExternalStoreID: "loc-1",
		Status: st, Version: version, PlacedAt: time.Now(),
		Lines:   []channel.OrderLine{{ExternalLineID: "l1", ExternalSKUID: "var-1", Title: "连衣裙", Qty: qty, PriceCents: 6000}},
		Amounts: channel.OrderAmounts{GoodsCents: 6000 * int64(qty), BuyerPaidCents: 6000 * int64(qty)}})
	r.n++
	req, raw := channeltest.OrderWebhook("k", fmt.Sprintf("evt-%d-%d", r.b.ID, r.n), id)
	if _, err := r.svc.Inbound(r.ctx, r.b.ID, req, raw); err != nil {
		t.Fatal(err)
	}
	r.drain(t)
}

func (r *fakeOrderRig) channelOrderID(t *testing.T, id string) int64 {
	return adminQueryInt64(t, `SELECT id FROM channel_orders WHERE binding_id = $1 AND external_order_id = $2`, r.b.ID, id)
}

// Review Focus 2：先处理到 version 2 的「已取消」，再来 version 1 的「新单」→ 不建 keel 订单、状态不回退。
func TestChannelOrderOldVersionDoesNotOverrideNew(t *testing.T) {
	r := newFakeOrderRig(t, nil, nil)
	r.put(t, "o-1", 2, channel.OrderCancelled, 1)
	r.put(t, "o-1", 1, channel.OrderNew, 1)
	co := r.channelOrderID(t, "o-1")
	if n := keelOrdersOf(t, co); n != 0 {
		t.Fatalf("迟到的旧版本建了 %d 张 keel 订单", n)
	}
	if got := adminQueryString(t, `SELECT status || ':' || version FROM channel_orders WHERE id = $1`, co); got != "6:2" {
		t.Fatalf("渠道单 状态:版本 = %q，期望 6:2", got)
	}
}

// 要接单的渠道：没配自动接单 → 等人（不建单）；配了自动接单 → 成单入队 Act(接单)；缺货 → 关单入队 Act(拒单)。
func TestChannelOrderAcceptRequired(t *testing.T) {
	acceptRequired := func(c *channel.Caps) { c.AcceptRequired, c.AcceptTimeout = true, 5*time.Minute }

	t.Run("等人接单", func(t *testing.T) {
		r := newFakeOrderRig(t, acceptRequired, nil)
		r.put(t, "w-1", 1, channel.OrderNew, 1)
		if n := keelOrdersOf(t, r.channelOrderID(t, "w-1")); n != 0 {
			t.Fatalf("没接单就建了 %d 张 keel 订单", n)
		}
	})
	t.Run("自动接单与缺货拒单", func(t *testing.T) {
		r := newFakeOrderRig(t, acceptRequired, map[string]any{"auto_accept": true})
		r.put(t, "a-1", 1, channel.OrderNew, 1)
		co := r.channelOrderID(t, "a-1")
		if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status = 20`, co); n != 1 {
			t.Fatalf("自动接单后已支付的 keel 订单 %d 张", n)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
			fmt.Sprintf("act:%d:accept", co)); n != 1 {
			t.Fatalf("接单动作入队 %d 次，期望 1", n)
		}
		r.put(t, "a-2", 1, channel.OrderNew, 100) // 北店只有 50 件
		co2 := r.channelOrderID(t, "a-2")
		if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE channel_order_id = $1 AND status <> 90`, co2); n != 0 {
			t.Fatalf("缺货还有 %d 张没关的 keel 订单", n)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE queue = $1 AND job_key = $2`, service.QueueChannelOrderAction,
			fmt.Sprintf("act:%d:reject", co2)); n != 1 {
			t.Fatalf("缺货拒单入队 %d 次，期望 1", n)
		}
	})
}
