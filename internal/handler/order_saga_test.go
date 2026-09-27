package handler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/service"
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

// branchOf 取一个注册在协调器上的分支函数。
//
// 取的是 testOrders 那一个实例的 —— 与路由、与协调器用的是同一批函数。
// 自己 new 一个 OrderService 来测的话，测的就是一份复制品。
func branchOf(t *testing.T, name string) dtm.BranchFunc {
	t.Helper()
	fn, ok := testOrders.Branches()[name]
	if !ok {
		t.Fatalf("分支 %q 没有注册 —— 名字改了而这条测试没跟上", name)
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

// ① 租户只能从 gid 来。
//
// 硬约束一：WithSagaBranch **不解析 gid**，租户从 ctx 取，而那个 ctx 的唯一
// 产生者是 dtm.TenantContextFromGID。所以「ctx 里的租户」与「gid 里的租户」
// 一致性是按构造成立的，repository 那一层没有复核。
//
// 这条测试从外面证明那条路是锁着的：拿一个**租户段是 shop-b** 的 gid 去调
// shop-a 那笔订单的库存分支。分支会在 shop-b 的租户上下文里查 order_no，
// RLS 把它挡在视野外，于是业务根本跑不起来 —— 而 shop-a 的库存一分没动。
//
// 把 TenantContextFromGID 换成「从别处取租户」（比如一个写死的默认租户、
// 或者一个测试传进来的 ctx），这条断言当场红。
func TestBranchTakesItsTenantFromTheGIDOnly(t *testing.T) {
	skuA, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantA := seedDraftOrder(t, "shop-a", skuA, 1)
	merchantB := merchantIDOf(t, "shop-b")
	if merchantA == merchantB {
		t.Fatal("两家店解析成了同一个 id —— 这条测试没有区分力")
	}

	stock := branchOf(t, service.BranchOrderStock)

	// 拿别家的 gid：分支跑在 shop-b 的租户下，查不到这笔订单。
	foreign := gidFor(t, merchantB, orderNo)
	if got := stock(foreign, "01", "action"); got != dtm.Failure {
		t.Fatalf("用 shop-b 的 gid 调分支返回 %d，期望 Failure(%d)", got, dtm.Failure)
	}
	if after := availableOf(t, skuA); after != before {
		t.Fatalf("sku %d 的水位从 %d 变成了 %d —— 分支在别家的租户上下文里扣掉了 shop-a 的库存",
			skuA, before, after)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("留下了 %d 行流水：%+v", len(logs), logs)
	}

	// 阳性对照：换成自己的 gid，同一个分支必须真的扣成功。
	// 没有这一步，上面的「没扣」也可能只是因为这个分支根本不工作。
	own := gidFor(t, merchantA, orderNo)
	if got := stock(own, "01", "action"); got != dtm.Success {
		t.Fatalf("用 shop-a 的 gid 调分支返回 %d，期望 Success(%d)", got, dtm.Success)
	}
	if after := availableOf(t, skuA); after != before-1 {
		t.Fatalf("自己的 gid 也没扣成：水位 %d → %d", before, after)
	}
	t.Logf("shop-b 的 gid 扣不动 shop-a 的库存；shop-a 的 gid 扣掉了 1 件")
}

// ② 空回滚与重复都是正常路径，**而且悬挂保护是真的**。
//
// 架构 §5：「最易踩的坑：后两者是正常路径。返回失败会让 TC 误判并无限重试。」
//
// 这条测试一次走完三件事：
//
//	· 正向从没来过时先到的补偿 → 空回滚：返回成功，什么都不做；
//	· 随后迟到的正向 → 屏障判成重复（补偿已经把那个位置占了）：返回成功，
//	  **而且绝不能真的去扣库存** —— 那就是「悬挂」，一笔已经被回滚的事务
//	  又把货扣走了，再也没人来补；
//	· 全程水位不动、流水为空。
func TestNullCompensationAndSuspendedActionAreNormalPaths(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)

	// 补偿先到：正向从没执行过 → 空回滚。
	if got := branchOf(t, service.BranchOrderStockUndo)(gid, "01", "compensate"); got != dtm.Success {
		t.Fatalf("空回滚返回 %d，期望 Success(%d) —— 返回失败会让协调器无限重试一件"+
			"本来就不该做的事", got, dtm.Success)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("空回滚动了库存：%d → %d —— 它把货「补」了回去，而根本没扣过",
			before, after)
	}

	// 迟到的正向：屏障判成重复，什么都不做，返回成功。
	if got := branchOf(t, service.BranchOrderStock)(gid, "01", "action"); got != dtm.Success {
		t.Fatalf("悬挂的正向返回 %d，期望 Success(%d)", got, dtm.Success)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("悬挂的正向真的扣了库存：%d → %d —— 屏障的悬挂保护失效了，"+
			"这笔货再也没人补回来", before, after)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("留下了 %d 行流水：%+v —— 应该一行都没有", len(logs), logs)
	}
	t.Logf("空回滚与悬挂正向都返回成功，水位始终是 %d", before)
}

// 同一个正向分支被调两次，只扣一次。
//
// 屏障的重复判定。协调器在没收到回执时会重放分支，而重放一次就多扣一次货
// 是这条链路上最贵的一类 bug。
func TestRepeatedActionDeductsOnlyOnce(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 3)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 2)
	gid := gidFor(t, merchantID, orderNo)
	stock := branchOf(t, service.BranchOrderStock)

	for i := 0; i < 2; i++ {
		if got := stock(gid, "01", "action"); got != dtm.Success {
			t.Fatalf("第 %d 次调用返回 %d，期望 Success(%d)", i+1, got, dtm.Success)
		}
	}
	if after := availableOf(t, sku); after != before-2 {
		t.Fatalf("水位 %d → %d，期望只扣一次到 %d —— 屏障的重复判定没生效",
			before, after, before-2)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 1 {
		t.Fatalf("流水有 %d 行，期望 1 行：%+v", len(logs), logs)
	}
	t.Logf("两次调用只扣了一次：%d → %d", before, before-2)
}

