package handler_test

// 两库形态（微服务拆分阶段 1b 的验收，docs/电商系统-微服务拆分方案.md「分阶段」第 1 行）。
//
// core 与库存各一个库：core 跑在包级的测试库上，库存跑在一个**只跑过 db/migrations-inventory**
// 的第二个库上（testdb.NewInventoryDB）。装配与 KEEL_ROLE=core 一样：公网路由、下单服务、超时补偿、
// 库存 outbox 的 worker 全部经 inventory.Remote 调一个 httptest 起的库存服务（验签 + 租户头，
// 与 KEEL_ROLE=inventory 的 internalRouter 同一个装法），SAGA 的库存分支经 dtm.BranchResolver
// 的 http:// 地址被协调器真的用 HTTP 调到（分支令牌准入）。
//
// 「core 从不碰库存表」怎么证明：这家店的库存行在测试开始时从 core 库**搬**到库存库（core 库里删掉），
// 全程结束时断言 core 库里这家店在 inventories / inventory_logs / activity_stocks 三张表里一行都没有 ——
// core 若有一处还直接读写库存表，要么读到空（下单就会缺货失败），要么写出一行（最后的断言红）。
// 源头上另有 repository 的 TestQueryFilesStayOnTheirSideOfTheSplit 扫全部查询。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/testdb"
)

// twoDB 是一套 KEEL_ROLE=core + KEEL_ROLE=inventory 的装配，库存在第二个库上。
type twoDB struct {
	invPool  *pgxpool.Pool // 库存库，应用角色（库存服务用它）
	invAdmin *pgxpool.Pool // 库存库，管理员（夹具与断言用，绕过 RLS）
	srv      *httptest.Server
	down     atomic.Bool // 为真时库存服务「不在」：连接被直接掐断
	res      dtm.BranchResolver
	remote   inventory.Service
	orders   *service.OrderService
	engine   *gin.Engine
	outbox   *service.InventoryOutboxService
	sweeper  *service.SweepService

	// 跨 0 通知（stock_msg_test.go）：只有 newTwoDBWithStockMsg 装。库存进程自己的协调器 + 通知器，
	// core 的内网端口上挂着接收分支（与 KEEL_ROLE=inventory 配 KEEL_CORE_URL、core 配 KEEL_INTERNAL_ADDR 同一个装法）。
	notifier *inventory.StockNotifier
	flags    *service.StockFlagService
}

func newTwoDB(t *testing.T) *twoDB { return newTwoDBOpts(t, false) }

// newTwoDBWithStockMsg 同 newTwoDB，另外接上跨 0 通知。
func newTwoDBWithStockMsg(t *testing.T) *twoDB { return newTwoDBOpts(t, true) }

