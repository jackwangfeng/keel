package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 子事务屏障的测试。**每一种判定都由真实数据库走出来**，没有桩。
//
// 断言的形状是刻意的：每条都同时钉住「判定是什么」「业务到底有没有发生」两件事。
// 只查其中一个的话，两类完全不同的故障会给出同一个绿：
//
//   - 只查判定：屏障判对了但业务照跑，库存被扣两次，测试全绿；
//   - 只查库存：屏障整个失灵而业务恰好失败了，测试也全绿。
//
// 库存水位一律用管理员连接读（availableQty，见 inventory_test.go），读的是
// 库里的真实状态，不是被测代码自己的返回值。

// barrierGID 造一个本次运行独有的 gid，形状与 dtm.OrderGID 一致
// （order-{merchant_id}-{order_no}）。
//
// 这里手拼而不是 import internal/dtm：WithSagaBranch **不解析 gid**，它对屏障
// 而言只是一个不透明的字符串。测试跟着走同一个假设，顺便也不把 cgo 拖进这个
// 测试二进制。
func barrierGID(t *testing.T, merchantID int64) string {
	t.Helper()
	gid := fmt.Sprintf("order-%d-t2%d", merchantID, time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, db.AdminDSN())
		if err != nil {
			t.Errorf("清理屏障行时连不上: %v", err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, `DELETE FROM barrier WHERE gid = $1`, gid); err != nil {
			t.Errorf("清理屏障行失败: %v", err)
		}
	})
	return gid
}

