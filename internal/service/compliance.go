package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/understanding"
)

// 快路径：商品对外可见之前的广告法违禁词检查（商品理解服务设计 §2）。
//
// ===========================================================================
// 一、挂在哪两条路上，以及为什么不是全部四条
// ===========================================================================
//
// 商品的文案能被商家改动的入口有四个：
//
//	POST   /admin/products                      建草稿
//	PATCH  /admin/products/{id}                 改文案
//	POST   /admin/products/{id}/publication     上架 / 下架
//	DELETE /admin/products/{id}                 软删
//
// 检查挂在**两个**上：`publication` 的 publish，以及 PATCH **当商品已经在架时**。
//
// 判据只有一句话：**这段文字会不会出现在买家面前。**
//
//   - 建草稿不查。§2 的原话是「商品发布不能被 AI 阻塞」，而草稿按契约的定义是
//     「从未对外出现过」（publication 端点的描述）。商家写到一半的标题里有
//     「最」字就被 422 顶回来，是在写作过程中打断人，而那段文字一个买家也看不到。
//     法律风险的载体是「发布」这个动作，不是「保存」。
//   - 上架查。它是让文案出现在前台列表与检索结果里的那个动作。
//   - **在架时改文案也查**，而这一条是补漏：只拦上架的话，
//     「先发一个干净的标题上架，再 PATCH 成违禁标题」是一条完整的绕过路径，
//     两步都返回 200。要守的不变量是「对外可见的文案没有违禁词」，
//     而在架状态下的 PATCH 是改变那段可见文案的第二个入口。
//   - 下架与软删不查：它们只会让文案从买家面前消失。
//
// 代价写清楚：存量商品（这一轮之前就在架的）不会被回头检查。触发一次检查需要
// 商家自己再动一次那件商品。做全量回头检查要一条「把在架商品逐件过一遍并下架
// 命中的那些」的运维动作，那是一次会让商家措手不及的批量下架，
// 不该由一次代码发布顺手做掉。
//
// ===========================================================================
// 二、检查在**事务里**跑，命中就让事务回滚
// ===========================================================================
//
// 顺序是：先执行那次写（上架 / 改文案），再在同一个事务里拿回写后的行去检查，
// 命中则返回错误让事务整个回滚。
//
// 为什么不是「先查再写」：PATCH 是部分更新，「改完之后的文案」要把请求体与
// 库里那一行合并才算得出来，而那份合并逻辑已经在 SQL 里了
// （db/queries/admin_products.sql 的 UpdateProduct 用的是 COALESCE）。
// 在这一层再实现一遍合并，是第二份会与 SQL 分叉的真相 —— 而它们分叉的症状是
// 「检查看到的文案与真正落库的文案不是同一段」，也就是这道闸门在某些字段组合下
// 静默失效。拿回写后的行去检查，检查的就是**真正会被买家看到的那一份**。
//
// 代价：一次被拒绝的发布也会占用一次写事务（随后回滚）。发布不是高频操作，
// 这笔代价换到的是「检查对象与落库对象必然是同一个」。
//
// ===========================================================================
// 三、超时 = 拒绝。这是全系统唯一一处宁可误拒的地方
// ===========================================================================
//
// §7 的降级表里只有这一行是「保守拒绝」，docs/ai-capabilities.md 的
// 「三条纪律」第三条又重复了一遍：「AI 故障不得阻塞交易主链路。
// 唯一的例外是第 6 项合规检查——那里宁可误拒。」
//
// 落到代码上是三条，每一条都有测试盯着：
//
//	① 预算 complianceBudget（200 ms，§2 给的数）由这一层用 context 施加；
//	② 检查器返回任何错误（含 ctx 超时）→ ErrComplianceUnavailable → 拒绝；
//	③ **检查器是 nil 也拒绝**。这一条最容易被写反：一个「没配检查器就跳过」
//	   的分支看上去很合理，而它的效果是任何一次装配失误都变成静默放行。
//	   fail-closed 的方向在这里是硬的。
//
// ===========================================================================
// 四、结论**不落库**，而这不是偷懒
// ===========================================================================
//
// 设计 §3 说 product_understanding.results 放「合规结论」。这一轮不写它，
// 理由是一条实测过的连锁反应：那张表上挂着
// touch_product_understanding_updated_at，**任何一次 UPDATE 都会把 updated_at
// 推到 now()**，而那一列是索引触发点的水位线
// （db/queries/semantic.sql 的 ListStaleProductsForIndex 比的正是它）。
//
// 于是「发布时顺手记一条合规结论」会把刚发布的商品的水位线推到现在，
// 而上架这个动作同时也把 products.updated_at 推到现在 ——
// `p.updated_at > pu.updated_at` 不成立，**这件刚上架的商品不会进索引队列**。
// 症状：新品搜不到，而且不报错。
//
// 这与 MarkProductIndexed 上那笔挂账是同一件事的两个面。真要落库，
// 代价是先把「失败 / 结论」与「水位线」拆开（多一列，或者让触发点不看
// pu.updated_at），那是一次独立的动作。
//
// 所以本轮合规结论只回给商家，不进库。后果诚实说一条：后台看不到
// 「这件商品上次因为什么被拒」，商家只在那一次响应里见过它。