func newTwoDBOpts(t *testing.T, stockMsg bool) *twoDB {
	t.Helper()
	ctx := context.Background()
	appDSN, adminDSN, cleanup, err := testdb.NewInventoryDB(ctx, "inv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	e := &twoDB{}
	if e.invPool, err = db.NewPoolFromDSN(ctx, appDSN); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.invPool.Close)
	if e.invAdmin, err = pgxpool.New(ctx, adminDSN); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.invAdmin.Close)

	// core 的内网端口（只在 stockMsg 时有东西挂）：库存进程的协调器把跨 0 通知投到这里。
	// 先起服务器、后填 handler —— 通知器要 core 的地址，而接收分支要 core 的库存客户端，后者又要库存进程的地址。
	var coreInternal atomic.Pointer[gin.Engine]
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if h := coreInternal.Load(); h != nil {
			h.ServeHTTP(w, req)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(coreSrv.Close)

	// 库存进程：库存接口 + 两个 SAGA 分支，背后是建在库存库上的进程内实现。
	local := inventory.NewLocal(repository.NewInventoryStore(e.invPool))
	if stockMsg {
		res, err := dtm.NewBranchResolver(coreSrv.URL, remoteInventorySecret)
		if err != nil {
			t.Fatal(err)
		}
		e.notifier = inventory.NewStockNotifier(repository.NewInventoryStore(e.invPool), res.BranchURL(inventory.BranchStockChanged))
		dir := t.TempDir()
		invTC, err := dtm.Start("sqlite:"+filepath.Join(dir, "inventory-dtm.db"), 0,
			map[string]dtm.BranchFunc{inventory.BranchStockMsgQuery: e.notifier.QueryBranch()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(invTC.Close)
		e.notifier.Attach(invTC)
		local.WithStockNotifier(e.notifier)
	}
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: remoteInventorySecret})
	inventory.Mount(routes.Tenant, local)
	inventory.MountSaga(routes.Saga, local)
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if e.down.Load() {
			// 「不在」：连接直接断掉（与进程被杀、端口没人听一样是传输层失败），不回任何状态码。
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(e.srv.Close)

	// core 进程。
	c, err := rpc.NewClient(e.srv.URL, remoteInventorySecret, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	e.remote = inventory.NewRemote(c)
	if e.res, err = dtm.NewBranchResolver(e.srv.URL, remoteInventorySecret); err != nil {
		t.Fatal(err)
	}
	e.orders = service.NewOrderService(repository.New(testPool), e.remote, nil, nil)
	e.orders.UseBranchResolver(e.res)
	dir, err := os.MkdirTemp("", "keel-twodb-dtm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	tc, err := dtm.Start("sqlite:"+filepath.Join(dir, "dtm.db"), 0, app.Branches(e.orders)) // 不注册库存分支：它们在库存进程里
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	e.orders.AttachCoordinator(tc)
	e.engine = app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}), testSigner,
		e.orders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{}, app.WithInventory(e.remote))
	if stockMsg {
		e.flags = service.NewStockFlagService(repository.New(testPool), e.remote, 0, nil)
		cr, routes := rpc.NewRouter(rpc.ServerConfig{Secret: remoteInventorySecret})
		dtm.MountBranches(routes.Saga, map[string]dtm.BranchFuncEx{inventory.BranchStockChanged: dtm.Ex(e.flags.StockMsgBranch())})
		coreInternal.Store(cr)
	}
	e.outbox = service.NewInventoryOutboxService(repository.New(testPool), e.remote, service.InventoryOutboxConfig{}, nil)
	e.sweeper = service.NewSweepService(repository.New(testPool), e.remote, service.SweepConfig{}, nil)
	return e
}

// use 让包级的请求帮手（reqAs、createOrder……都打 testEngine）在这个测试里打 core 形态的路由。
func (e *twoDB) use(t *testing.T) {
	old := testEngine
	testEngine = e.engine
	t.Cleanup(func() { testEngine = old })
}

// moveStock 把一家店的库存行从 core 库搬到库存库（core 库里删掉）。
func (e *twoDB) moveStock(t *testing.T, merchantID int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := admin(t).Query(ctx, `SELECT sku_id, store_id, available_qty, warning_qty FROM inventories WHERE merchant_id = $1`, merchantID)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		sku, store int64
		qty, warn  int32
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sku, &r.store, &r.qty, &r.warn); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	rows.Close()
	for _, r := range all {
		if _, err := e.invAdmin.Exec(ctx, `INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty, warning_qty)
			VALUES ($1, $2, $3, $4, $5)`, r.sku, r.store, merchantID, r.qty, r.warn); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`DELETE FROM inventory_logs WHERE merchant_id = $1`,
		`DELETE FROM activity_stocks WHERE merchant_id = $1`,
		`DELETE FROM inventories WHERE merchant_id = $1`,
	} {
		if _, err := admin(t).Exec(ctx, stmt, merchantID); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *twoDB) invInt(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.invAdmin.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("库存库查询失败（%s）: %v", sql, err)
	}
	return n
}

func (e *twoDB) qty(t *testing.T, storeID, skuID int64) int32 {
	t.Helper()
	return int32(e.invInt(t, `SELECT COALESCE(max(available_qty), 0) FROM inventories WHERE store_id = $1 AND sku_id = $2`, storeID, skuID))
}

// coreUntouched 断言 core 库里这家店在三张库存表里一行都没有。
func coreUntouched(t *testing.T, merchantID int64) {
	t.Helper()
	for _, tbl := range []string{"inventories", "inventory_logs", "activity_stocks"} {
		if n := adminQueryInt64(t, `SELECT count(*) FROM `+tbl+` WHERE merchant_id = $1`, merchantID); n != 0 {
			t.Errorf("core 库的 %s 里出现了这家店的 %d 行 —— core 还在直接写库存表", tbl, n)
		}
	}
}

