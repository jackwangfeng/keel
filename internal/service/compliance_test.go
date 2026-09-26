package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/understanding"
)

// 合规检查那道闸门的行为测试（商品理解服务设计 §2 / §7）。
//
// ===========================================================================
// 这一组要守的是全系统唯一一条「宁可误拒」的规矩
// ===========================================================================
//
// §7 的降级表里只有合规检查这一行是「保守拒绝」，docs/ai-capabilities.md 的
// 「三条纪律」第三条又重复了一遍。落到可测的断言上是两句话，而它们其实是
// 同一句：
//
//	① 检查**卡住**（超时）时，发布被**拒**，不是放行；
//	② 检查**答不出来**（引擎不可用 / 检查器没装上）时，同样是拒，不是放行。
//
// **这两条最容易假绿**，而且假绿的方向是危险的那一边：一个「检查出错就跳过」
// 的实现会让这一组里除了它们之外的每一条测试都照常通过 —— 违禁词照样拦得住
// （那条路走的是正常返回），干净文案照样发得出去，只有引擎坏掉的那一刻
// 闸门无声地消失。所以下面每一条都**同时**断言两件事：拿到的是
// ErrComplianceUnavailable，**以及那次写没有落库**。少了后半句，
// 一个「报错但照样提交」的实现也是绿的。
//
// ===========================================================================
// 为什么这一组不碰数据库
// ===========================================================================
//
// 要造出来的三种情形 ——「检查卡住」「引擎答不出来」「检查器是 nil」——
// 没有一种和 SQL 有关，而真实的检查器是纯计算，快得没法超时
// （understanding.ComplianceCheck 扫三段文本）。用假仓储 + 假检查器，
// 这一组在任何机器上都跑得动，而且**每一条失败都只可能是这道闸门的问题**。
//
// 事务语义由 fakeCatalogRepo 兑现：fn 返回错误 → 不提交（RolledBack）。
// 那正是 repository.WithTenant 的形状（begin / defer rollback / commit），
// 所以「命中就让事务整个回滚」这件事在这里是可断言的。

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// fakeCatalogRepo 是一个只认识事务边界的假仓储。
//
// 它不模拟 SQL，只模拟一件事：**fn 返回错误时那次写不算数**。
// Committed 为 false 就是「回滚了」。
type fakeCatalogRepo struct {
	tx        *fakeCatalogTx
	Committed bool
}

func (r *fakeCatalogRepo) WithTenant(ctx context.Context, fn func(repository.Tx) error) error {
	if err := fn(r.tx); err != nil {
		r.Committed = false // 回滚：那次 UPDATE 没有落库
		return err
	}
	r.Committed = true
	return nil
}

// fakeCatalogTx 只实现这一组用得到的两个方法。
//
// 内嵌 repository.Tx（值是 nil）让它满足那个大接口；别的方法一旦被调到
// 会当场 panic，而那正是我们想要的 —— 这一组测试如果走到了别的查询上，
// 说明被测的路径和以为的不是同一条。
type fakeCatalogTx struct {
	repository.Tx
	product repository.AdminProduct
	// Wrote 记下那次写**被调用过**。它与 repo.Committed 是两件事：
	// 写调用发生了，但事务可能随后回滚 —— 这一组要断言的正是这个组合。
	Wrote bool
}

func (t *fakeCatalogTx) SetProductPublication(ctx context.Context, id int64, publish bool) (repository.AdminProduct, error) {
	t.Wrote = true
	p := t.product
	if publish {
		p.Status = 1
	} else {
		p.Status = 2
	}
	return p, nil
}

func (t *fakeCatalogTx) UpdateProduct(ctx context.Context, id int64, patch repository.ProductPatch) (repository.AdminProduct, error) {
	t.Wrote = true
	p := t.product
	if patch.Title != nil {
		p.Title = *patch.Title
	}
	return p, nil
}

// stalledChecker 是一个永远算不完的检查器：它一直等到 ctx 被取消。
//
// 这就是「检查卡住」在测试里的样子。真实的检查器没法卡住（纯计算），
// 而 §7 那条规矩说的恰恰是它卡住时该怎么办 —— 不造一个假的，
// 那条规矩就没有任何东西盯着。
type stalledChecker struct{}

