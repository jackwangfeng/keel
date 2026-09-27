package handler_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// SAGA 分支本身的测试。
//
// 上面那一组走 HTTP，验的是链路；这一组直接调**注册在协调器上的那几个分支函数**
// （testOrders.Branches() 返回的就是它们，与 app.Run 里注册进去的是同一批），
// 验的是三条只有在分支这一层才看得见的事：
//
//	① 租户只能从 gid 来 —— 拿别家的 gid 调分支，业务必须跑不起来；
//	② 空回滚与重复是**正常路径** —— 返回失败会让协调器误判并无限重试；
//	③ 补偿真的把货放回去了，而且和「根本没扣过」区分得开。
//
// ③ 单独用一条真 SAGA 跑（见文件末尾），因为它要的是协调器真的回过头来调补偿。

// seedDraftOrder 用管理员连接造一笔 status = 0 创建中的订单和它的一行订单项。
//
// 走管理员连接（绕过 RLS）而不是打 HTTP：这一组测试的被测对象是分支，
// 订单只是它的输入。用 HTTP 造的话，一笔订单会连着把 SAGA 也跑完，
// 剩不下任何一个还没被处理过的 status = 0 订单给分支用。
func seedDraftOrder(t *testing.T, merchantCode string, skuID int64, qty int32) (string, int64) {
	t.Helper()
	ctx := context.Background()
	conn := admin(t)

	merchantID := merchantIDOf(t, merchantCode)
	var userID int64
	if err := conn.QueryRow(ctx,
		`SELECT id FROM users WHERE merchant_id = $1 AND phone = $2 AND deleted_at IS NULL`,
		merchantID, seedPhone).Scan(&userID); err != nil {
		t.Fatalf("取 %s 的种子买家失败: %v", merchantCode, err)
	}

	orderNo := fmt.Sprintf("saga%d", time.Now().UnixNano())
	var orderID int64
	// store_id / region_id / store_snapshot 三列都是 NOT NULL（00020）。
	// 它们不是补给约束看的摆设：分支拿到的只有三个字符串（gid / branchID /
	// op），扣减扣的是**订单行上那家店**，这笔夹具单挂错店，
	// 下面「扣的是不是这个 SKU 这家店」的断言就落在另一行库存上。
	// 门店与大区从 stores 现取（种子里那家默认店），快照按 00020 回填段
	// 同一个形状拼 —— 展示字段，不放 id。
	if err := conn.QueryRow(ctx, `
		INSERT INTO orders (merchant_id, order_no, user_id, status,
		                    goods_amount_cents, payable_cents, receiver_snapshot, expire_at,
		                    store_id, region_id, store_snapshot)
		SELECT $1, $2, $3, 0, s.price_cents * $5, s.price_cents * $5,
		       '{"receiver_name":"夹具"}'::jsonb, now() + interval '30 minutes',
		       st.id, st.region_id,
		       jsonb_build_object('store_name', st.name, 'region_name', r.name,
		                          'address', st.address, 'phone', st.phone)
		  FROM skus s
		  JOIN stores  st ON st.merchant_id = $1 AND st.is_default AND st.deleted_at IS NULL
		  JOIN regions r  ON r.id = st.region_id
		 WHERE s.id = $4
		RETURNING id`, merchantID, orderNo, userID, skuID, qty).Scan(&orderID); err != nil {
		t.Fatalf("造订单夹具失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO order_items (merchant_id, order_id, sku_id, product_id, title_snapshot,
		                         spec_snapshot, price_cents, list_price_cents, quantity, amount_cents)
		SELECT $1, $2, s.id, s.product_id, '夹具商品', '{}'::jsonb,
		       s.price_cents, s.price_cents, $4::int, s.price_cents * $4::int
		  FROM skus s WHERE s.id = $3`, merchantID, orderID, skuID, qty); err != nil {
		t.Fatalf("造订单项夹具失败: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM inventory_logs WHERE biz_id = $1`,
			`DELETE FROM order_items WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
			`DELETE FROM orders WHERE order_no = $1`,
		} {
			if _, err := conn.Exec(c, stmt, orderNo); err != nil {
				t.Errorf("清理夹具失败 (%s): %v", stmt, err)
			}
		}
	})
	return orderNo, merchantID
}

// branchOf 取一个注册在协调器上的 core 分支函数。
//
// 取的是 testOrders 那一个实例的 —— 与路由、与协调器用的是同一批函数。
func branchOf(t *testing.T, name string) dtm.BranchFunc {
	t.Helper()
	fn, ok := testOrders.Branches()[name]
	if !ok {
		t.Fatalf("分支 %q 没有注册 —— 名字改了而这条测试没跟上", name)
	}
	return fn
}

// invBranchOf 取库存服务的 SAGA 分支（单体形态下注册在 testTC 上的就是它们）。
func invBranchOf(t *testing.T, name string) dtm.BranchFuncEx {
	t.Helper()
	fn, ok := app.InventoryBranches(testInvLocal)[name]
	if !ok {
		t.Fatalf("库存分支 %q 没有注册", name)
	}
	return fn
}

// gidFor 拼一个下单 gid。
func gidFor(t *testing.T, merchantID int64, orderNo string) string {
	t.Helper()
	gid, err := dtm.OrderGID(merchantID, orderNo)
	if err != nil {
		t.Fatal(err)
	}
	return gid
}

// orderStoreOf 读订单挂的门店。
func orderStoreOf(t *testing.T, orderNo string) int64 {
	t.Helper()
	var id int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT store_id FROM orders WHERE order_no = $1`, orderNo).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// deductPayload 拼库存分支的载荷（与 service.OrderService 下单时拼的是同一个形状）。
func deductPayload(t *testing.T, orderNo string, storeID, skuID int64, qty int32) string {
	t.Helper()
	p, err := inventory.EncodeDeductPayload(inventory.DeductPayload{
		OrderNo: orderNo, StoreID: storeID, Lines: []inventory.OrderLine{{SKUID: skuID, Qty: qty}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// logsOfType 数一笔订单某一种流水有几行。
func logsOfType(logs []invLog, bizType int16) int {
	n := 0
	for _, l := range logs {
		if l.BizType == bizType {
			n++
		}
	}
	return n
}

// ① 租户只能从 gid 来。
//
// 库存分支的租户只经 dtm.TenantContextFromGID 从 gid 解出来，载荷里没有租户。拿一个
// 租户段是 shop-b 的 gid、载荷却是 shop-a 的 SKU 去调它：分支跑在 shop-b 的租户下，
// RLS 把 shop-a 的库存行挡在视野外（≡ 缺行），于是只会提交一行 shop-b 名下的拒绝 ——
// shop-a 的库存一件不动、shop-a 名下一行流水都没有。
func TestBranchTakesItsTenantFromTheGIDOnly(t *testing.T) {
	skuA, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantA := seedDraftOrder(t, "shop-a", skuA, 1)
	storeA := orderStoreOf(t, orderNo)
	merchantB := merchantIDOf(t, "shop-b")
	if merchantA == merchantB {
		t.Fatal("两家店解析成了同一个 id —— 这条测试没有区分力")
	}
	deduct := invBranchOf(t, inventory.BranchDeduct)
	payload := deductPayload(t, orderNo, storeA, skuA, 1)

	foreign := gidFor(t, merchantB, orderNo)
	if got := deduct(foreign, "03", "action", payload); got != dtm.Success {
		t.Fatalf("用 shop-b 的 gid 调库存分支返回 %d，期望 Success（一行拒绝）", got)
	}
	if after := availableOf(t, skuA); after != before {
		t.Fatalf("sku %d 的水位从 %d 变成了 %d —— 分支在别家的租户上下文里扣掉了 shop-a 的库存", skuA, before, after)
	}
	for _, l := range inventoryLogsOf(t, orderNo) {
		if l.BizType == inventory.BizOrderDeduct {
			t.Fatalf("留下了扣减流水：%+v", l)
		}
	}

	// 阳性对照：换成自己的 gid，同一个分支必须真的扣成功。
	own := gidFor(t, merchantA, orderNo)
	if got := deduct(own, "03", "action", payload); got != dtm.Success {
		t.Fatalf("用 shop-a 的 gid 调分支返回 %d，期望 Success", got)
	}
	if after := availableOf(t, skuA); after != before-1 {
		t.Fatalf("自己的 gid 也没扣成：水位 %d → %d", before, after)
	}
}

// 载荷里的订单号与 gid 对不上：编排拼错了载荷，确定性失败，一件都不扣。
func TestInventoryBranchRefusesAPayloadForAnotherOrder(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	payload := deductPayload(t, orderNo+"x", orderStoreOf(t, orderNo), sku, 1)
	if got := invBranchOf(t, inventory.BranchDeduct)(gidFor(t, merchantID, orderNo), "03", "action", payload); got != dtm.Failure {
		t.Fatalf("载荷订单号对不上返回 %d，期望 Failure", got)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("水位 %d → %d", before, after)
	}
}

// ② 空回滚与悬挂都是正常路径（屏障在库存库，单体下就是这个库）。
func TestNullCompensationAndSuspendedActionAreNormalPaths(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)
	payload := deductPayload(t, orderNo, orderStoreOf(t, orderNo), sku, 1)

	if got := invBranchOf(t, inventory.BranchRestore)(gid, "03", "compensate", payload); got != dtm.Success {
		t.Fatalf("空回滚返回 %d，期望 Success", got)
	}
	if got := invBranchOf(t, inventory.BranchDeduct)(gid, "03", "action", payload); got != dtm.Success {
		t.Fatalf("悬挂的正向返回 %d，期望 Success", got)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("悬挂的正向真的扣了库存：%d → %d —— 屏障的悬挂保护失效了", before, after)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("留下了 %d 行流水：%+v —— 应该一行都没有", len(logs), logs)
	}
}

// 同一个正向分支被调两次，只扣一次。
func TestRepeatedActionDeductsOnlyOnce(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 3)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 2)
	gid := gidFor(t, merchantID, orderNo)
	payload := deductPayload(t, orderNo, orderStoreOf(t, orderNo), sku, 2)
	for i := 0; i < 2; i++ {
		if got := invBranchOf(t, inventory.BranchDeduct)(gid, "03", "action", payload); got != dtm.Success {
			t.Fatalf("第 %d 次调用返回 %d", i+1, got)
		}
	}
	if after := availableOf(t, sku); after != before-2 {
		t.Fatalf("水位 %d → %d，期望只扣一次到 %d", before, after, before-2)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 1 {
		t.Fatalf("流水有 %d 行，期望 1 行：%+v", len(logs), logs)
	}
}

// ③ 补偿真的把货放回去了，**而且和「根本没扣过」区分得开**（两行流水）。
// 真 SAGA：第一步是真的库存分支（带载荷），第二步注定失败，协调器回过头来调真的补偿。
func TestStockCompensationReallyPutsItBack(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 3)
	const qty = 2
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, qty)
	gid := gidFor(t, merchantID, orderNo)
	steps, err := dtm.StepsJSON(
		dtm.Step{Action: "local://" + inventory.BranchDeduct, Compensate: "local://" + inventory.BranchRestore,
			Payload: deductPayload(t, orderNo, orderStoreOf(t, orderNo), sku, qty)},
		dtm.Step{Action: "local://test_always_fail", Compensate: "local://test_always_fail_undo"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := testTC.SubmitSaga(gid, steps); err != nil {
		t.Fatalf("提交 SAGA 失败: %v", err)
	}
	status, err := testTC.WaitFinal(gid, 15000)
	if err != nil {
		t.Fatalf("等待终态失败: %v", err)
	}
	if status == "succeed" {
		t.Fatalf("事务终态是 succeed —— 第二步没有失败，这条测试根本没有走到补偿")
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("补偿之后水位是 %d，期望回到 %d", after, before)
	}
	logs := inventoryLogsOf(t, orderNo)
	if len(logs) != 2 {
		t.Fatalf("库存流水有 %d 行，期望 2 行（扣一次、补一次）：%+v", len(logs), logs)
	}
	deduct, restore := logs[0], logs[1]
	if deduct.BizType != 1 || deduct.ChangeQty != -qty || deduct.Before != before || deduct.After != before-qty {
		t.Fatalf("第一行不是一次真扣减：%+v", deduct)
	}
	if restore.BizType != 2 || restore.ChangeQty != qty || restore.Before != before-qty || restore.After != before {
		t.Fatalf("第二行不是一次真回补：%+v", restore)
	}
}

// 分支拿到的 op 与它的角色不符时，不许把业务跑起来（Unknown：卡住看得见）。
func TestBranchRefusesAMismatchedOp(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)
	payload := deductPayload(t, orderNo, orderStoreOf(t, orderNo), sku, 1)
	if got := invBranchOf(t, inventory.BranchDeduct)(gid, "03", "compensate", payload); got != dtm.Unknown {
		t.Fatalf("正向分支收到 compensate 返回 %d，期望 Unknown", got)
	}
	if got := invBranchOf(t, inventory.BranchRestore)(gid, "03", "action", payload); got != dtm.Unknown {
		t.Fatalf("补偿分支收到 action 返回 %d，期望 Unknown", got)
	}
	if got := branchOf(t, service.BranchOrderFinish)(gid, "04", "compensate"); got != dtm.Unknown {
		t.Fatalf("收尾分支收到 compensate 返回 %d，期望 Unknown", got)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("它还是动了库存：%d → %d", before, after)
	}
}

// 收尾分支：扣减被拒的原因回到 core（库存服务提交一行拒绝、返回成功；收尾分支读到拒绝码 → Failure）。
func TestFinishBranchTurnsARejectionIntoFailure(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 1)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)
	payload := deductPayload(t, orderNo, orderStoreOf(t, orderNo), sku, before+1)
	if got := invBranchOf(t, inventory.BranchDeduct)(gid, "03", "action", payload); got != dtm.Success {
		t.Fatalf("缺货的库存分支返回 %d，期望 Success（拒绝是提交的结论）", got)
	}
	if got := branchOf(t, service.BranchOrderFinish)(gid, "04", "action"); got != dtm.Failure {
		t.Fatalf("收尾分支读到拒绝返回 %d，期望 Failure", got)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("水位 %d → %d", before, after)
	}
}