// ③ 补偿真的把货放回去了，**而且和「根本没扣过」区分得开**。
//
// 这条测试提交一个真的 SAGA：第一步是真的库存分支，第二步是一个注定失败的
// 测试分支（main_test.go 里注册的 test_always_fail，那里写了它为什么存在）。
// 于是协调器会回过头来调真的 order_stock_undo。
//
// 为什么不能在正常的下单链路上测这件事：生产编排里库存是最后一步，而一个失败的
// 分支是**原子回滚**的（屏障那一行和业务写在同一个事务里），所以正常链路上
// 库存的补偿永远轮不到真的执行。对着一条不可达的分支写断言就是空转 ——
// 这个仓库前几轮反复出现的正是这种问题。
//
// 「区分得开」靠的是 inventory_logs：补偿跑完之后 available_qty 回到原值，
// 和从来没扣过一模一样。只看水位的话，把整个库存分支（正向 + 补偿）一起删掉，
// 断言照样绿。两行流水（-n 然后 +n）是唯一的区别。
func TestStockCompensationReallyPutsItBack(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 3)
	const qty = 2
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, qty)
	gid := gidFor(t, merchantID, orderNo)

	steps := fmt.Sprintf(
		`[{"action":"local://%s","compensate":"local://%s"},`+
			`{"action":"local://test_always_fail","compensate":"local://test_always_fail_undo"}]`,
		service.BranchOrderStock, service.BranchOrderStockUndo)

	if err := testTC.SubmitSaga(gid, steps); err != nil {
		t.Fatalf("提交 SAGA 失败: %v", err)
	}
	status, err := testTC.WaitFinal(gid, 15000)
	if err != nil {
		t.Fatalf("等待终态失败: %v", err)
	}
	if status == "succeed" {
		t.Fatalf("事务终态是 succeed —— 第二步那个注定失败的分支没有失败，"+
			"这条测试根本没有走到补偿。gid=%s", gid)
	}

	if after := availableOf(t, sku); after != before {
		t.Fatalf("补偿之后水位是 %d，期望回到 %d —— 库存没有被补回来", after, before)
	}

	logs := inventoryLogsOf(t, orderNo)
	if len(logs) != 2 {
		t.Fatalf("库存流水有 %d 行，期望 2 行（扣一次、补一次）：%+v\n"+
			"0 行说明正向根本没执行（那么「补回来了」是句废话）；"+
			"1 行说明补偿没跑", len(logs), logs)
	}
	deduct, restore := logs[0], logs[1]
	if deduct.BizType != 1 || deduct.ChangeQty != -qty ||
		deduct.Before != before || deduct.After != before-qty {
		t.Fatalf("第一行不是一次真扣减：%+v（期望 biz_type=1 change=-%d %d→%d）",
			deduct, qty, before, before-qty)
	}
	if restore.BizType != 2 || restore.ChangeQty != qty ||
		restore.Before != before-qty || restore.After != before {
		t.Fatalf("第二行不是一次真回补：%+v（期望 biz_type=2 change=+%d %d→%d）",
			restore, qty, before-qty, before)
	}
	t.Logf("终态 %q：水位 %d → %d → %d，流水 %+v / %+v",
		status, before, before-qty, before, deduct, restore)
}

