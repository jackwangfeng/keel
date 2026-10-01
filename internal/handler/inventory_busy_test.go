package handler_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 拆分形态下库存进程的库里那一行被锁着：库存进程等锁超过 lock_timeout（55P03），回 503 busy
// （确定没生效），core 据此回 busy，而不是以前那句「写操作可能已经生效」的 inventory-unavailable。
//
// 「确定没生效」由放锁之后的重试来证：CAS 的前提仍是 50 —— 第一次那一笔要是写进去了，
// 重试会撞前提不成立（409），而不是 200。
//
// lock_timeout 是包初始化时读的（repository.lockTimeoutSetting，默认 3s），这条测试要等满它。
func TestRemoteInventoryLockTimeoutIsBusyNotUnknown(t *testing.T) {
	cs := newBuyerShop(t)
	// 写超时要长过库存进程的 lock_timeout（3s）：否则先到的是 core 这边的超时（结果未知），
	// 验不到库存进程自己回的 busy。remoteEngine 的客户端是 2s，这里另装一套。
	c, err := rpc.NewClient(startInventoryServer(t).URL, remoteInventorySecret, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	remote := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(inventory.NewRemote(c)))

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM inventories WHERE store_id = $1 AND sku_id = $2 FOR UPDATE`,
		cs.NorthStore, cs.ShirtSKU); err != nil {
		t.Fatal(err)
	}

	cas := fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", cs.NorthStore, cs.ShirtSKU)
	body := `{"expected_available_qty":50,"available_qty":7}`
	w := serve(remote, http.MethodPut, cs.Host, cas, body, cs.Token, "")
	if got := problemType(t, w, http.StatusServiceUnavailable, "库存行被锁时的 CAS"); got != problem.TypeBusy {
		t.Fatalf("库存行被锁时 type 是 %q，期望 %q（%s）", got, problem.TypeBusy, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("busy 没带 Retry-After")
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var inv api.AdminInventory
	decodeInto(t, serve(remote, http.MethodPut, cs.Host, cas, body, cs.Token, ""),
		http.StatusOK, "放锁之后原样重试（前提仍是 50：第一次确实没写进去）", &inv)
	if inv.AvailableQty != 7 {
		t.Fatalf("重试之后是 %d，期望 7", inv.AvailableQty)
	}
}
