package handler_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 上一轮（任务 5）留下的两笔账里，本轮收掉的那一笔。
//
// # 账：SubmitSaga 失败时，那把幂等键被锁死 24 小时
//
// 抢占记录与订单草稿在**同一个事务**里提交（service/order.go 第一段），
// 然后才提交 SAGA。所以 SubmitSaga 失败时，库里留下的是：一行 status = 0 的
// 幂等记录 + 一笔 status = 0 的草稿订单，而 SAGA 一个分支都没跑过。
//
// 不撤销那行抢占的话，客户端拿**同一把钥匙**重试会一直撞 409 处理中，
// 直到 24 小时后 expire_at 过期 —— 而「同一个逻辑请求的重试用同一把钥匙」
// 正是 Idempotency-Key 的语义。也就是说我们锁死的是客户端**正确的**行为，
// 它唯一的出路是换一把钥匙，而那恰恰是幂等协议要它别做的。
//
// 另一笔（WaitFinal 超时后幂等行永远停在「处理中」）本轮**没有**收，
// 理由与两条候选改法各自的问题写在 service/order.go 的那段注释里。

// failingCoordinator 是一个 SubmitSaga 永远失败的协调器。
//
// 为什么要它：真协调器在这套测试环境里没有一条可控的失败路径 ——
// sagaSteps 是常量（写不出未注册的分支名），gid 由随机订单号生成（撞不出重复），
// 而把真协调器关掉会毁掉同一个包里其他所有测试。
//
// 它替换的是**唯一**一个被 mock 掉的东西：service.Coordinator 这个接口
// （两个方法）。repo 是真的、库是真的、幂等键与订单都真的落了库 ——
// 被测的正是「落了库之后怎么收拾」。
type failingCoordinator struct{}

var errSubmitRefused = errors.New("协调器拒绝了这笔提交（测试构造）")

func (failingCoordinator) SubmitSaga(string, string) error { return errSubmitRefused }

func (failingCoordinator) WaitFinal(string, int) (string, error) {
	// 走不到：SubmitSaga 先失败了。真被调到说明被测代码在提交失败之后
	// 还继续往下走了，那本身就是 bug，所以这里 panic 而不是回一个假终态。
	panic("SubmitSaga 已经失败了，不该再等终态")
}