func (stalledChecker) Check(ctx context.Context, in understanding.ProductInput) ([]understanding.Violation, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// brokenChecker 是「引擎不可用」：立刻返回一个错误，一条命中都给不出。
//
// 它与 stalledChecker 是同一条规矩的两个面，但故障形态不同：一个是超时，
// 一个是当场失败。分成两条测试，是因为一个「只处理 ctx.Err() 」的实现
// 能通过前者而放行后者。
type brokenChecker struct{ err error }

func (c brokenChecker) Check(ctx context.Context, in understanding.ProductInput) ([]understanding.Violation, error) {
	return nil, c.err
}

// newCatalogFixture 造一个挂着假仓储的服务，外加一个带后台身份的 ctx。
//
// title 是那件商品当前的标题 —— 合规检查读的就是它。
func newCatalogFixture(t *testing.T, title string) (*service.AdminCatalogService, *fakeCatalogRepo, context.Context) {
	t.Helper()
	repo := &fakeCatalogRepo{tx: &fakeCatalogTx{
		product: repository.AdminProduct{ID: 7, Title: title, Status: 0},
	}}
	svc := service.NewAdminCatalogService(repo, nil)
	return svc, repo, staffContext()
}

// staffContext 造一个带后台身份的 ctx。
//
// 这 16 条接口的第一道是 requireStaff：没有它，下面每一条都会在碰到
// 合规检查之前就以「未认证」失败，而那会让这一组**全都假绿** ——
// 它们断言的是「发布被拒」，而「因为没登录被拒」也满足那句话。
// 所以身份必须是真的，被拒的理由才有意义。
func staffContext() context.Context {
	merchant := int64(1)
	return auth.NewStaffContext(context.Background(), auth.StaffIdentity{
		StaffID:    1,
		SessionID:  1,
		MerchantID: &merchant,
		Role:       auth.StaffRoleAdmin,
		Status:     auth.StaffStatusActive,
	})
}

// ---------------------------------------------------------------------------
// 一、超时 = 拒绝。§7 里全系统唯一一处宁可误拒
// ---------------------------------------------------------------------------

// 检查卡住时，上架被**拒**，而且那次写没有落库。
//
// 把 checkCompliance 里那句 `return fmt.Errorf("%w: %v", ErrComplianceUnavailable, err)`
// 改成 `return nil`（也就是「超时就放行」），这条测试是唯一会红的那一条。
func TestPublishIsRejectedWhenTheComplianceCheckStalls(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "一个完全干净的标题")
	svc.Compliance = stalledChecker{}
	svc.ComplianceBudget = 30 * time.Millisecond

	_, err := svc.SetPublication(ctx, 7, true)

	if err == nil {
		t.Fatalf("合规检查卡住时上架**成功**了 —— §7 的降级表里这一行是「保守拒绝」，" +
			"是全系统唯一一处宁可误拒的地方（docs/ai-capabilities.md 三条纪律第三条）。" +
			"检查答不出来时放行，等于这道闸门在引擎出问题的那一刻静默消失")
	}
	if !errors.Is(err, service.ErrComplianceUnavailable) {
		t.Fatalf("上架被拒了，但理由是 %v，不是 ErrComplianceUnavailable —— "+
			"handler 按这个 sentinel 翻 503 + Retry-After，翻不出来的话"+
			"商家看到的会是 500，而 500 的意思是「我们写错了代码」，不是「稍后再试」", err)
	}
	if repo.Committed {
		t.Fatalf("上架被拒了，但事务**提交了** —— 商品已经上架，只是商家收到一个错误。" +
			"这比直接放行更坏：库里是上架状态，而没有任何人知道它没过检查")
	}
	if !repo.tx.Wrote {
		t.Fatalf("那次 SetProductPublication 根本没被调用 —— " +
			"这条测试于是没有在测「写了再查、命中就回滚」，它测的是别的东西")
	}
}

// 预算真的被施加了：卡住的检查不会把请求吊死到调用方自己的 ctx 超时。
//
// 这一条盯的是 checkCompliance 里那个 context.WithTimeout。把它去掉
// （直接把 ctx 传给检查器）之后上面那条测试**照样绿** —— 因为
// stalledChecker 等的是 ctx.Done()，而测试的 ctx 最终也会被 go test 取消。
// 区别只在时间上，所以这里用时间断言。
func TestTheComplianceBudgetIsActuallyEnforced(t *testing.T) {
	svc, _, ctx := newCatalogFixture(t, "一个完全干净的标题")
	svc.Compliance = stalledChecker{}
	svc.ComplianceBudget = 50 * time.Millisecond

	start := time.Now()
	_, err := svc.SetPublication(ctx, 7, true)
	elapsed := time.Since(start)

	if !errors.Is(err, service.ErrComplianceUnavailable) {
		t.Fatalf("期望 ErrComplianceUnavailable，拿到 %v", err)
	}
	// 上界给得宽（预算的 20 倍），因为这里要抓的不是「快不快」，
	// 而是「有没有预算」—— 没有预算时这个调用会一直等到调用方的 ctx 结束，
	// 也就是整个测试超时，而不是 50 ms 之后返回。
	if elapsed > time.Second {
		t.Fatalf("合规检查卡住之后过了 %v 才返回，而预算是 %v —— "+
			"checkCompliance 没有用 context.WithTimeout 把预算施加下去，"+
			"于是「200 ms 预算」只是注释里的一个数字，"+
			"真卡住时商家那一端是一次挂死的请求", elapsed, svc.ComplianceBudget)
	}
}