// ComplianceChecker 是快路径的检查器。
//
// 抽成接口只为一件事：让「检查卡住」这件事可以被测试造出来
// （understanding.ComplianceCheck 是纯计算，快得没法超时）。
// 生产路径上它永远是 understanding.ComplianceCheck{}。
type ComplianceChecker interface {
	Check(ctx context.Context, in understanding.ProductInput) ([]understanding.Violation, error)
}

// complianceBudget 是 §2 给的那个数：快路径预算 < 200 ms。
//
// 它不是配置项。一个能被调大的预算迟早会被调大 —— 而调大它的场景
// （「合规检查偶尔超时，把预算提到 5 秒就好了」）恰好是应该去修检查器、
// 而不是让商家等 5 秒再被拒绝的那个场景。
const complianceBudget = 200 * time.Millisecond

// complianceRetryAfterSeconds 是 503 上那个 Retry-After。
//
// 取 5 秒：它要比一次瞬时抖动长、比商家的耐心短。这个数字本身不重要，
// 重要的是**有**这个头 —— 没有它，客户端能做的只有立刻重试，
// 而立刻重试会撞上同一个还没恢复的检查器。
const complianceRetryAfterSeconds = 5

// ComplianceRejection 是「文案命中违禁词」这个失败。
//
// 它是一个类型而不是 sentinel，因为**违规位置是响应的一部分**：
// 契约的 Problem.errors 要逐条给出 field / offset / length（FieldError），
// 而一个 sentinel 带不走那些。handler 用 errors.As 把它取出来。
type ComplianceRejection struct {
	Violations []understanding.Violation
}

func (e *ComplianceRejection) Error() string {
	terms := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		terms = append(terms, v.Term)
	}
	return fmt.Sprintf("商品文案命中 %d 处广告法违禁词：%s",
		len(e.Violations), strings.Join(terms, "、"))
}

// ErrComplianceUnavailable 是「检查没能在预算内给出结论」。
//
// **它不是一次放行**。handler 把它翻成 503 + Retry-After，商品没有被上架。
// 见文件头第三节：这是全系统唯一一处宁可误拒的地方。
var ErrComplianceUnavailable = errors.New("合规检查没能在预算内给出结论，保守拒绝")

// checkCompliance 扫一件商品的三段文案。
//
// p 是**写之后**从同一个事务里读回来的那一行（文件头第二节）。
func (s *AdminCatalogService) checkCompliance(ctx context.Context, p repository.AdminProduct) error {
	checker := s.Compliance
	if checker == nil {
		// fail-closed。见文件头第三节第 ③ 条：一个「没配就跳过」的分支
		// 会让任何一次装配失误变成静默放行，而那正是这道闸门要防的东西。
		return fmt.Errorf("%w（没有配置合规检查器）", ErrComplianceUnavailable)
	}

	budget := s.ComplianceBudget
	if budget <= 0 {
		budget = complianceBudget
	}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	in := understanding.ProductInput{ProductID: p.ID, Title: p.Title}
	if p.Subtitle != nil {
		in.Subtitle = *p.Subtitle
	}
	if p.Description != nil {
		in.Description = *p.Description
	}

	violations, err := checker.Check(cctx, in)
	if err != nil {
		// 任何错误都是拒绝，包括 ctx 超时。不区分「检查器坏了」与「检查超时」：
		// 对商家来说两者一样（稍后再试），而对我们来说两者的处置也一样
		// （不许放行）。日志里有原因，响应里没有 —— problem.Write 刻意不收
		// detail，因为这条路径上的 err 里常常带着内部细节。
		return fmt.Errorf("%w: %v", ErrComplianceUnavailable, err)
	}
	if len(violations) > 0 {
		return &ComplianceRejection{Violations: violations}
	}
	return nil
}

// ComplianceRetryAfter 是 503 上那个 Retry-After 头的值（秒，十进制串）。
//
// 由这一层给而不是 handler 自己写一个数：这个秒数与
// complianceRetryAfterSeconds 上那段理由是同一件事，两处各写一个数字
// 迟早对不上，而对不上的症状是文档里写 5 秒、响应里回 30 秒。
func ComplianceRetryAfter() string {
	return strconv.Itoa(complianceRetryAfterSeconds)
}