// SubmitSaga 失败之后，同一把钥匙必须还能用。
//
// 三段：
//
//	① 用一个注定失败的协调器下单 —— 抢占与草稿都真的落了库，然后提交失败；
//	② 断言那行抢占**被撤掉了**（直接读库，不打 HTTP）；
//	③ **拿同一把钥匙、同一个请求体，走真实的 HTTP 链路再下一次，必须成功。**
//
// ③ 才是这条账真正的验收：只断言 ② 的话，「DELETE 跑了」与「客户端真的能重试」
// 之间还隔着幂等那一整段逻辑。而 ③ 走的是和真实客户端完全一样的路。
//
// 变异：把 service/order.go 里那句 s.releaseKey(...) 删掉，② 与 ③ 同时红
// （③ 会拿到 409 处理中）。
func TestSubmitSagaFailureReleasesTheIdempotencyKey(t *testing.T) {
	merchantID := merchantIDOf(t, "shop-a")
	addr := addressIDOf(t, "shop-a", seedAddressA)
	sku, before := anySKUWithStock(t, "shop-a", 2)
	key := "submitfail-" + uniqueKey()

	// ① 注定失败的协调器 + 真 repo + 真库。
	//
	// ctx 手工拼出租户与身份，因为这一段刻意不走 HTTP —— 要替换的正是
	// 路由里那一个协调器，而路由不提供替换它的口子。
	ctx := tenant.NewContext(context.Background(), merchantID)
	ctx = auth.NewContext(ctx, auth.Identity{UserID: seedUserID(t, "shop-a"), SessionID: 1})

	svc := service.NewOrderService(repository.New(testPool), failingCoordinator{}, nil)
	_, err := svc.Create(ctx, service.CreateRequest{
		Items:     []service.LineInput{{SKUID: sku, Quantity: 1}},
		AddressID: addr,
		// store_id 是必填的（00020），服务端没有回落分支。不填的话这一段
		// 会在「门店不合法」上失败，而这条测试要的失败是**提交失败**——
		// 两者都返回错误，而第 ① 段的前置条件（抢占与草稿真的落了库）
		// 只有后者成立。
		StoreID: storeIDOf(t, "shop-a"),
	}, key)
	if !errors.Is(err, errSubmitRefused) {
		t.Fatalf("下单返回的错误是 %v，期望包着 errSubmitRefused —— "+
			"这条测试的前置（提交真的失败了）没有成立", err)
	}

	// 阳性对照：抢占**确实发生过**。没有这一条，「现在查不到那一行」可能只是
	// 因为它从来就没被插进去过，而那样第 ② 段是空转。
	// 草稿订单是抢占同事务的产物，它在 = 抢占也在过。
	var drafts int
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM orders
		 WHERE merchant_id = $1 AND status = 0 AND created_at > now() - interval '1 minute'`,
		merchantID).Scan(&drafts); err != nil {
		t.Fatal(err)
	}
	if drafts == 0 {
		t.Fatal("库里没有刚落下的 status = 0 草稿订单 —— 第一段事务根本没提交，" +
			"那么「抢占记录被撤掉了」证明不了任何事")
	}

	// ② 抢占记录没了。
	if n := idempotencyRowsFor(t, "shop-a", key); n != 0 {
		t.Fatalf("幂等键 %s 还留着 %d 行 —— 它会在 24 小时里一直回 409 处理中，"+
			"而客户端拿同一把钥匙重试正是 Idempotency-Key 的语义", key, n)
	}

	// ③ 同一把钥匙、真实 HTTP 链路，必须能下单成功。
	tok := login(t, hostA, seedPhone, seedPassword).AccessToken
	w := createOrder(t, hostA, orderBody(t, "shop-a", addr, sku, 1, ""), tok, key)
	if w.Code != http.StatusCreated {
		t.Fatalf("拿同一把钥匙重试返回 %d，期望 201：%s\n"+
			"409 说明那行抢占没撤掉；其它码说明重试路径上还有别的问题",
			w.Code, w.Body.String())
	}
	if got := availableOf(t, sku); got != before-1 {
		t.Fatalf("重试那一单没有真的扣库存：水位 %d，期望 %d —— "+
			"「重试成功了」不能只看状态码", got, before-1)
	}
	// 夹具不留痕：这一单真的扣了一件货，不还回去的话跑在后面的测试会以
	// 「找不到水位够的 SKU」的形式失败，而真因是这里泄漏了库存。
	t.Cleanup(func() { dropOrder(t, orderNoForKey(t, "shop-a", key)) })
	t.Logf("SubmitSaga 失败后抢占被撤销，同一把钥匙 %s 重试成功下单（水位 %d → %d）",
		key, before, before-1)
}

// idempotencyRowsFor 数某家店下某把钥匙在 idempotency_keys 里有几行。
func idempotencyRowsFor(t *testing.T, merchantCode, key string) int {
	t.Helper()
	var n int
	if err := admin(t).QueryRow(context.Background(), `
		SELECT count(*) FROM idempotency_keys k
		  JOIN merchants m ON m.id = k.merchant_id
		 WHERE m.code = $1 AND k.idem_key = $2`, merchantCode, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedUserID 取某家店种子买家的 id。
func seedUserID(t *testing.T, merchantCode string) int64 {
	t.Helper()
	var id int64
	if err := admin(t).QueryRow(context.Background(), `
		SELECT u.id FROM users u JOIN merchants m ON m.id = u.merchant_id
		 WHERE m.code = $1 AND u.phone = $2 AND u.deleted_at IS NULL`,
		merchantCode, seedPhone).Scan(&id); err != nil {
		t.Fatalf("取 %s 的种子买家失败: %v", merchantCode, err)
	}
	return id
}

// 建单补偿关不掉一笔**已经被付掉**的订单时，必须有声音。
//
// # 这个状态今天不可达，那为什么还要测它
//
// 它是「建单在前、库存在后」那个窗口真的被踩到时的样子：订单已经是 10 待支付、
// 库存还没扣，这时用户付了钱（10 → 20），随后库存分支失败触发全局补偿 ——
// 而补偿那条 UPDATE 带着 `status IN (0, 10)`，对一笔 20 的订单影响 0 行。
//
// 上一轮那段代码把 0 行一律当成功（理由是「补偿必须幂等」），于是这一单会
// 安静地停在「已支付」，而它一件库存都没有。钱在里面，货没有。
//
// **窗口今天不可达**，靠的是 order_no 只在 SAGA 到终态之后才对外返回
// （service/order.go 的 Create 第三段）—— 支付回调必须带着单号才找得到订单。
// 但那条不变量住在另一个函数里：哪天有人让 Create 提前返回单号（比如为了
// 「让前端早点开始轮询」），这个洞就开了，而在此之前没有任何东西会提醒他。
//
// 所以这条测试**直接把那个状态构造出来**，并断言两件事：
//
//	· 补偿仍然返回成功 —— 报错会让协调器无限重试一件它改不了的事；
//	· 但它留下一条 Error 日志 —— 不可达不等于不存在。
//
// 日志是这里唯一的可观察点，所以测试临时换掉默认 logger。换而不是加一个
// 「上次有没有报警」的字段：那个字段只为测试存在，而且它会让生产代码多一处
// 状态；日志本来就是这条路径的产物，测它就是测真的东西。
func TestCreateCompensationShoutsWhenTheOrderWasAlreadyPaid(t *testing.T) {
	sku, _ := anySKUWithStock(t, "shop-a", 2)
	orderNo, merchantID := seedDraftOrder(t, "shop-a", sku, 1)
	gid := gidFor(t, merchantID, orderNo)
	// seedDraftOrder 的清理不认得 payments（它比支付回调早一轮）。在它之后注册，
	// 于是按 LIFO 先跑，否则那边删 orders 会撞外键。
	t.Cleanup(func() {
		if _, err := admin(t).Exec(context.Background(),
			`DELETE FROM payments WHERE order_id IN (SELECT id FROM orders WHERE order_no = $1)`,
			orderNo); err != nil {
			t.Errorf("清理支付单失败: %v", err)
		}
	})

	// 把订单推到 10，再用一条真回调把它推到 20 —— 走的都是生产路径。
	if got := branchOf(t, service.BranchOrderCreate)(gid, "01", "action"); got != dtm.Success {
		t.Fatalf("建单分支正向返回 %d，期望 Success", got)
	}
	payable := payableOf(t, orderNo)
	if w := notifyPayment(t, hostA, "shop-a", "wechat",
		payload(orderNo, "paidwindow-"+uniqueKey(), payable)); w.Code != http.StatusOK {
		t.Fatalf("支付回调失败：%d %s", w.Code, w.Body.String())
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("前置没成立：订单状态是 %d，期望 20", got)
	}

	// 现在让建单分支的补偿跑一次 —— 这正是窗口被踩到时会发生的事。
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// 补偿必须用**与正向同一个** branchID："01"。屏障的空回滚保护是按
	// (gid, branch_id) 判的 —— 换一个号，屏障会以为这个分支的正向从没来过，
	// 判成 NullCompensation 直接空转，那样这条测试测的就是屏障，不是补偿。
	if got := branchOf(t, service.BranchOrderCreateUndo)(gid, "01", "compensate"); got != dtm.Success {
		t.Fatalf("建单补偿返回 %d，期望 Success(%d) —— 补偿对一件它改不了的事报错，"+
			"协调器会无限重试", got, dtm.Success)
	}
	if got := orderStatusOf(t, orderNo); got != 20 {
		t.Fatalf("补偿把一笔已支付的订单改成了 %d —— 钱已经收了", got)
	}

	if !strings.Contains(buf.String(), "建单补偿关不掉这一单") {
		t.Fatalf("补偿关不掉一笔已支付的订单，却一声不响。捕获到的 Error 日志：\n%s\n"+
			"这个洞靠的是「单号在 SAGA 到终态前不外泄」这条住在别处的不变量，"+
			"它一旦被打开，必须有声音", buf.String())
	}
	t.Logf("补偿对已支付订单返回 Success 且没有改动它，同时留下了 Error 日志")
}
