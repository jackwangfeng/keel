package shopify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/channel/shopify/shopifytest"
)

type cell struct {
	variant, item string
	l             channel.Listing
}

// cells 放 n 个跟踪库存的变体（一件商品），每个在 loc 上 available = start+i。
func cells(t *testing.T, r rig, loc string, n int, start int32) []cell {
	t.Helper()
	vs := make([]shopifytest.Variant, n)
	for i := range vs {
		vs[i] = shopifytest.Variant{SKU: fmt.Sprintf("S%d", i), Price: "1.00", Tracked: true, Levels: map[string]int32{loc: start + int32(i)}}
	}
	pg := r.sim.AddProduct(shopifytest.Product{Title: "p", Variants: vs})
	var out []cell
	for i, vg := range r.sim.VariantIDs(pg) {
		ig := r.sim.InventoryItem(vg)
		extra, _ := json.Marshal(map[string]any{"inventory_item_id": ig, "product_id": pg, "tracked": true})
		prev := start + int32(i)
		out = append(out, cell{variant: vg, item: ig, l: channel.Listing{StoreID: 1, SKUID: int64(i + 1), ExternalStoreID: loc,
			ExternalSKUID: vg, Extra: extra, PrevQty: &prev, Qty: prev + 10, PriceCents: 1234, IdemKey: fmt.Sprintf("7:1:%d:2", i+1)}})
	}
	return out
}

func listings(cs []cell) []channel.Listing {
	out := make([]channel.Listing, len(cs))
	for i, c := range cs {
		out[i] = c.l
	}
	return out
}

func TestPushListingsAllGood(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 3, 5)
	res, err := r.a.PushListings(context.Background(), r.b, listings(cs))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cs {
		if res[i].Err != nil {
			t.Fatalf("第 %d 条：%v", i, res[i].Err)
		}
		if q, _ := r.sim.Available(c.item, loc); q != c.l.Qty {
			t.Fatalf("第 %d 条 Shopify 上是 %d，期望 %d", i, q, c.l.Qty)
		}
	}
	if n := r.sim.Calls("SetQty"); n != 1 {
		t.Fatalf("SetQty 调了 %d 次", n)
	}
	if n := r.sim.Calls("SetPrices"); n != 0 {
		t.Fatalf("没有 PushPrice 却改了价（%d 次）", n)
	}
}

// Review Focus 3：一批里一条冲突，其余的同一次调用里重提成功。
func TestPushListingsConflictIsolated(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 3, 5)
	r.sim.SetAvailable(cs[1].item, loc, 42) // 店员在 Shopify 后台改了第 2 条
	res, err := r.a.PushListings(context.Background(), r.b, listings(cs))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 2} {
		if res[i].Err != nil {
			t.Fatalf("第 %d 条：%v", i, res[i].Err)
		}
		if q, _ := r.sim.Available(cs[i].item, loc); q != cs[i].l.Qty {
			t.Fatalf("第 %d 条没有重提：Shopify 上是 %d", i, q)
		}
	}
	if !res[1].Conflict || res[1].ObservedQty == nil || *res[1].ObservedQty != 42 || res[1].Err == nil {
		t.Fatalf("冲突那条 = %+v", res[1])
	}
	if q, _ := r.sim.Available(cs[1].item, loc); q != 42 {
		t.Fatalf("冲突那条被写了：%d", q)
	}
	keys := r.sim.IdemKeys()
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("重提应当换一个幂等键：%v", keys)
	}
}

func TestPushListingsNullPrevUntrackedAndBatches(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 600, 0)
	ls := listings(cs)
	ls[0].PrevQty = nil
	extra, _ := json.Marshal(map[string]any{"inventory_item_id": cs[1].item, "product_id": "p", "tracked": false})
	ls[1].Extra = extra
	res, err := r.a.PushListings(context.Background(), r.b, ls)
	if err != nil {
		t.Fatal(err)
	}
	for i := range res {
		if res[i].Err != nil {
			t.Fatalf("第 %d 条：%v", i, res[i].Err)
		}
	}
	if q, _ := r.sim.Available(cs[0].item, loc); q != ls[0].Qty {
		t.Fatalf("PrevQty 为 nil 的那条没生效：%d", q)
	}
	if q, _ := r.sim.Available(cs[1].item, loc); q != 1 {
		t.Fatalf("不跟踪库存的那条被写了：%d", q)
	}
	if n := r.sim.Calls("SetQty"); n != 3 {
		t.Fatalf("599 条应分 3 批，调了 %d 次", n)
	}
}

