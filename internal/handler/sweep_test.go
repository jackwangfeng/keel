package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/service"
)

// 超时补偿定时任务（Task 6）的行为测试。
//
// 这一组和别的几组有一个结构上的差别，值得先说清楚：**它没有 HTTP**。
// 定时任务跑在任何请求之外，所以这里直接调 SweepOnce，租户由它自己从
// merchants 里枚举出来 —— 那正是被测的东西之一（硬约束三）。
//
// 核对事实一律走管理员连接绕过 RLS（admin(t)），理由与别处一样：
// 「跑完之后再 GET 一次订单，看到状态是 90」只证明同一段代码前后自洽。
//
// 每条断言都要能区分「我守的这件事坏了」与「附近有别的东西坏了」。
// 这一组里最要紧的一条是：**光看 available_qty 是不够的**。扣减与回补跑完之后
// 水位回到原值，和「从来没扣过」一模一样；上一轮已经踩过这个坑（把扣减和补偿
// 一起变成空操作，只看水位的断言恰好假绿）。所以凡是涉及库存的断言，
// 一律同时钉住 inventory_logs 的行数、biz_type 与前后水位。

// expire 把一笔订单的 expire_at 推到过去，让它对扫描可见。
//
// 不用 sleep 也不用把 orderExpireIn 调短：前者要等 30 分钟，
// 后者会让被测的常量不是生产里那一个。
func expire(t *testing.T, orderNo string) {
	t.Helper()
	ct, err := admin(t).Exec(context.Background(),
		`UPDATE orders SET expire_at = now() - interval '1 minute' WHERE order_no = $1`, orderNo)
	if err != nil {
		t.Fatalf("把订单 %s 改成已过期失败: %v", orderNo, err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("改订单 %s 的过期时间影响了 %d 行 —— 夹具没找到这一单",
			orderNo, ct.RowsAffected())
	}
}

// placeRealOrder 走**真实的 HTTP 下单链路**下一笔单，返回单号与它扣掉的水位。
//
// 不用 seedDraftOrder 那种直插：这一组要验的是「SAGA 正向阶段扣掉的库存
// 被放回去了」，而直插的订单一件库存都没扣过 —— 那时「放回去了」是句废话。
func placeRealOrder(t *testing.T, host, merchantCode, addrName string, qty int) (
	orderNo string, skuID int64, beforeStock int32) {
	t.Helper()
	tok := login(t, host, seedPhone, seedPassword).AccessToken
	addr := addressIDOf(t, merchantCode, addrName)
	skuID, beforeStock = anySKUWithStock(t, merchantCode, int32(qty)+1)

	w := createOrder(t, host, orderBody(t, merchantCode, addr, skuID, qty, ""), tok, "sweep-"+uniqueKey())
	if w.Code != http.StatusCreated {
		t.Fatalf("下单失败：%d %s", w.Code, w.Body.String())
	}
	var order api.Order
	if err := json.Unmarshal(w.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if got := availableOf(t, skuID); got != beforeStock-int32(qty) {
		t.Fatalf("下单没有真的扣库存：%d → %d（期望 %d）—— "+
			"后面那条「补偿把货放回去了」的断言会因此失去区分力",
			beforeStock, got, beforeStock-int32(qty))
	}
	t.Cleanup(func() { dropOrder(t, order.OrderNo) })
	return order.OrderNo, skuID, beforeStock
}

// dropOrder 把一笔订单在库里留下的全部痕迹抹掉，**并把它净消耗掉的库存还回去**。
//
// 还库存这一步是必须的，而且必须按**流水的净额**算，不能按下单数量算：
// 这一组里有的订单走完了「扣减 + 超时回补」（净额 0，什么都不用还），
// 有的停在只扣了一次（净额 -n，要还 n）。按下单数量一律还 n 的话，
// 走完回补的那些会被多还一次，于是后面的测试拿到的「原始水位」是虚高的，
// 而一条「回补之后水位回到原值」的断言在虚高的基线上照样绿。
//
// 为什么要还：同一个包里的测试共用一个库，而种子给每个 SKU 的水位是有限的。
// 不还的话，跑在后面的测试会以「找不到水位够的 SKU」的形式失败 ——
// 真因是夹具泄漏，不是被测代码。这是本轮实际踩到的那个坑。
func dropOrder(t *testing.T, orderNo string) {
	t.Helper()
	ctx := context.Background()
	conn := adminSession(t)
	if _, err := conn.Exec(ctx, `
		UPDATE inventories i
		   SET available_qty = i.available_qty - l.net
		  FROM (SELECT sku_id, sum(change_qty)::int AS net
		          FROM inventory_logs WHERE biz_id = $1 GROUP BY sku_id) l
		 WHERE i.sku_id = l.sku_id`, orderNo); err != nil {
		t.Errorf("还原订单 %s 消耗的库存失败: %v", orderNo, err)
	}
	for _, stmt := range []string{
		`DELETE FROM inventory_logs WHERE biz_id = $1`,
		`DELETE FROM payment_returns WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
		`DELETE FROM payment_intents WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
		`DELETE FROM payments WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
		`DELETE FROM order_items WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
		`DELETE FROM orders WHERE order_no = $1`,
	} {
		if _, err := conn.Exec(ctx, stmt, orderNo); err != nil {
			t.Errorf("清理订单 %s 失败 (%s): %v", orderNo, stmt, err)
		}
	}
}

// 第一类：超时未支付 → 关单 + 库存回补 + 一行 biz_type = 3 的流水。
//
// 这是这个任务存在的全部意义：SAGA 在正向阶段就真实扣减，超卖为零、少卖存在，
// 而补偿任务跑不起来时少卖会变成**永久漏卖**（架构 §5 / M2 计划）。
//
// 三条断言各守一件事，缺一条都会让另外两条变成空转：
//
//	· 订单到 90         —— 只有这一条时，库存可以完全没动而测试照样绿；
//	· 水位回到原值       —— 只有这一条时，「从来没扣过」与「扣了又补」分不开；
//	· 流水恰好两行、
//	  第二行是 biz_type 3 —— 这一条才是「回补真的发生了、而且是超时任务干的」。
func TestSweepReleasesStockOfExpiredPendingOrders(t *testing.T) {
	const qty = 2
	orderNo, sku, before := placeRealOrder(t, hostA, "shop-a", seedAddressA, qty)
	if got := orderStatusOf(t, orderNo); got != 10 {
		t.Fatalf("下单之后订单状态是 %d，期望 10 待支付", got)
	}
	expire(t, orderNo)

	rep, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if rep.Released < 1 {
		t.Fatalf("这一轮 released=%d，期望至少 1 —— 这笔已过期的待支付订单没被扫到。"+
			"报告：%+v", rep.Released, rep)
	}

	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("订单 %s 的状态是 %d，期望 90 已关闭", orderNo, got)
	}
	if got := availableOf(t, sku); got != before {
		t.Fatalf("sku %d 的水位是 %d，期望回到 %d —— 库存没被放回去，这就是永久漏卖",
			sku, got, before)
	}

	logs := inventoryLogsOf(t, orderNo)
	if len(logs) != 2 {
		t.Fatalf("库存流水有 %d 行，期望 2 行（下单扣一次、超时放一次）：%+v\n"+
			"1 行说明回补根本没跑，而水位那条断言此刻是假绿（它看到的原值可能"+
			"只是因为扣减也没跑）", len(logs), logs)
	}
	deduct, release := logs[0], logs[1]
	if deduct.BizType != 1 || deduct.ChangeQty != -qty ||
		deduct.Before != before || deduct.After != before-qty {
		t.Fatalf("第一行不是一次下单扣减：%+v（期望 biz_type=1 change=-%d %d→%d）",
			deduct, qty, before, before-qty)
	}
	if release.BizType != 3 {
		t.Fatalf("第二行的 biz_type 是 %d，期望 3 超时关单释放 —— "+
			"写成 2（SAGA 补偿回补）的话，「这批货被锁了多久」和「转化率在哪一步掉的」"+
			"两个问题都答不出来了：%+v", release.BizType, release)
	}
	if release.ChangeQty != qty || release.Before != before-qty || release.After != before {
		t.Fatalf("第二行不是一次真回补：%+v（期望 change=+%d %d→%d）",
			release, qty, before-qty, before)
	}
	t.Logf("订单 %s：10 →（超时）→ 90，水位 %d → %d → %d，流水 %+v / %+v",
		orderNo, before, before-qty, before, deduct, release)
}

// 第二类：孤儿草稿 → 只关单，**一件库存都不许动**。
//
// status = 0 的订单从没进过 SAGA：编排是「建单在前、库存在后」，订单还停在 0
// 说明建单分支的正向没成功，而库存分支排在它后面，连开始都没开始。
//
// 「不回补」这条断言必须同时看水位和流水：给它加一次回补，水位会变（水位那条红），
// 而且会多一行 biz_type = 3 的流水（流水那条也红）。只看其中一条时，
// 一次「回补了但没记流水」或者「记了流水但没回补」都能溜过去。
func TestSweepClosesOrphanDraftsWithoutTouchingStock(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, _ := seedDraftOrder(t, "shop-a", sku, 1)
	if got := orderStatusOf(t, orderNo); got != 0 {
		t.Fatalf("夹具造出来的订单状态是 %d，期望 0 创建中", got)
	}
	expire(t, orderNo)

	rep, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if rep.ClosedDrafts < 1 {
		t.Fatalf("这一轮 closed_drafts=%d，期望至少 1 —— 孤儿草稿没被扫到。报告：%+v",
			rep.ClosedDrafts, rep)
	}

	if got := orderStatusOf(t, orderNo); got != 90 {
		t.Fatalf("孤儿草稿 %s 的状态是 %d，期望 90 —— 它没人关，就是一行永远留在库里的垃圾",
			orderNo, got)
	}
	if got := availableOf(t, sku); got != before {
		t.Fatalf("sku %d 的水位从 %d 变成了 %d —— 孤儿草稿一件库存都没扣过，"+
			"「回补」它等于凭空造出 %d 件货", sku, before, got, got-before)
	}
	if logs := inventoryLogsOf(t, orderNo); len(logs) != 0 {
		t.Fatalf("孤儿草稿留下了 %d 行库存流水：%+v —— 应该一行都没有", len(logs), logs)
	}
	// 阳性对照：这一轮里 Released 与 ClosedDrafts 是两个独立的计数，
	// 孤儿走的必须是后者。走错分支（当成第一类去回补）时上面三条会红，
	// 但这一条能直接说出走错了哪条路。
	t.Logf("孤儿草稿 %s：0 → 90，水位始终 %d，流水 0 行（report=%+v）", orderNo, before, rep)
}

// 孤儿草稿身上有库存流水时，**不许关，要报**。
//
// 「孤儿不回补库存」这条规则的依据是另一个文件里的一个常量
// （service/order_saga.go 的 sagaSteps：建单在前、库存在后）。哪天有人把那两行
// 对调，孤儿清理就会开始静默地漏掉库存回补 —— 水位、订单状态、日志全都正常，
// 只有对账能发现，而且是几个月之后。
//
// 所以关单之前有一次点查（repository.AssertNoInventoryLog）。这条测试造出那个
// 不该存在的状态，断言任务**拒绝关单**并计进 Failed：关掉它的话线索会一起消失，
// 而留着它意味着每一轮都会再报一次，直到有人来看。
//
// 删掉 AssertNoInventoryLog 那一行，这条测试当场红（订单会变成 90）。
func TestSweepRefusesToCloseADraftThatHasInventoryLogs(t *testing.T) {
	sku, before := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	expire(t, orderNo)

	// 手工伪造一行流水：模拟「编排顺序被改过，库存分支先跑了」。
	// 走管理员连接直插，因为这个状态在正常代码路径上造不出来 —— 那正是重点。
	// store_id 是 NOT NULL（00020）：before_available / after_available 记的
	// 是**某一家门店**的水位，不写下是哪一家，同一个 SKU 在多家店的流水会
	// 交织成一条对不平的序列。这一行要伪装成「库存分支真的跑过」，
	// 所以它挂的门店必须就是上面那笔夹具订单的那一家 —— 取种子里那家默认店。
	if _, err := admin(t).Exec(context.Background(), `
		INSERT INTO inventory_logs (merchant_id, sku_id, store_id, change_qty, biz_type,
		                            biz_id, before_available, after_available)
		SELECT $1, $2, st.id, -1, 1, $3, $4, $5
		  FROM stores st
		 WHERE st.merchant_id = $1 AND st.is_default AND st.deleted_at IS NULL`,
		merchantID, sku, orderNo, before, before-1); err != nil {
		t.Fatalf("造流水夹具失败: %v", err)
	}

	rep, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if rep.Failed < 1 {
		t.Fatalf("这一轮 failed=%d，期望至少 1 —— 一笔带着库存流水的孤儿草稿"+
			"被当成正常的孤儿处理了。报告：%+v", rep.Failed, rep)
	}
	if got := orderStatusOf(t, orderNo); got != 0 {
		t.Fatalf("订单 %s 的状态是 %d，期望仍然是 0 —— "+
			"关掉它会让「这一单的库存到底扣没扣」这条线索一起消失", orderNo, got)
	}
	if got := availableOf(t, sku); got != before {
		t.Fatalf("sku %d 的水位被动了：%d → %d —— 拒绝这一单时不该有任何写入", sku, before, got)
	}
	t.Logf("带库存流水的孤儿草稿被拒：状态仍是 0，failed=%d", rep.Failed)
}

// 定时任务在**没有任何 HTTP 请求**的情况下也能跨租户工作。
//
// 这是硬约束三：任务没有 Host、没有请求上下文，它怎么拿到租户？
// 答案是枚举 merchants（tenant-root 类，没有 RLS），再一家一家进 WithTenant。
//
// 这条测试用一个光秃秃的 context.Background()（里面没有任何租户）驱动一轮，
// 并断言**两家店的订单都被处理了**。只测一家的话，一个写死默认租户的实现
// 照样能过 —— 而那正是这条断言要挡的东西。
func TestSweepFindsEveryTenantWithoutAnyRequestContext(t *testing.T) {
	skuA, beforeA := anySKUWithStock(t, "shop-a", 2)
	orderA, _ := seedDraftOrder(t, "shop-a", skuA, 1)
	expire(t, orderA)

	skuB, beforeB := anySKUWithStock(t, "shop-b", 2)
	orderB, _ := seedDraftOrder(t, "shop-b", skuB, 1)
	expire(t, orderB)

	// ctx 里什么都没有：没有租户、没有 Host、没有请求。
	rep, err := newSweeper(service.SweepConfig{}).SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if rep.Tenants < 2 {
		t.Fatalf("这一轮只走了 %d 家商户 —— merchants 枚举没生效。报告：%+v", rep.Tenants, rep)
	}

	for _, c := range []struct {
		code, orderNo string
		sku           int64
		before        int32
	}{{"shop-a", orderA, skuA, beforeA}, {"shop-b", orderB, skuB, beforeB}} {
		if got := orderStatusOf(t, c.orderNo); got != 90 {
			t.Fatalf("%s 的订单 %s 状态是 %d，期望 90 —— 这家店没被扫到",
				c.code, c.orderNo, got)
		}
		if got := availableOf(t, c.sku); got != c.before {
			t.Fatalf("%s 的 sku %d 水位被动了：%d → %d", c.code, c.sku, c.before, got)
		}
	}
	t.Logf("一个空 ctx 驱动的一轮扫过 %d 家商户，shop-a 与 shop-b 的订单都关了", rep.Tenants)
}

// 公平调度：一个大商家的积压不能把小商家饿死。
//
// 数据模型 §12 的形状是「每租户上限 + 兜底再取一次」。这里把每租户上限压到 1、
// 总预算压到 2，然后给 shop-a 造 3 笔积压、给 shop-b 造 1 笔。
//
// 没有公平调度时（按 id 顺序、不设每租户上限），一轮的 2 个预算会全被 shop-a
// 吃掉，shop-b 那笔一次也轮不到 —— 那正是「饿死」的样子。
//
// 断言写成「shop-b 那笔在**第一轮**就被处理了」，而不是「早晚会被处理」：
// 后者在任何实现下都成立（跑够多轮总会轮到），是一个恒真的不等式。
func TestSweepDoesNotStarveASmallTenant(t *testing.T) {
	skuB, _ := anySKUWithStock(t, "shop-b", 2)
	orderB, _ := seedDraftOrder(t, "shop-b", skuB, 1)
	expire(t, orderB)

	var backlog []string
	for i := 0; i < 3; i++ {
		skuA, _ := anySKUWithStock(t, "shop-a", 2)
		o, _ := seedDraftOrder(t, "shop-a", skuA, 1)
		expire(t, o)
		backlog = append(backlog, o)
	}

	// 每租户上限 1、总预算 2：够 shop-a 与 shop-b 各一笔，不够 shop-a 吃掉全部。
	rep, err := newSweeper(service.SweepConfig{PerTenantCap: 1, RoundBudget: 2}).
		SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}

	if got := orderStatusOf(t, orderB); got != 90 {
		closed := 0
		for _, o := range backlog {
			if orderStatusOf(t, o) == 90 {
				closed++
			}
		}
		t.Fatalf("shop-b 的订单 %s 状态是 %d（期望 90），而 shop-a 的 3 笔积压里"+
			"关掉了 %d 笔 —— 一轮的预算被大商家吃光了，小商家被饿死。报告：%+v",
			orderB, got, closed, rep)
	}

	// 阳性对照：预算确实是紧的。shop-a 的 3 笔不可能在这一轮全被处理，
	// 否则「预算被吃光」这件事根本没有发生的余地，上面那条断言是空转。
	closed := 0
	for _, o := range backlog {
		if orderStatusOf(t, o) == 90 {
			closed++
		}
	}
	if closed >= len(backlog) {
		t.Fatalf("shop-a 的 %d 笔积压在一轮里全关掉了 —— 总预算（%d）没有生效，"+
			"上面那条公平断言没有区分力", len(backlog), 2)
	}
	t.Logf("一轮预算 2、每租户上限 1：shop-b 那笔被处理了，shop-a 的 3 笔只处理了 %d 笔",
		closed)
}