// branchHTTP 用协调器同一种方式（dtmrs 的 HTTP 驱动：POST <地址>&gid=&branch_id=&op=，正文是载荷）
// 调库存进程的 SAGA 分支，按 dtmrs 的判读规则回 Success / Failure / Unknown。
func (e *twoDB) branchHTTP(name string) dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		u := e.res.BranchURL(name) + "&gid=" + url.QueryEscape(gid) + "&branch_id=" + branchID + "&op=" + op + "&trans_type=saga"
		resp, err := http.Post(u, "application/json", strings.NewReader(payload))
		if err != nil {
			return dtm.Unknown
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return dtm.Success
		case http.StatusConflict:
			return dtm.Failure
		default:
			return dtm.Unknown
		}
	}
}

func lastOrderOfUser(t *testing.T, userID int64) string {
	t.Helper()
	var no string
	if err := admin(t).QueryRow(context.Background(),
		`SELECT order_no FROM orders WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, userID).Scan(&no); err != nil {
		t.Fatal(err)
	}
	return no
}

func TestTwoDatabases(t *testing.T) {
	e := newTwoDB(t)
	cs := newCouponShop(t) // 单体路由建店、建商品、设库存（落在 core 库）……
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"inventory_logs", "activity_stocks", "inventories"} {
			e.invAdmin.Exec(ctx, `DELETE FROM `+tbl+` WHERE merchant_id = $1`, cs.MerchantID)
		}
	})
	e.moveStock(t, cs.MerchantID) // ……然后搬到库存库，core 库里一行不留
	e.use(t)
	defer coreUntouched(t, cs.MerchantID)
	b := cs.newBuyer(t, "twodb")
	ctx := context.Background()

	t.Run("下单扣减_SAGA库存分支走HTTP_屏障在库存库", func(t *testing.T) {
		before := e.qty(t, cs.NorthStore, cs.DressSKU)
		o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
		if got := e.qty(t, cs.NorthStore, cs.DressSKU); got != before-2 {
			t.Fatalf("库存库里水位 %d，期望 %d", got, before-2)
		}
		if n := e.invInt(t, `SELECT count(*) FROM inventory_logs WHERE biz_id = $1 AND biz_type = 1`, o.OrderNo); n != 1 {
			t.Fatalf("库存库里这一单的扣减流水 %d 行，期望 1", n)
		}
		gid := gidFor(t, cs.MerchantID, o.OrderNo)
		if n := e.invInt(t, `SELECT count(*) FROM barrier WHERE gid = $1`, gid); n == 0 {
			t.Fatal("库存分支的屏障没有记在库存库里")
		}
		// core 库里的屏障只有 core 的三个分支，库存分支的那一行不在这里。
		invBranches := map[string]bool{}
		rows, _ := e.invAdmin.Query(ctx, `SELECT DISTINCT branch_id FROM barrier WHERE gid = $1`, gid)
		for rows.Next() {
			var id string
			rows.Scan(&id)
			invBranches[id] = true
		}
		rows.Close()
		if n := adminQueryInt64(t, `SELECT count(*) FROM barrier WHERE gid = $1 AND branch_id = ANY($2)`,
			gid, branchKeys(invBranches)); n != 0 {
			t.Fatalf("库存分支的屏障行出现在了 core 库里（%d 行）", n)
		}
	})

	t.Run("秒杀配额_整组设在库存库_下单扣配额", func(t *testing.T) {
		p := cs.livePromotion(t, "两库秒杀", fmt.Sprintf(`"promotion_type":4,`+
			`"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]`, cs.ShirtSKU))
		if q := e.invInt(t, `SELECT quota FROM activity_stocks WHERE promotion_id = $1`, p.Id); q != 3 {
			t.Fatalf("库存库里的配额是 %d，期望 3", q)
		}
		o := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 1, nil)
		if o.PayableCents != 990 {
			t.Fatalf("应当按秒杀价 990 成交，实得 %d", o.PayableCents)
		}
		if s := e.invInt(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, p.Id); s != 1 {
			t.Fatalf("库存库里的已售是 %d，期望 1", s)
		}
		// 后台看到的配额与已售是经库存服务取回来的。
		var got api.AdminPromotion
		decodeInto(t, getAs(t, cs.Host, fmt.Sprintf("/api/v1/admin/promotions/%d", p.Id), cs.Token), http.StatusOK, "活动详情", &got)
		if len(got.Skus) != 1 || got.Skus[0].StockQty != 3 || got.Skus[0].SoldQty != 1 {
			t.Fatalf("后台活动详情里的配额 / 已售：%+v", got.Skus)
		}
		// 卖出过的 SKU 不能移出活动 —— 由库存服务判，422。
		wantStatus(t, cs.patchPromotion(t, p.Id, `{"status":0}`), http.StatusOK, "下线")
		w := cs.patchPromotion(t, p.Id, fmt.Sprintf(`{"skus":[{"sku_id":%d,"promo_price_cents":990,"stock_qty":3}]}`, cs.DressSKU))
		wantStatus(t, w, http.StatusUnprocessableEntity, "移除卖出过的 SKU")
		// 取消：配额一起放回。
		wantStatus(t, postIdem(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/cancel", "", b.Token), http.StatusOK, "取消秒杀单")
		if s := e.invInt(t, `SELECT sold FROM activity_stocks WHERE promotion_id = $1`, p.Id); s != 0 {
			t.Fatalf("取消之后已售是 %d，期望 0", s)
		}
	})

	t.Run("支付与取消_取消经outbox放回", func(t *testing.T) {
		paid := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		if st := orderStatusOf(t, paid.OrderNo); st != 20 {
			t.Fatalf("支付之后订单是 %d，期望 20", st)
		}
		before := e.qty(t, cs.NorthStore, cs.DressSKU)
		o := cs.placeOrder(t, b, cs.NorthStore, cs.DressSKU, 3, nil)
		wantStatus(t, postIdem(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/cancel", "", b.Token), http.StatusOK, "取消")
		if got := e.qty(t, cs.NorthStore, cs.DressSKU); got != before {
			t.Fatalf("取消之后水位 %d，期望放回到 %d", got, before)
		}
		if n := e.invInt(t, `SELECT count(*) FROM inventory_logs WHERE biz_id = $1 AND biz_type = 6`, o.OrderNo); n != 1 {
			t.Fatalf("买家取消释放的流水 %d 行，期望 1", n)
		}
	})

	t.Run("超时关单", func(t *testing.T) {
		before := e.qty(t, cs.NorthStore, cs.ShirtSKU)
		o := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 2, nil)
		adminExec(t, `UPDATE orders SET expire_at = now() - interval '1 minute' WHERE order_no = $1`, o.OrderNo)
		if _, err := e.sweeper.SweepOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if st := orderStatusOf(t, o.OrderNo); st != 90 {
			t.Fatalf("超时之后订单是 %d，期望 90", st)
		}
		if got := e.qty(t, cs.NorthStore, cs.ShirtSKU); got != before {
			t.Fatalf("超时关单之后水位 %d，期望 %d", got, before)
		}
	})

	t.Run("退款回补_按退款单号幂等", func(t *testing.T) {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 2, nil)
		after := e.qty(t, cs.NorthStore, cs.DressSKU)
		_, lines := cs.lines(t, b, o.OrderNo)
		r := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 2}))
		cs.mustApprove(t, r.RefundNo)
		if got := e.qty(t, cs.NorthStore, cs.DressSKU); got != after+2 {
			t.Fatalf("未发货退款到账之后水位 %d，期望 %d", got, after+2)
		}
		// 同一张退款单再回补一次（outbox 重试 / 回包丢失）：什么都不改。
		res, err := e.remote.RestockForRefund(tenant.NewContext(ctx, cs.MerchantID), inventory.RestockRequest{
			RefundNo: r.RefundNo, StoreID: cs.NorthStore, Lines: []inventory.OrderLine{{SKUID: cs.DressSKU, Qty: 2}}})
		if err != nil || !res.Replayed || e.qty(t, cs.NorthStore, cs.DressSKU) != after+2 {
			t.Fatalf("重复回补：%+v %v，水位 %d", res, err, e.qty(t, cs.NorthStore, cs.DressSKU))
		}
	})

	t.Run("后台CAS与相对调整", func(t *testing.T) {
		cur := e.qty(t, cs.SouthStore, cs.ShirtSKU)
		cas := fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", cs.SouthStore, cs.ShirtSKU)
		wantStatus(t, putAs(t, cs.Host, cas, fmt.Sprintf(`{"expected_available_qty":%d,"available_qty":7}`, cur), cs.Token),
			http.StatusOK, "CAS")
		wantStatus(t, postIdem(t, cs.Host, adjustPath(cs.SouthStore, cs.ShirtSKU), `{"delta":5,"reason":"进货"}`, cs.Token),
			http.StatusOK, "相对调整")
		if got := e.qty(t, cs.SouthStore, cs.ShirtSKU); got != 12 {
			t.Fatalf("CAS 到 7 再加 5 之后库存库里是 %d，期望 12", got)
		}
	})

	t.Run("库存服务不在时下单_SAGA重试到它回来", func(t *testing.T) {
		before := e.qty(t, cs.NorthStore, cs.DressSKU)
		e.down.Store(true)
		done := make(chan int, 1)
		go func() {
			w := createOrder(t, cs.Host, cs.orderJSON(b, cs.NorthStore, cs.DressSKU, 1, nil), b.Token, "down-"+uniqueKey())
			done <- w.Code
		}()
		time.Sleep(3 * time.Second)
		if got := e.qty(t, cs.NorthStore, cs.DressSKU); got != before {
			t.Fatalf("库存服务不在时水位变了：%d → %d", before, got)
		}
		e.down.Store(false)
		code := <-done
		orderNo := lastOrderOfUser(t, b.UserID)
		// WaitFinal 等 15 秒；协调器的重试若更晚，下单回 409 处理中，SAGA 仍在后台推进。
		if code != http.StatusCreated && code != http.StatusConflict {
			t.Fatalf("下单回 %d，期望 201 或 409 处理中", code)
		}
		deadline := time.Now().Add(90 * time.Second)
		for orderStatusOf(t, orderNo) != 10 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
		}
		if st := orderStatusOf(t, orderNo); st != 10 {
			t.Fatalf("库存服务回来之后订单是 %d，期望 SAGA 推完到 10", st)
		}
		if got := e.qty(t, cs.NorthStore, cs.DressSKU); got != before-1 {
			t.Fatalf("SAGA 推完之后水位 %d，期望 %d（只扣一次）", got, before-1)
		}
		t.Logf("库存服务断开 3 秒，下单回 %d，SAGA 在它回来之后推完", code)
	})

	t.Run("库存服务不在时取消_放回任务重试到它回来", func(t *testing.T) {
		before := e.qty(t, cs.NorthStore, cs.ShirtSKU)
		o := cs.placeOrder(t, b, cs.NorthStore, cs.ShirtSKU, 2, nil)
		e.down.Store(true)
		wantStatus(t, postIdem(t, cs.Host, "/api/v1/orders/"+o.OrderNo+"/cancel", "", b.Token), http.StatusOK, "库存服务不在时取消")
		if st := orderStatusOf(t, o.OrderNo); st != 90 {
			t.Fatalf("取消没有生效：订单 %d", st)
		}
		key := "release:" + o.OrderNo
		adminExec(t, `UPDATE jobs SET run_after = now() WHERE queue = $1 AND job_key = $2`, service.QueueInventoryRelease, key)
		rep, err := e.outbox.WorkOnce(ctx)
		if err != nil || rep.Retried == 0 {
			t.Fatalf("库存服务不在时 worker 应当退避重试：%+v %v", rep, err)
		}
		e.down.Store(false)
		if got := e.qty(t, cs.NorthStore, cs.ShirtSKU); got != before-2 {
			t.Fatalf("放回还没发生，水位应当仍是 %d，实得 %d", before-2, got)
		}
		adminExec(t, `UPDATE jobs SET run_after = now() WHERE queue = $1 AND job_key = $2`, service.QueueInventoryRelease, key)
		if _, err := e.outbox.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if got := e.qty(t, cs.NorthStore, cs.ShirtSKU); got != before {
			t.Fatalf("库存服务回来之后水位 %d，期望放回到 %d", got, before)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE queue = $1 AND job_key = $2 AND status = 2`,
			service.QueueInventoryRelease, key); n != 1 {
			t.Fatalf("放回任务没有标成完成（%d）", n)
		}
	})

	t.Run("F2_取消与迟到的库存分支赛跑", func(t *testing.T) {
		runF2(t, f2Env{
			orders:  e.orders,
			deduct:  e.branchHTTP(inventory.BranchDeduct),
			restore: e.branchHTTP(inventory.BranchRestore),
			stock:   func(t *testing.T, sku, store int64) int32 { return e.qty(t, store, sku) },
			setup: func(t *testing.T, merchantID, sku, store int64, qty int32) {
				if _, err := e.invAdmin.Exec(context.Background(), `INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
					VALUES ($1, $2, $3, $4) ON CONFLICT (sku_id, store_id) DO UPDATE SET available_qty = EXCLUDED.available_qty`,
					sku, store, merchantID, qty); err != nil {
					t.Fatal(err)
				}
			},
		})
	})
}

func branchKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