// ---------------------------------------------------------------------------
// 审查发现的窗口（F2）：取消与一次迟到重试的库存分支赛跑
// ---------------------------------------------------------------------------

// f2Env 是赛跑测试要的四样东西：单体与两库各给一份（两库那份的库存分支走 HTTP）。
type f2Env struct {
	orders  *service.OrderService
	deduct  dtm.BranchFuncEx
	restore dtm.BranchFuncEx
	stock   func(t *testing.T, skuID, storeID int64) int32
	// setup 在库存那一侧给这个 SKU 放 qty 件（两库形态下库存在另一个库里）；单体为 nil（种子里有）。
	setup func(t *testing.T, merchantID, skuID, storeID int64, qty int32)
}

// cancelAs 以订单的买家身份取消（与 POST /orders/{no}/cancel 同一个服务方法）。
func cancelAs(t *testing.T, env f2Env, merchantID int64, orderNo string) {
	t.Helper()
	var userID int64
	if err := admin(t).QueryRow(context.Background(), `SELECT user_id FROM orders WHERE order_no = $1`,
		orderNo).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	ctx := auth.NewContext(tenant.NewContext(context.Background(), merchantID), auth.Identity{UserID: userID})
	if _, _, err := env.orders.Cancel(ctx, orderNo, freshIdemKey()); err != nil {
		t.Fatalf("取消 %s 失败: %v", orderNo, err)
	}
}

