package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 库存服务不在、熔断器已经打开时，下单当场回 503 inventory-unavailable，确定没有下单。
//
// 2026-10 破坏性测试：同样的场景下 POST /orders 等满 15 秒（WaitFinal）回 409 处理中，
// SAGA 在后台对着一个不在的服务一遍遍重试，而那一单最后多半被补偿关掉 —— 客户端等了 15 秒，
// 换来一句「稍后重试」。熔断器开着时这件事的结局早就知道了。
//
// 「确定没有下单」：库里没有这个买家的订单，幂等键也没被占住（同一把钥匙之后能重新抢到）。
func TestOrderFailsFastWhileInventoryCircuitIsOpen(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "circuit")

	dead := startInventoryServer(t)
	dead.Close() // 地址是真的，只是没人听了
	c, err := rpc.NewClient(dead.URL, remoteInventorySecret, 2*time.Second,
		rpc.WithReadTimeout(200*time.Millisecond), rpc.WithBreaker(2, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 下单服务也要拿这个远端实现：包级的 testOrders 接的是进程内库存。协调器不接 ——
	// 熔断判在提交 SAGA 之前，走到协调器那一步本身就说明修复没生效（会以 500 红）。
	remoteInv := inventory.NewRemote(c)
	orders := service.NewOrderService(repository.New(testPool), remoteInv, nil, nil)
	engine := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner, orders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(remoteInv))

	// 连续两次读失败：熔断器打开（与线上「商品详情、购物车先后读不到库存」同一个过程）。
	for i := 0; i < 2; i++ {
		_ = c.ReadJSON(context.Background(), rpc.Prefix+"/inventory/store-stock", struct{}{}, nil)
	}
	if !c.CircuitOpen() {
		t.Fatalf("前提不成立：两次读失败之后熔断器是 %s", c.BreakerState())
	}

	key := freshIdemKey()
	body := orderBodyAt(b.Address, cs.NorthStore, [][2]int64{{cs.DressSKU, 1}}, nil)
	start := time.Now()
	w := serve(engine, http.MethodPost, cs.Host, "/api/v1/orders", body, b.Token, key)
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("熔断开着时下单等了 %s 才回 —— 应当当场失败", d)
	}
	if got := problemType(t, w, http.StatusServiceUnavailable, "熔断中的下单"); got != problem.TypeInventoryUnavailable {
		t.Fatalf("type 是 %q，期望 %q（%s）", got, problem.TypeInventoryUnavailable, w.Body.String())
	}
	var p struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if !strings.Contains(p.Detail, "没有下单") {
		t.Errorf("detail 没说「没有下单」：%q", p.Detail)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("没有 Retry-After")
	}

	if n := adminQueryInt64(t, `SELECT count(*) FROM orders WHERE user_id = $1`, b.UserID); n != 0 {
		t.Fatalf("熔断中下单回了 503，库里却有 %d 笔这个买家的订单", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM idempotency_keys WHERE merchant_id = $1 AND idem_key = $2`,
		cs.MerchantID, key); n != 0 {
		t.Fatalf("熔断中下单回了 503，那把钥匙却被占住了（%d 行）", n)
	}
}
