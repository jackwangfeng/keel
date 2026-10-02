package handler_test

// 可售数跨 0 → 有货排序标记（二阶段消息，inventory 包 stock_msg.go、service/stock_flags.go）。
//
// 单体与两库各跑一遍同一组断言：
//
//   - 不跨 0 的扣减不发消息（通知器的计数不动）；
//   - 下单把某 SKU 扣到 0 → 标记变「无货」，**不跑全量刷新**；
//   - 关单回补 → 变回「有货」；
//   - 同一条消息投递两次 → 第二次被屏障挡住，结果不变；
//   - 两条消息乱序到达 → 结果以到达那一刻的库存为准（接收方不信消息内容）；
//   - 回查：本地事务提交了的消息答「已提交」，没有屏障的 gid 答「没提交」。
//
// 单体的装配照 app.Run：一个协调器，库存分支、接收分支（local://stock_changed）、回查分支都注册在它上面，
// 所有进程内库存实现共用一个通知器。两库的装配见 newTwoDBWithStockMsg：库存进程自己一个协调器，
// 投递经 HTTP 进 core 的内网端口。两套都用自己的协调器与路由，不碰包级那一套（包级的库存实现不发通知，
// 别的测试的清理不会和异步到达的消息赛跑）。

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// stockMsgRig 是一种形态下这组测试要用的东西。
type stockMsgRig struct {
	engine   *gin.Engine
	notifier *inventory.StockNotifier
	flags    *service.StockFlagService
	// invExec / invInt 打在库存所在的那个库上（单体是测试库，两库是库存库），管理员身份。
	invExec func(t *testing.T, sql string, args ...any)
	invInt  func(t *testing.T, sql string, args ...any) int64
	// invPool 是那个库的管理员连接池（回查那一步要读一个 gid 出来）。
	invPool interface {
		QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	}
}

func newMonolithStockMsgRig(t *testing.T) stockMsgRig {
	t.Helper()
	store := repository.NewInventoryStore(testPool)
	n := inventory.NewStockNotifier(store, "local://"+inventory.BranchStockChanged, "local://"+inventory.BranchStockMsgQuery)
	local := inventory.NewLocal(store).WithStockNotifier(n)
	orders := service.NewOrderService(repository.New(testPool), local, nil, nil)
	flags := service.NewStockFlagService(repository.New(testPool), local, 0, nil)
	ex := app.InventoryBranches(local)
	for name, fn := range app.StockMsgBranches(n, flags) {
		ex[name] = fn
	}
	tc, err := dtm.StartEx("sqlite:"+filepath.Join(t.TempDir(), "dtm.db"), 0, app.Branches(orders), ex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	orders.AttachCoordinator(tc)
	n.Attach(tc)
	engine := app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		orders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(local))
	return stockMsgRig{
		engine: engine, notifier: n, flags: flags,
		invExec: func(t *testing.T, sql string, args ...any) { t.Helper(); adminExec(t, sql, args...) },
		invInt:  func(t *testing.T, sql string, args ...any) int64 { t.Helper(); return adminQueryInt64(t, sql, args...) },
		invPool: admin(t),
	}
}

func TestStockMsgMonolith(t *testing.T) {
	cs := newCouponShop(t) // 先建店：清理后进先出，协调器要先于删商品关掉
	rig := newMonolithStockMsgRig(t)
	useEngine(t, rig.engine)
	exerciseStockMsg(t, cs, rig)
}