// runF2 跑三种排列，每一种结束时水位都必须回到原值（「不多不少，只放一次」）：
//
//	A 取消先、迟到的扣减后   库存服务看到释放流水，拒绝扣减（released）；收尾分支 Failure，补偿什么都不放
//	B 扣减先、取消后         取消按流水放回扣掉的那些；收尾分支看到 90 → Failure，补偿按流水什么都不放
//	C 两者并发，多轮         无论谁先，水位回到原值
func runF2(t *testing.T, env f2Env) {
	const qty = 2
	prepare := func(t *testing.T) (orderNo string, merchantID, sku, store int64, before int32, gid, payload string) {
		sku, _ = anySKUWithStock(t, "shop-a", qty)
		orderNo, merchantID = seedDraftOrder(t, "shop-a", sku, qty)
		store = orderStoreOf(t, orderNo)
		if env.setup != nil {
			env.setup(t, merchantID, sku, store, 50)
		}
		// 建单分支已经把它推到 10（这一段窗口正是审查说的那一段）。
		if _, err := admin(t).Exec(context.Background(), `UPDATE orders SET status = 10 WHERE order_no = $1`, orderNo); err != nil {
			t.Fatal(err)
		}
		before = env.stock(t, sku, store)
		gid = gidFor(t, merchantID, orderNo)
		payload = deductPayload(t, orderNo, store, sku, qty)
		return
	}
	finish := func(gid string) int {
		return env.orders.Branches()[service.BranchOrderFinish](gid, "04", "action")
	}

	t.Run("A_取消先_迟到的扣减后", func(t *testing.T) {
		orderNo, merchantID, sku, store, before, gid, payload := prepare(t)
		cancelAs(t, env, merchantID, orderNo)
		if got := env.deduct(gid, "03", "action", payload); got != dtm.Success {
			t.Fatalf("迟到的扣减返回 %d", got)
		}
		if got := env.stock(t, sku, store); got != before {
			t.Fatalf("已取消的订单被扣了库存：%d → %d", before, got)
		}
		if got := finish(gid); got != dtm.Failure {
			t.Fatalf("收尾分支返回 %d，期望 Failure", got)
		}
		if got := env.restore(gid, "03", "compensate", payload); got != dtm.Success {
			t.Fatalf("补偿返回 %d", got)
		}
		if got := env.stock(t, sku, store); got != before {
			t.Fatalf("补偿之后水位 %d，期望 %d —— 凭空多出了库存", got, before)
		}
	})

	t.Run("B_扣减先_取消后", func(t *testing.T) {
		orderNo, merchantID, sku, store, before, gid, payload := prepare(t)
		if got := env.deduct(gid, "03", "action", payload); got != dtm.Success {
			t.Fatalf("扣减返回 %d", got)
		}
		if got := env.stock(t, sku, store); got != before-qty {
			t.Fatalf("扣减之后水位 %d，期望 %d", got, before-qty)
		}
		cancelAs(t, env, merchantID, orderNo)
		if got := env.stock(t, sku, store); got != before {
			t.Fatalf("取消之后水位 %d，期望放回到 %d", got, before)
		}
		if got := finish(gid); got != dtm.Failure {
			t.Fatalf("收尾分支对已取消的订单返回 %d，期望 Failure", got)
		}
		if got := env.restore(gid, "03", "compensate", payload); got != dtm.Success {
			t.Fatalf("补偿返回 %d", got)
		}
		if got := env.stock(t, sku, store); got != before {
			t.Fatalf("取消与补偿都放了一次：水位 %d，期望 %d", got, before)
		}
	})

	t.Run("C_并发", func(t *testing.T) {
		for i := 0; i < 8; i++ {
			orderNo, merchantID, sku, store, before, gid, payload := prepare(t)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); env.deduct(gid, "03", "action", payload) }()
			go func() { defer wg.Done(); cancelAs(t, env, merchantID, orderNo) }()
			wg.Wait()
			if got := finish(gid); got != dtm.Failure {
				t.Fatalf("第 %d 轮：收尾分支返回 %d，期望 Failure", i, got)
			}
			env.restore(gid, "03", "compensate", payload)
			if got := env.stock(t, sku, store); got != before {
				t.Fatalf("第 %d 轮：水位 %d，期望 %d —— 取消与迟到的扣减赛跑之后多了或少了库存", i, got, before)
			}
		}
	})
}

// 单体形态的 F2。两库形态的同一组在 two_db_test.go。
func TestCancelRacingARetriedInventoryBranch(t *testing.T) {
	runF2(t, f2Env{
		orders:  testOrders,
		deduct:  invBranchOf(t, inventory.BranchDeduct),
		restore: invBranchOf(t, inventory.BranchRestore),
		stock:   func(t *testing.T, sku, _ int64) int32 { return availableOf(t, sku) },
	})
}