// barrierRows 数这个 gid 在屏障表里留下的行。
//
// 必须走管理员连接：keel_app 在 barrier 上**只有 INSERT**，它读不到自己写的
// 东西（00008_barrier.sql 的 GRANT 面）。这也正是这个断言有区分力的原因——
// 它查的是库里的行，不是被测代码愿意说的话。
func barrierRows(t *testing.T, gid string) int {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var n int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM barrier WHERE gid = $1`, gid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// failIfBarrierDenied 把「屏障 SQL 被改坏了」从「附近别的东西坏了」里挑出来。
//
// keel_app 在 barrier 上只有 INSERT，于是 `ON CONFLICT (gid, ...) DO NOTHING`
// 这种带冲突目标的写法会当场 42501。没有这一句的话，那次改动的症状是一堆
// 测试一起红，而错误信息停在「期望 Execute，实得 None」上，不指向真因。
func failIfBarrierDenied(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, repository.ErrBarrierDenied) {
		t.Fatalf("屏障 INSERT 被权限拒绝 —— 它的 SQL 是不是带上了冲突目标？"+
			"keel_app 在 barrier 上只有 INSERT，带冲突目标要额外的 SELECT 权。原始错误: %v", err)
	}
}

// 判定一：Execute —— 第一次投递执行业务；判定三：Duplicated —— 第二次不执行。
//
// 顺带钉住「屏障按 (gid, branch_id, op) 分开」：换一个 branch_id 必须重新是
// Execute。少了这一条，一个把 branch_id 漏出主键的屏障也能让上面两条全绿，
// 而它的真实后果是同一笔 SAGA 的第二个分支被判成重复，直接跳过业务。
func TestSagaBranchExecutesOnceThenReportsDuplicated(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	calls := 0
	deduct := func(q repository.Tx) error {
		calls++
		_, err := q.DeductInventory(ctx, f.skuA, 1)
		return err
	}

	d, err := r.WithSagaBranch(asA, gid, "01", "action", deduct)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("第一次投递失败: %v", err)
	}
	if d != repository.DecisionExecute {
		t.Fatalf("第一次判定是 %v，期望 Execute", d)
	}
	if calls != 1 {
		t.Fatalf("业务被调用 %d 次，期望 1 —— Execute 必须真的跑业务", calls)
	}
	if got := availableQty(t, f.skuA); got != 9 {
		t.Fatalf("水位是 %d，期望 9 —— 判定说 Execute，但业务没落地", got)
	}

	// 同一个 (gid, branch_id, op) 再来一次：协调器重投的样子。
	d, err = r.WithSagaBranch(asA, gid, "01", "action", deduct)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("重复投递返回了失败: %v —— 重复是**正常路径**，"+
			"返回失败会让协调器误判并无限重试", err)
	}
	if d != repository.DecisionDuplicated {
		t.Fatalf("第二次判定是 %v，期望 Duplicated", d)
	}
	if calls != 1 {
		t.Fatalf("业务被调用 %d 次，期望仍是 1 —— 重复投递把业务又跑了一遍", calls)
	}
	if got := availableQty(t, f.skuA); got != 9 {
		t.Fatalf("水位是 %d，期望仍是 9 —— 同一个分支被扣了两次", got)
	}

	// 换一个分支号：是同一笔全局事务里的另一个分支，必须重新执行。
	d, err = r.WithSagaBranch(asA, gid, "02", "action", deduct)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("第二个分支失败: %v", err)
	}
	if d != repository.DecisionExecute {
		t.Fatalf("分支 02 的判定是 %v，期望 Execute —— 屏障把不同分支当成了同一个", d)
	}
	if got := availableQty(t, f.skuA); got != 8 {
		t.Fatalf("水位是 %d，期望 8 —— 第二个分支的业务没跑", got)
	}
}

// 判定二：NullCompensation —— 补偿先于正向到达，什么都不做、返回成功。
// 以及紧接着的悬挂：迟到的正向必须**不执行**。
//
// 这是最容易写错的一条，因为它的两种错法方向相反：
//   - 补偿真的回补了 —— 凭空多出库存；
//   - 迟到的正向真的扣了 —— 一笔已经被全局回滚的订单把库存扣走，再没人补回来。
//
// 所以两次调用都断言水位一动不动，而水位是管理员连接读出来的真实值。
func TestSagaBranchNullCompensationThenSuspendedAction(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	before := availableQty(t, f.skuA)

	restored := 0
	restore := func(q repository.Tx) error {
		restored++
		_, err := q.RestoreInventory(ctx, f.skuA, 5)
		return err
	}
	deducted := 0
	deduct := func(q repository.Tx) error {
		deducted++
		_, err := q.DeductInventory(ctx, f.skuA, 5)
		return err
	}

	// 补偿先到，而正向分支从来没有执行过。
	d, err := r.WithSagaBranch(asA, gid, "01", "compensate", restore)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("空回滚返回了失败: %v —— 空回滚是**正常路径**，"+
			"返回失败会让协调器无限重试一件本来就不该做的事", err)
	}
	if d != repository.DecisionNullCompensation {
		t.Fatalf("判定是 %v，期望 NullCompensation", d)
	}
	if restored != 0 {
		t.Fatalf("补偿业务被调用了 %d 次 —— 正向从未执行，这次回补是凭空加库存", restored)
	}
	if got := availableQty(t, f.skuA); got != before {
		t.Fatalf("水位从 %d 变成了 %d —— 空回滚把库存真的补回去了", before, got)
	}

	// 悬挂：迟到的正向。它在屏障里的位置已经被上面那次补偿占掉了。
	d, err = r.WithSagaBranch(asA, gid, "01", "action", deduct)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("迟到的正向返回了失败: %v —— 它是正常路径（Duplicated）", err)
	}
	if d != repository.DecisionDuplicated {
		t.Fatalf("迟到的正向判定是 %v，期望 Duplicated", d)
	}
	if deducted != 0 {
		t.Fatalf("迟到的正向业务被调用了 %d 次 —— 这笔扣减再也不会有人补回来", deducted)
	}
	if got := availableQty(t, f.skuA); got != before {
		t.Fatalf("水位从 %d 变成了 %d —— 悬挂的正向真的扣了库存", before, got)
	}
}

// 正常链路：正向执行 → 补偿执行 → 重复补偿空转。
//
// 没有这一条的话，上面那条「空回滚不执行业务」可能只是因为补偿路径根本不工作。
func TestSagaBranchCompensatesAfterARealAction(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	deduct := func(q repository.Tx) error { _, e := q.DeductInventory(ctx, f.skuA, 4); return e }
	restore := func(q repository.Tx) error { _, e := q.RestoreInventory(ctx, f.skuA, 4); return e }

	d, err := r.WithSagaBranch(asA, gid, "01", "action", deduct)
	failIfBarrierDenied(t, err)
	if err != nil || d != repository.DecisionExecute {
		t.Fatalf("正向判定 %v，错误 %v，期望 Execute/nil", d, err)
	}
	if got := availableQty(t, f.skuA); got != 6 {
		t.Fatalf("正向之后水位是 %d，期望 6", got)
	}

	d, err = r.WithSagaBranch(asA, gid, "01", "compensate", restore)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("补偿失败: %v", err)
	}
	if d != repository.DecisionExecute {
		t.Fatalf("补偿判定是 %v，期望 Execute —— 正向真的执行过，这次补偿必须落地", d)
	}
	if got := availableQty(t, f.skuA); got != 10 {
		t.Fatalf("补偿之后水位是 %d，期望 10", got)
	}

	d, err = r.WithSagaBranch(asA, gid, "01", "compensate", restore)
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("重复补偿返回了失败: %v", err)
	}
	if d != repository.DecisionDuplicated {
		t.Fatalf("重复补偿判定是 %v，期望 Duplicated", d)
	}
	if got := availableQty(t, f.skuA); got != 10 {
		t.Fatalf("重复补偿之后水位是 %d，期望 10 —— 补了两次", got)
	}
}

// **屏障与业务同事务**：业务失败时，屏障那行必须跟着消失。
//
// 这一条不是读代码能确认的，所以用一次真实的失败去验，三个角度一起看：
//
//	① 错误原样上浮（是业务那个错，不是别的东西炸了）；
//	② 库存没动；
//	③ 管理员连接在屏障表里数到 0 行；
//	④ **紧接着的重试仍然是 Execute 且业务真的落地**。
//
// ④ 是这条测试的重心。屏障若先落地（比如有人把它挪去一条独立连接上发），
// ①②③ 里的 ③ 会红，而更要命的后果是 ④：协调器的下一次重试被判成 Duplicated
// 直接跳过业务，正向阶段扣掉的库存再没人回补——架构 §5 的「少卖」从可恢复
// 变成永久漏账。
func TestSagaBranchRollsBackTheBarrierWithTheBusiness(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	// 一个只属于这条测试的错误：断言认得出它，就不会把「别的东西也失败了」
	// 误读成「业务失败被正确地上浮了」。
	errBoom := errors.New("业务自己炸了")

	d, err := r.WithSagaBranch(asA, gid, "01", "action", func(q repository.Tx) error {
		if _, e := q.DeductInventory(ctx, f.skuA, 3); e != nil {
			return e
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("期望业务错误原样上浮，实得 %v", err)
	}
	// 出错时不报判定：事务已经回滚，这次调用等于没发生过。
	if d.String() != "None" {
		t.Fatalf("失败的调用报回了判定 %v —— 调用方少看一眼 err 就会以为业务做过了", d)
	}
	if got := availableQty(t, f.skuA); got != 10 {
		t.Fatalf("水位是 %d，期望 10 —— 业务失败了却留下了扣减", got)
	}
	if n := barrierRows(t, gid); n != 0 {
		// 用 Errorf 而不是 Fatalf：让下面那次重试也跑完。屏障若先落地，
		// 真正要命的不是多出来的那一行，是重试被判成 Duplicated——两句一起报，
		// 读日志的人不必再猜后果。
		t.Errorf("业务失败后屏障表里还有 %d 行 —— 屏障没跟业务同事务提交。"+
			"下一次重试会被判成 Duplicated，这笔扣减再也不会发生", n)
	}

	// 重试：协调器重放的样子。必须重新是 Execute，而且业务这次真的落地。
	d, err = r.WithSagaBranch(asA, gid, "01", "action", func(q repository.Tx) error {
		_, e := q.DeductInventory(ctx, f.skuA, 3)
		return e
	})
	failIfBarrierDenied(t, err)
	if err != nil {
		t.Fatalf("重试失败: %v", err)
	}
	if d != repository.DecisionExecute {
		t.Fatalf("重试的判定是 %v，期望 Execute —— 上一次的屏障行留下来了，"+
			"重试被当成了重复请求", d)
	}
	if got := availableQty(t, f.skuA); got != 7 {
		t.Fatalf("重试之后水位是 %d，期望 7 —— 重试没有真的扣到库存", got)
	}
}

// 未知的 op 必须报错，不能被当成正向分支。
//
// dtmrs 那边 op 是 enum，过了 C ABI 只剩字符串，那个保证没跟过来。一个拼错的
// "compensat" 若被当成正向：空回滚保护整个失效（它不再抢正向的位置），
// 悬挂的正向也照样执行。症状是一次跨越重启的、不可复现的库存错账。
func TestSagaBranchRefusesUnknownOp(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	for _, op := range []string{"compensat", "", "ACTION", "action "} {
		called := false
		d, err := r.WithSagaBranch(asA, gid, "01", op, func(q repository.Tx) error {
			called = true
			_, e := q.DeductInventory(ctx, f.skuA, 1)
			return e
		})
		if !errors.Is(err, repository.ErrUnknownBranchOp) {
			t.Fatalf("op=%q：期望 ErrUnknownBranchOp，实得 %v（判定 %v）", op, err, d)
		}
		if called {
			t.Fatalf("op=%q：业务被执行了", op)
		}
	}
	if n := barrierRows(t, gid); n != 0 {
		t.Fatalf("被拒的 op 在屏障表里留下了 %d 行", n)
	}
	if got := availableQty(t, f.skuA); got != 10 {
		t.Fatalf("水位是 %d，期望 10", got)
	}
}

// 没有租户上下文时，屏障这条路也必须当场失败。
//
// 它守的是「WithSagaBranch 不是绕开租户检查的第二条路」。这条路要是漏了，
// 它比 WithTenant 那条更难发现：分支跑在任何 HTTP 请求之外，没有中间件，
// 租户全靠 gid 解出来放进 ctx——漏设的形状在这里是最自然的。
func TestSagaBranchRefusesMissingTenant(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	gid := barrierGID(t, f.merchantA)

	called := false
	d, err := r.WithSagaBranch(ctx, gid, "01", "action", func(q repository.Tx) error {
		called = true
		return nil
	})
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("期望 tenant.ErrNoTenant，实得 %v（判定 %v）", err, d)
	}
	if called {
		t.Fatal("没有租户上下文时业务仍被执行")
	}
	if n := barrierRows(t, gid); n != 0 {
		t.Fatalf("没有租户上下文时屏障仍写了 %d 行 —— 屏障 INSERT 跑在了 set_config 之前"+
			"（或者跑在了另一条连接上）", n)
	}
}

// 屏障事务里的业务写入照样受 RLS 管：扣别家的 SKU 是 ErrSKUNotInTenant，
// 不是缺货，而且屏障那行也跟着回滚。
//
// 这条的区分力在于商家 B 的水位是够的（10 ≥ 1），所以这次失败只可能来自 RLS。
// 它同时证明 WithSagaBranch 真的设了 app.merchant_id —— 没设的话
// current_merchant() 抛的是 42501，落不进 ErrSKUNotInTenant 这一支。
func TestSagaBranchStillRunsUnderRLS(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	if got := availableQty(t, f.skuB); got < 1 {
		t.Fatalf("夹具坏了：商家 B 的水位是 %d，这次失败就分不清是缺货还是不可见", got)
	}

	_, err := r.WithSagaBranch(asA, gid, "01", "action", func(q repository.Tx) error {
		_, e := q.DeductInventory(ctx, f.skuB, 1)
		return e
	})
	if !errors.Is(err, repository.ErrSKUNotInTenant) {
		t.Fatalf("期望 ErrSKUNotInTenant，实得 %v", err)
	}
	if got := availableQty(t, f.skuB); got != 10 {
		t.Fatalf("商家 B 的水位变成了 %d —— 跨租户扣减真的写进去了", got)
	}
	if n := barrierRows(t, gid); n != 0 {
		t.Fatalf("失败的分支在屏障表里留下了 %d 行 —— 重试会被判成重复", n)
	}
}

// 并发投递同一个分支：**恰好一次** Execute。
//
// 屏障的幂等靠的是唯一约束加 `ON CONFLICT DO NOTHING`，而不是「先查一下在不在」。
// 后者是最顺手的写法，它在串行测试里与前者完全无法区分——两个并发事务会同时
// 查到「不在」，然后双双执行业务。这条测试是那两种写法唯一分得开的地方。
//
// （顺带：先 SELECT 再 INSERT 在这里还会当场 42501，keel_app 在 barrier 上
// 没有 SELECT 权。两道闸都指向同一个结论。）
func TestSagaBranchIsIdempotentUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	gid := barrierGID(t, f.merchantA)

	const n = 16
	var mu sync.Mutex
	counts := map[string]int{}
	var errs []error

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := r.WithSagaBranch(asA, gid, "01", "action", func(q repository.Tx) error {
				_, e := q.DeductInventory(ctx, f.skuA, 1)
				return e
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			counts[d.String()]++
		}()
	}
	wg.Wait()

	for _, err := range errs {
		failIfBarrierDenied(t, err)
	}
	if len(errs) != 0 {
		t.Fatalf("%d/%d 次并发投递失败，第一个错误: %v", len(errs), n, errs[0])
	}
	if counts["Execute"] != 1 {
		t.Fatalf("%d 次并发投递里有 %d 次 Execute，期望恰好 1（判定分布 %v）",
			n, counts["Execute"], counts)
	}
	if got := availableQty(t, f.skuA); got != 9 {
		t.Fatalf("水位是 %d，期望 9 —— 并发下业务被执行了不止一次", got)
	}
}