func TestStockMsgTwoDatabases(t *testing.T) {
	cs := newCouponShop(t)
	e := newTwoDBWithStockMsg(t)
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(ctx, `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
	})
	e.moveStock(t, cs.MerchantID)
	e.use(t)
	defer coreUntouched(t, cs.MerchantID)
	exerciseStockMsg(t, cs, stockMsgRig{
		engine: e.engine, notifier: e.notifier, flags: e.flags,
		invExec: func(t *testing.T, sql string, args ...any) {
			t.Helper()
			if _, err := e.invAdmin.Exec(context.Background(), sql, args...); err != nil {
				t.Fatal(err)
			}
		},
		invInt:  e.invInt,
		invPool: e.invAdmin,
	})
}

func useEngine(t *testing.T, eng *gin.Engine) {
	old := testEngine
	testEngine = eng
	t.Cleanup(func() { testEngine = old })
}

func exerciseStockMsg(t *testing.T, cs couponShop, rig stockMsgRig) {
	ctx := context.Background()
	store, product, sku := cs.NorthStore, cs.DressProduct, cs.DressSKU
	qty := func(t *testing.T) int {
		t.Helper()
		return int(rig.invInt(t, `SELECT COALESCE(max(available_qty), 0) FROM inventories WHERE store_id = $1 AND sku_id = $2`, store, sku))
	}
	// flag：1 有货、0 无货、-1 没有这一行。
	flag := func(t *testing.T) int64 {
		t.Helper()
		return adminQueryInt64(t, `SELECT COALESCE((SELECT CASE WHEN in_stock THEN 1 ELSE 0 END
			FROM product_store_stock WHERE store_id = $1 AND product_id = $2), -1)`, store, product)
	}
	waitFlag := func(t *testing.T, want bool, what string) {
		t.Helper()
		w := int64(0)
		if want {
			w = 1
		}
		deadline := time.Now().Add(15 * time.Second)
		for flag(t) != w {
			if time.Now().After(deadline) {
				t.Fatalf("%s：等了 15 秒标记仍是 %d，期望 %d（库存 %d）", what, flag(t), w, qty(t))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	gidOf := func(tag string) string {
		return fmt.Sprintf("%s%d-%d-%s-%d", inventory.StockMsgGIDPrefix, cs.MerchantID, store, tag, sku)
	}
	deliver := func(t *testing.T, gid string) {
		t.Helper()
		// 手投用 0.12 之前的旧形状（门店与 SKU 编在 gid 里、没有载荷）：升级时还在途的消息照样能处理。
		if got := rig.flags.StockMsgBranch()(gid, "01", "action", ""); got != dtm.Success {
			t.Fatalf("接收分支对 %s 返回 %d", gid, got)
		}
	}

	b := cs.newBuyer(t, "stockmsg")
	waitFlag(t, true, "夹具（后台设了 50 件，那一刷已经写过标记）")

	t.Run("不跨0的扣减不发消息", func(t *testing.T) {
		sent := rig.notifier.Sent()
		before := qty(t)
		cs.placeOrder(t, b, store, sku, 1, nil)
		if qty(t) != before-1 {
			t.Fatalf("水位 %d，期望 %d", qty(t), before-1)
		}
		if got := rig.notifier.Sent(); got != sent {
			t.Fatalf("%d → %d 不跨 0，却登记了 %d 条消息", before, before-1, got-sent)
		}
	})

	var drained string
	t.Run("扣到0_标记变无货_不等全量刷新", func(t *testing.T) {
		sent := rig.notifier.Sent()
		o := cs.placeOrder(t, b, store, sku, qty(t), nil)
		drained = o.OrderNo
		if qty(t) != 0 {
			t.Fatalf("没扣到 0：%d", qty(t))
		}
		if got := rig.notifier.Sent(); got != sent+1 {
			t.Fatalf("扣到 0 应当登记 1 条消息，实得 %d", got-sent)
		}
		waitFlag(t, false, "扣到 0 之后")
		// 回查屏障记在库存所在的库里，与扣减同一个事务提交。
		if n := rig.invInt(t, `SELECT count(*) FROM barrier WHERE trans_type = 'msg' AND gid LIKE $1`,
			fmt.Sprintf("%s%d-%%", inventory.StockMsgGIDPrefix, cs.MerchantID)); n != 1 { // 0.12 起 gid 只有商家，门店在载荷里
			t.Fatalf("库存库里这家店的消息屏障 %d 行，期望 1", n)
		}
	})

	t.Run("关单回补_变回有货", func(t *testing.T) {
		if drained == "" {
			t.Skip("上一步没跑成")
		}
		wantStatus(t, postIdem(t, cs.Host, "/api/v1/orders/"+drained+"/cancel", "", b.Token), http.StatusOK, "取消")
		if qty(t) == 0 {
			t.Fatal("取消之后没放回")
		}
		waitFlag(t, true, "取消回补之后")
	})

	t.Run("回查_提交了答已提交_没有屏障答没提交", func(t *testing.T) {
		var gid string
		if err := rig.invPool.QueryRow(ctx, `SELECT gid FROM barrier WHERE trans_type = 'msg' AND reason = 'msg'
			AND gid LIKE $1 ORDER BY create_time LIMIT 1`,
			fmt.Sprintf("%s%d-%%", inventory.StockMsgGIDPrefix, cs.MerchantID)).Scan(&gid); err != nil {
			t.Skipf("拿不到一条真实消息的 gid：%v", err)
		}
		q := rig.notifier.QueryBranch()
		if got := q(gid, "00", "action"); got != dtm.Success {
			t.Fatalf("本地事务提交了的消息回查得到 %d，期望 Success", got)
		}
		never := gidOf("0000000000000000")
		if got := q(never, "00", "action"); got != dtm.Failure {
			t.Fatalf("从没提交过的消息回查得到 %d，期望 Failure", got)
		}
		// 回查插下了 rollback 标记，同一个 gid 的本地事务从此占不到屏障。第二次回查读不回 reason
		// （keel_app 在 barrier 上只有 INSERT，repository/msg_barrier.go 写了这个取舍），答的是「已提交」——
		// 对「去查一下」的消息，多投一次只是多重算一次。
		if n := rig.invInt(t, `SELECT count(*) FROM barrier WHERE gid = $1 AND reason = 'rollback'`, never); n != 1 {
			t.Fatalf("回查没有插下 rollback 标记（%d 行）", n)
		}
	})

	t.Run("重复投递_结果不变", func(t *testing.T) {
		gid := gidOf("00000000000000d1")
		// 先把标记弄错，证明第一次投递真的回源重算了。
		adminExec(t, `UPDATE product_store_stock SET in_stock = false WHERE store_id = $1 AND product_id = $2`, store, product)
		deliver(t, gid)
		if flag(t) != 1 {
			t.Fatalf("第一次投递之后标记 %d，期望按当前库存（%d）算成有货", flag(t), qty(t))
		}
		deliver(t, gid)
		if flag(t) != 1 {
			t.Fatalf("重复投递之后标记 %d", flag(t))
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM barrier WHERE gid = $1 AND branch_id = '01'`, gid); n != 1 {
			t.Fatalf("接收分支的屏障 %d 行，期望 1（第二次被判成重复）", n)
		}
	})

	t.Run("乱序投递_以当前库存为准", func(t *testing.T) {
		restore := qty(t)
		defer rig.invExec(t, `UPDATE inventories SET available_qty = $3 WHERE store_id = $1 AND sku_id = $2`, store, sku, restore)
		// 两次跨 0：先到 0（消息 A），再回到 5（消息 B）。水位直接改库、不经通知器 —— 消息由测试手投，好控制顺序。
		rig.invExec(t, `UPDATE inventories SET available_qty = 0 WHERE store_id = $1 AND sku_id = $2`, store, sku)
		a := gidOf("00000000000000a1")
		rig.invExec(t, `UPDATE inventories SET available_qty = 5 WHERE store_id = $1 AND sku_id = $2`, store, sku)
		bb := gidOf("00000000000000b1")
		// B 先到：按当前（5）算有货。
		deliver(t, bb)
		if flag(t) != 1 {
			t.Fatalf("B 先到之后标记 %d，期望有货", flag(t))
		}
		// A 迟到：它「说的」是到 0，但接收方不信它，按当前（5）算，仍有货。
		deliver(t, a)
		if flag(t) != 1 {
			t.Fatalf("迟到的 A 把标记改成了 %d —— 接收方信了消息内容", flag(t))
		}
		// 反过来：水位回到 0，再迟到一条「回到 5」的消息，也按当前（0）算。
		rig.invExec(t, `UPDATE inventories SET available_qty = 0 WHERE store_id = $1 AND sku_id = $2`, store, sku)
		deliver(t, gidOf("00000000000000a2"))
		deliver(t, gidOf("00000000000000b2"))
		if flag(t) != 0 {
			t.Fatalf("水位是 0，两条消息乱序到达之后标记 %d，期望无货", flag(t))
		}
	})
}