func TestPushListingsSameGroupSameKey(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 2, 5)
	ctx := context.Background()
	if _, err := r.a.PushListings(ctx, r.b, listings(cs)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.PushListings(ctx, r.b, listings(cs)); err != nil { // 重推同一次（渠道层没记下第一次的结果）
		t.Fatal(err)
	}
	keys := r.sim.IdemKeys()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("同一组重推的幂等键不同：%v", keys)
	}
}

func TestPushListingsPrices(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 3, 5)
	ls := listings(cs)
	ls[0].PushPrice, ls[1].PushPrice = true, true
	ls[1].ExternalSKUID = "gid://shopify/ProductVariant/1" // 不属于这件商品
	res, err := r.a.PushListings(context.Background(), r.b, ls)
	if err != nil {
		t.Fatal(err)
	}
	if res[1].Err == nil {
		t.Fatal("错的变体改价应当报错")
	}
	if res[0].Err == nil {
		t.Fatal("同组有一条出错时整组没生效，第一条却报了成功")
	}
	if res[2].Err != nil {
		t.Fatalf("没推价格的那条被同组改价的错连累：%v", res[2].Err)
	}
	// 模拟平台按 Shopify 的做法：有 userErrors 整组不生效
	if p := r.sim.Price(cs[2].variant); p != "1.00" {
		t.Fatalf("没 PushPrice 的那条价格被改成了 %s", p)
	}
	ls[1].ExternalSKUID = cs[1].variant
	res, err = r.a.PushListings(context.Background(), r.b, []channel.Listing{ls[0], ls[1]})
	if err != nil {
		t.Fatal(err)
	}
	if p := r.sim.Price(cs[0].variant); p != "12.34" {
		t.Fatalf("改价之后 Shopify 上是 %s，期望 12.34", p)
	}
	if n := r.sim.Calls("SetPrices"); n != 2 {
		t.Fatalf("同一件商品应一次改价，SetPrices 共调了 %d 次（期望 2 轮各 1 次）", n)
	}
}

func TestPushListingsWholeBatchErrors(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 2, 5)
	r.sim.ThrottleNext(1)
	_, err := r.a.PushListings(context.Background(), r.b, listings(cs))
	var re *channel.RetryableError
	if !errors.As(err, &re) || !re.RateLimited {
		t.Fatalf("限流应整批返回 RateLimited：%v", err)
	}
	r.sim.RotateSecret("rotated-secret-000")
	_, err = r.a.PushListings(context.Background(), r.b, listings(cs))
	if !errors.Is(err, channel.ErrCredentials) {
		t.Fatalf("凭据失效应整批返回 ErrCredentials：%v", err)
	}
	noSecrets(t, err)
}

func TestEnsureWebhooks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.sim.AddWebhook(shopifytest.WebhookSub{Topic: "PRODUCTS_UPDATE", URI: "https://old.example/x"})
	const cb = "https://demo.example/api/v1/webhooks/channels/7"
	if err := r.a.EnsureWebhooks(ctx, r.b, cb); err != nil {
		t.Fatal(err)
	}
	if err := r.a.EnsureWebhooks(ctx, r.b, cb); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range r.sim.Webhooks() {
		if w.URI == cb {
			n++
		}
	}
	if n != 11 || len(r.sim.Webhooks()) != 12 {
		t.Fatalf("订阅 = %+v，期望指向回调地址的 11 条 + 旧的 1 条", r.sim.Webhooks())
	}
}

// 审查 2：同一个 IdemKey（version 没变）带着不同的数重发，必须是不同的幂等键，新数要生效。
func TestPushListingsSameVersionDifferentQty(t *testing.T) {
	r := newRig(t)
	loc := r.sim.AddLocation("A")
	cs := cells(t, r, loc, 1, 5)
	ctx := context.Background()
	if _, err := r.a.PushListings(ctx, r.b, listings(cs)); err != nil {
		t.Fatal(err)
	}
	ls := listings(cs)
	q := ls[0].Qty
	ls[0].PrevQty, ls[0].Qty = &q, q-1 // 上次那个数已经生效、但渠道层没记下；之后又卖了一件
	res, err := r.a.PushListings(ctx, r.b, ls)
	if err != nil || res[0].Err != nil {
		t.Fatalf("%v / %v", err, res[0].Err)
	}
	if got, _ := r.sim.Available(cs[0].item, loc); got != q-1 {
		t.Fatalf("Shopify 上是 %d，期望 %d（被当成了重放）", got, q-1)
	}
}