// ---------------------------------------------------------------------------
// 二、引擎不可用 ≠ 放行。和上一条是同一条规矩
// ---------------------------------------------------------------------------

// 检查器当场失败（引擎不可用）时，上架同样被拒。
//
// 与上一条分开，是因为一个只认 ctx 超时的实现
// （`if errors.Is(err, context.DeadlineExceeded) { reject }`）能通过上一条
// 而在这里放行 —— 而「引擎连不上」比「引擎慢」常见得多。
func TestPublishIsRejectedWhenTheEngineIsUnavailable(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "一个完全干净的标题")
	engineDown := errors.New("connect 127.0.0.1:18081: connection refused")
	svc.Compliance = brokenChecker{err: engineDown}

	_, err := svc.SetPublication(ctx, 7, true)

	if err == nil {
		t.Fatalf("检查器返回错误（%v）时上架**成功**了 —— "+
			"合规检查不可用不是一张放行证。§7：这一处宁可误拒", engineDown)
	}
	if !errors.Is(err, service.ErrComplianceUnavailable) {
		t.Fatalf("期望 ErrComplianceUnavailable（handler 据此回 503 + Retry-After），拿到 %v", err)
	}
	if repo.Committed {
		t.Fatalf("检查器失败，但上架那次写提交了 —— 商品已经对买家可见，且没过检查")
	}
}

// 检查器**根本没装上**（nil）时也是拒绝，不是跳过。
//
// 这一条最容易被写反：一个 `if s.Compliance == nil { return nil }` 看上去
// 非常合理（「没配就不查」），而它的效果是任何一次装配失误
// —— 比如有人绕开 NewAdminCatalogService 自己构造一个 AdminCatalogService ——
// 都变成一条静默放行的路。fail-closed 的方向在这里是硬的。
func TestPublishIsRejectedWhenNoCheckerIsConfigured(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "一个完全干净的标题")
	svc.Compliance = nil // 装配失误的样子

	_, err := svc.SetPublication(ctx, 7, true)

	if !errors.Is(err, service.ErrComplianceUnavailable) {
		t.Fatalf("没有配置合规检查器时上架的结果是 %v，期望 ErrComplianceUnavailable —— "+
			"「没配就跳过」把一次装配失误变成了一道无声消失的闸门。"+
			"把这个字段置空只该让商品发不出去，不该让违规文案发得出去", err)
	}
	if repo.Committed {
		t.Fatalf("没有检查器，却把商品上架了")
	}
}

// ---------------------------------------------------------------------------
// 三、上面三条不是靠「什么都拒绝」通过的
// ---------------------------------------------------------------------------
//
// 这一节是前三条的阳性对照。没有它，一个 `SetPublication 永远返回错误`
// 的实现会让前三条全绿 —— 而那显然不是我们要的东西。

// 干净文案 + 正常检查器 = 上架成功，事务提交。
func TestACleanProductPublishesAndCommits(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "手冲咖啡壶 600ml 耐热玻璃")

	p, err := svc.SetPublication(ctx, 7, true)
	if err != nil {
		t.Fatalf("干净文案的商品上架失败了: %v —— 这道闸门在误拒", err)
	}
	if !repo.Committed {
		t.Fatalf("上架成功返回了，但事务没提交")
	}
	if p.Status != 1 {
		t.Fatalf("上架之后 status = %d，期望 1", p.Status)
	}
}

// **下架不查**。下架只会让文案从买家面前消失，拦它没有任何意义。
//
// 用卡住的检查器来证明这一点：如果 unpublish 也走检查，这里会以
// ErrComplianceUnavailable 失败 —— 也就是「商家想把违规商品下架，
// 却因为合规检查不可用而下不掉」，一个恰好把事情变得更糟的行为。
func TestUnpublishIsNotBlockedByAStalledCheck(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "全网第一的最佳好物") // 文案本身是违规的
	svc.Compliance = stalledChecker{}
	svc.ComplianceBudget = 30 * time.Millisecond

	if _, err := svc.SetPublication(ctx, 7, false); err != nil {
		t.Fatalf("下架失败了: %v —— 下架不该过合规检查。"+
			"拦住下架的后果是：一件违规商品在检查器坏掉期间**下不掉**，"+
			"而那正好是最需要把它撤下来的时候", err)
	}
	if !repo.Committed {
		t.Fatalf("下架没有提交")
	}
}

// ---------------------------------------------------------------------------
// 四、命中时，响应带得走「哪个字段第几个字」
// ---------------------------------------------------------------------------