// 分支拿到的 op 与它的角色不符时，不许把业务跑起来。
//
// 编排 JSON 里把 compensate 指向正向分支（或反过来）不会有编译错误，症状是：
// 屏障按补偿语义判定（空回滚保护），而分支体在扣库存。这里当场拦下来，
// 返回 Unknown —— 事务卡住看得见，跑在错误语义下的写入看不见。
func TestBranchRefusesAMismatchedOp(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)

	// 拿正向分支当补偿用。
	if got := branchOf(t, service.BranchOrderStock)(gid, "01", "compensate"); got != dtm.Unknown {
		t.Fatalf("正向分支收到 compensate 返回 %d，期望 Unknown(%d)", got, dtm.Unknown)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("它还是把库存扣了：%d → %d", before, after)
	}
	// 反过来：补偿分支收到 action。
	if got := branchOf(t, service.BranchOrderStockUndo)(gid, "01", "action"); got != dtm.Unknown {
		t.Fatalf("补偿分支收到 action 返回 %d，期望 Unknown(%d)", got, dtm.Unknown)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("它把库存补出来了：%d → %d", before, after)
	}
}

// 审查发现的窗口：订单已经被关掉（买家取消 / 超时关单）之后，库存分支的一次迟到重试
// 不能再扣货。扣了就是给一笔关掉的单扣库存；而关单那一侧回补过的话，就是凭空多出一份。
// 这里直接把订单推到 90 再调正向：必须是确定性失败（触发全局补偿），水位不动、一行流水都没有。
func TestStockBranchRefusesAnOrderThatWasAlreadyClosed(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 3)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 2)
	if _, err := admin(t).Exec(context.Background(),
		`UPDATE orders SET status = 90 WHERE order_no = $1`, orderNo); err != nil {
		t.Fatal(err)
	}
	gid := gidFor(t, merchantID, orderNo)
	if got := branchOf(t, service.BranchOrderStock)(gid, "01", "action"); got != dtm.Failure {
		t.Fatalf("对已关闭订单的库存正向返回 %d，期望 Failure(%d) —— 重试也改变不了，应当触发补偿",
			got, dtm.Failure)
	}
	if after := availableOf(t, sku); after != before {
		t.Fatalf("已关闭的订单被扣了库存：%d → %d", before, after)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("留下了 %d 行流水：%+v —— 应该一行都没有", len(logs), logs)
	}
}