// 兜底那一趟：只剩一个租户有积压时，不许让 worker 空转。
//
// §12 的原话：「公平的目的是防饿死，不是让机器闲着。」
// 只有「每租户上限」那一条的话，系统里只剩一家店有任务时，它会被自己的上限
// 卡住，而预算白白剩着。
//
// 这条测试给 shop-b 一家造 3 笔，每租户上限 1、总预算 3。没有兜底时一轮只关得掉
// 1 笔（上限）；有兜底时第二趟去掉上限，把剩下的预算用完。
func TestSweepFallsBackWhenOnlyOneTenantHasBacklog(t *testing.T) {
	var orders []string
	for i := 0; i < 3; i++ {
		sku, _ := anySKUWithStock(t, "shop-b", 2)
		o, _ := seedDraftOrder(t, "shop-b", sku, 1)
		expire(t, o)
		orders = append(orders, o)
	}

	rep, err := newSweeper(service.SweepConfig{PerTenantCap: 1, RoundBudget: 3}).
		SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if !rep.Fallback {
		t.Fatalf("这一轮没有跑兜底（fallback=false）—— 有租户被上限卡住、"+
			"而预算还剩着，第二趟必须发生。报告：%+v", rep)
	}

	closed := 0
	for _, o := range orders {
		if orderStatusOf(t, o) == 90 {
			closed++
		}
	}
	if closed != len(orders) {
		t.Fatalf("shop-b 的 %d 笔只关掉了 %d 笔，而这一轮预算是 3 —— "+
			"兜底没有把剩下的预算用完，worker 在空转。报告：%+v",
			len(orders), closed, rep)
	}
	t.Logf("只剩一家有积压时兜底生效：上限 1、预算 3，一轮关掉了 %d 笔", closed)
}

// payableOf 读一笔订单的应付金额（支付回调要用它）。
func payableOf(t *testing.T, orderNo string) int64 {
	t.Helper()
	var cents int64
	if err := admin(t).QueryRow(context.Background(),
		`SELECT payable_cents FROM orders WHERE order_no = $1`, orderNo).Scan(&cents); err != nil {
		t.Fatalf("读订单 %s 的应付金额失败: %v", orderNo, err)
	}
	return cents
}