// 拒绝不是一个 sentinel：它带着每一处命中的 field / offset / length。
//
// 设计 §2 的原话是「命中违禁词时直接拒绝发布并返回具体位置」——
// 「拒绝」两个字不够，商家得知道改哪里。把 ComplianceRejection 换成一个
// sentinel error 之后，别的测试都还是绿的（发布照样被拒），
// 只有这一条会红。
func TestPublishRejectionCarriesEveryViolationPosition(t *testing.T) {
	// 「最佳」在第 3 个码点（下标 2）。
	svc, repo, ctx := newCatalogFixture(t, "本店最佳咖啡壶")

	_, err := svc.SetPublication(ctx, 7, true)

	var rejected *service.ComplianceRejection
	if !errors.As(err, &rejected) {
		t.Fatalf("标题里有「最佳」，上架的结果是 %v，期望一个 *ComplianceRejection", err)
	}
	if repo.Committed {
		t.Fatalf("命中违禁词，但事务提交了 —— 商品已经带着违禁标题上架了")
	}
	if len(rejected.Violations) != 1 {
		t.Fatalf("命中 %d 处，期望 1 处: %+v", len(rejected.Violations), rejected.Violations)
	}
	v := rejected.Violations[0]
	if v.Field != "title" {
		t.Errorf("命中字段是 %q，期望 \"title\" —— "+
			"客户端要靠它决定高亮哪个输入框", v.Field)
	}
	if v.Offset != 2 || v.Length != 2 {
		t.Errorf("「最佳」在「本店最佳咖啡壶」里的位置报成 offset=%d length=%d，"+
			"期望 offset=2 length=2（Unicode 码点，从 0 开始）—— "+
			"位置错了比没有位置更坏：客户端会高亮到一段没有问题的文字上",
			v.Offset, v.Length)
	}
	if v.Term != "最佳" {
		t.Errorf("命中的词报成 %q，期望「最佳」", v.Term)
	}
}

// ---------------------------------------------------------------------------
// 五、在架商品改文案也要查，草稿不查
// ---------------------------------------------------------------------------
//
// 这是 compliance.go 文件头第一节那个「挂在哪几条路上」的决定，
// 它有两个方向，各配一条测试。

// 在架商品被 PATCH 成违禁文案时，改动被拒且没有落库。
//
// 这一条堵的是一条完整的绕过路径：先用干净标题上架（过检查），
// 再 PATCH 成违禁标题（不查）—— 两步都 200，而买家看到的是违禁文案。
// 去掉 UpdateProduct 里那个 `if updated.Status == productStatusPublished`
// 分支，只有这一条会红。
func TestPatchingAPublishedProductIntoAViolationIsRejected(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "手冲咖啡壶")
	repo.tx.product.Status = 1 // 已经在架

	bad := "全网第一的咖啡壶"
	_, err := svc.UpdateProduct(ctx, 7, repository.ProductPatch{Title: &bad})

	var rejected *service.ComplianceRejection
	if !errors.As(err, &rejected) {
		t.Fatalf("把**在架**商品的标题改成「%s」，结果是 %v，期望被合规检查拒绝 —— "+
			"只拦上架的话，「先发干净标题上架，再 PATCH 成违禁标题」是一条"+
			"两步都返回 200 的完整绕过路径。要守的不变量是"+
			"「对外可见的文案没有违禁词」，而在架时的 PATCH 是改变它的第二个入口", bad, err)
	}
	if repo.Committed {
		t.Fatalf("改动被拒了，但事务提交了 —— 违禁标题已经落库并对买家可见")
	}
}

// 草稿改成违禁文案**不拦**。
//
// §2：「商品发布不能被 AI 阻塞」。草稿一个买家也看不到，在商家写到一半时
// 用 422 顶回去，是在写作过程中打断人，而法律风险的载体是「发布」这个动作。
//
// 它同时是上一条的阳性对照：一个「PATCH 一律检查」的实现会让上一条绿、
// 这一条红。
func TestPatchingADraftIntoAViolationIsAllowed(t *testing.T) {
	svc, repo, ctx := newCatalogFixture(t, "手冲咖啡壶")
	repo.tx.product.Status = 0 // 草稿

	bad := "全网第一的咖啡壶"
	if _, err := svc.UpdateProduct(ctx, 7, repository.ProductPatch{Title: &bad}); err != nil {
		t.Fatalf("把**草稿**的标题改成「%s」被拒了: %v —— "+
			"草稿一个买家也看不到，拦它只是在商家写到一半时打断他。"+
			"§2：商品发布不能被 AI 阻塞", bad, err)
	}
	if !repo.Committed {
		t.Fatalf("草稿的改动没有提交")
	}
}
