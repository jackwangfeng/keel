package handler_test

import (
	"context"
	"testing"
)

// 拆分部署（C 档）下的补货计算：restock_plan 的水位与断货天数都要跨 HTTP 问库存服务
// （inventory.Remote.StoreStock / StockoutDays）。北京门店连衣裙下 2 件 → 已售 2、可售 48。
func TestTwoDatabasesRestockPlan(t *testing.T) {
	e := newTwoDB(t)
	cs := newCouponShop(t)
	t.Cleanup(func() {
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(context.Background(), `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
	})
	e.moveStock(t, cs.MerchantID)
	e.use(t)
	b := cs.newBuyer(t, "twodb-restock")
	o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
	cs.pay(t, o.OrderNo, o.PayableCents)

	a := createAgent(t, cs.adminShop, `{"name":"AI","role":2}`)
	sess := mcpConnect(t, cs.Host, issueAgentKey(t, cs.adminShop, a.Id, `{"name":"t"}`).Secret)
	res, out := mcpCall(t, sess, "restock_plan", map[string]any{"store_id": cs.NorthStore, "all": true})
	if res.IsError {
		t.Fatalf("拆分部署下 restock_plan 出错：%s", mcpText(res))
	}
	for _, l := range out["lines"].([]any) {
		m := l.(map[string]any)
		if int64(m["sku_id"].(float64)) == cs.DressSKU {
			if m["sold"].(float64) != 2 || m["available"].(float64) != 48 || m["stockout_days"].(float64) != 0 {
				t.Fatalf("连衣裙：%v", m)
			}
			return
		}
	}
	t.Fatalf("restock_plan 里没有北京门店的连衣裙：%v", out)
}

