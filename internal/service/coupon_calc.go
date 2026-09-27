package service

import (
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 券怎么算。**全仓库只有这一份实现**（数据模型 §7「券怎么算」）。
//
// 试算（POST /orders/preview）、下单（POST /orders）、「本单可用券」
// （POST /coupons/applicable 与试算里的 applicable_coupons）三处都调 evaluateCoupon，
// 而且喂给它的是同一份输入：priceOrder 按**下单那家门店的生效价**
// （sku_prices_by_store）算出来的每一行。试算说能减多少，下单就减多少 ——
// 不是因为两边约定好了，而是根本没有第二份代码可以跑偏。
//
// 它是一个纯函数：不碰数据库、不看时钟（now 由调用方给），于是四种券型的边界、
// 范围的每一条规则都能在没有数据库的单元测试里逐分核对（coupon_calc_test.go）。

// 券型（数据模型 §7 coupon_templates.coupon_type）。
const (
	couponTypeFullReduction int16 = 1 // 满减
	couponTypeRateDiscount  int16 = 2 // 折扣
	couponTypeInstant       int16 = 3 // 立减
	couponTypeFreeShipping  int16 = 4 // 包邮：抵运费（00056），见 evaluateCoupon 与 pricing.go
)

// couponLine 是参与券计算的一行：金额与挑行的素材。
type couponLine struct {
	ProductID    int64
	BrandID      *int64
	CategoryPath *string
	AmountCents  int64
}

// couponVerdict 是一张券用在一单上的结论。
type couponVerdict struct {
	// Applicable 为假时 Reason 说明为什么，其余字段无意义。
	Applicable bool
	Reason     string

	// EligibleSubtotal 是适用范围内那几行的金额之和，门槛比的就是它。
	EligibleSubtotal int64
	// DiscountCents 是这张券在本单上的减免，已经按适用小计封顶。
	DiscountCents int64
	// LineDiscounts 与输入的 lines 一一对应：每一行分摊到的减免，求和恒等于 DiscountCents。
	LineDiscounts []int64

	// FreeShipping 为真表示这是一张包邮券，而且范围、门槛都满足：它不减商品的钱
	// （DiscountCents 为 0、LineDiscounts 全 0），能抵多少运费要等运费算出来之后
	// 由 freeShippingDeduction 定（计价顺序：券 → 运费 → 包邮券抵运费）。
	// 运费为 0 时调用方仍要把它当成「本单不可用」。
	FreeShipping bool
}

func notApplicable(format string, args ...any) couponVerdict {
	return couponVerdict{Reason: fmt.Sprintf(format, args...)}
}

// evaluateCoupon 判一张券在这一单上能不能用、能减多少、怎么分摊到行。
func evaluateCoupon(c repository.UserCoupon, scopes []repository.CouponScope,
	store repository.StoreScope, lines []couponLine, now time.Time) couponVerdict {

	// ---- 券本身的状态与有效期 ----
	switch c.Status {
	case repository.UserCouponUnused:
	case repository.UserCouponLocked:
		return notApplicable("这张券正被另一笔待支付订单占用")
	case repository.UserCouponUsed:
		return notApplicable("这张券已经用过了")
	default:
		return notApplicable("这张券已过期")
	}
	if now.Before(c.ValidStartAt) {
		return notApplicable("这张券 %s 起才可用", buyerClock(c.ValidStartAt))
	}
	if !now.Before(c.ValidEndAt) {
		return notApplicable("这张券已过期")
	}

	// ---- 门店维度：在哪家店下单可用 ----
	if !storeAllowed(scopes, store) {
		return notApplicable("这张券不能在这家门店使用")
	}

	// ---- 商品维度：哪几行参与计算 ----
	eligible := make([]bool, len(lines))
	var subtotal int64
	for i, ln := range lines {
		if lineEligible(scopes, ln) {
			eligible[i] = true
			subtotal += ln.AmountCents
		}
	}
	if subtotal <= 0 {
		return notApplicable("本单没有适用这张券的商品")
	}

	// ---- 券面减免 ----
	r := c.Rule
	var off int64
	switch r.CouponType {
	case couponTypeFullReduction:
		if subtotal < r.ThresholdCents {
			return notApplicable("适用商品小计 ¥%s，还差 ¥%s（满 ¥%s 可用）",
				yuan(subtotal), yuan(r.ThresholdCents-subtotal), yuan(r.ThresholdCents))
		}
		off = r.DiscountCents
	case couponTypeRateDiscount:
		if subtotal < r.ThresholdCents {
			return notApplicable("适用商品小计 ¥%s，还差 ¥%s（满 ¥%s 可用）",
				yuan(subtotal), yuan(r.ThresholdCents-subtotal), yuan(r.ThresholdCents))
		}
		off = rateDiscount(subtotal, r.DiscountRate)
		if r.MaxDiscountCents > 0 && off > r.MaxDiscountCents {
			off = r.MaxDiscountCents
		}
	case couponTypeInstant:
		off = r.DiscountCents
	case couponTypeFreeShipping:
		// 门槛比的是适用小计，与满减券同一个口径；抵多少运费不在这里定（见 FreeShipping）。
		if subtotal < r.ThresholdCents {
			return notApplicable("适用商品小计 ¥%s，还差 ¥%s（满 ¥%s 可用）",
				yuan(subtotal), yuan(r.ThresholdCents-subtotal), yuan(r.ThresholdCents))
		}
		return couponVerdict{
			Applicable:       true,
			EligibleSubtotal: subtotal,
			LineDiscounts:    make([]int64, len(lines)),
			FreeShipping:     true,
		}
	default:
		return notApplicable("券型 %d 无法识别", r.CouponType)
	}

	// 减免不超过适用商品的小计：应付因此不可能为负（数据模型 §7 第 3 步）。
	if off > subtotal {
		off = subtotal
	}
	if off <= 0 {
		// 折扣券在极小金额上会算出 0（向下取整）。一张「用了但一分没减」的券
		// 被锁定、被核销，买家会觉得券被吞了 —— 当作这一单用不了。
		return notApplicable("这张券在本单上减不了钱")
	}

	return couponVerdict{
		Applicable:       true,
		EligibleSubtotal: subtotal,
		DiscountCents:    off,
		LineDiscounts:    allocateDiscount(lines, eligible, subtotal, off),
	}
}

// rateDiscount 是折扣券的减免：小计 × (1000 − 千分比) / 1000，**向下取整到分**。
//
// 方向定为向下（对商家有利，每单至多少减不到 1 分），理由写在数据模型 §7：
// 向上取整会让「8.5 折」在某些金额上比 8.5 折更便宜，那是资损方向的误差，
// 而且每一单都朝同一个方向偏。
//
// 不在 int64 里赌乘法不溢出：拆成 q·off + ⌊r·off/1000⌋（subtotal = 1000q + r），
// 两项都小于 subtotal，结果与精确的 ⌊subtotal·off/1000⌋ 逐分相等。
func rateDiscount(subtotal int64, rate int16) int64 {
	if rate <= 0 || rate >= 1000 || subtotal <= 0 {
		return 0
	}
	off := int64(1000 - int64(rate))
	return subtotal/1000*off + (subtotal%1000)*off/1000
}

// storeAllowed 判门店维度（5 大区、6 门店）。
// 规则：没被任何排除命中，且（没有门店维度的包含规则，或至少命中一条）。排除优先。
func storeAllowed(scopes []repository.CouponScope, store repository.StoreScope) bool {
	hasInclude, included := false, false
	for _, s := range scopes {
		if s.ScopeType != repository.ScopeRegion && s.ScopeType != repository.ScopeStore {
			continue
		}
		hit := s.TargetID != nil &&
			((s.ScopeType == repository.ScopeRegion && *s.TargetID == store.RegionID) ||
				(s.ScopeType == repository.ScopeStore && *s.TargetID == store.StoreID))
		if !s.Include {
			if hit {
				return false
			}
			continue
		}
		hasInclude = true
		if hit {
			included = true
		}
	}
	return !hasInclude || included
}

// lineEligible 判商品维度（1 全场、2 分类含子孙、3 商品、4 品牌）。
// 规则同门店维度：排除优先；没有商品维度的包含规则即全场。
func lineEligible(scopes []repository.CouponScope, ln couponLine) bool {
	hasInclude, included := false, false
	for _, s := range scopes {
		if s.ScopeType == repository.ScopeRegion || s.ScopeType == repository.ScopeStore {
			continue
		}
		hit := goodsHit(s, ln)
		if !s.Include {
			if hit {
				return false
			}
			continue
		}
		hasInclude = true
		if hit {
			included = true
		}
	}
	return !hasInclude || included
}

func goodsHit(s repository.CouponScope, ln couponLine) bool {
	switch s.ScopeType {
	case repository.ScopeAll:
		return true
	case repository.ScopeCategory:
		// 含子孙：path 形如 /1/23/456/，含每一级祖先的 id，所以一个分类的 path
		// 恰好是它整棵子树的公共前缀（db/queries/products.sql 文件头）。
		// 任一边为 nil（分类已软删）即命中不了。
		return s.CategoryPath != nil && ln.CategoryPath != nil &&
			strings.HasPrefix(*ln.CategoryPath, *s.CategoryPath)
	case repository.ScopeProduct:
		return s.TargetID != nil && *s.TargetID == ln.ProductID
	case repository.ScopeBrand:
		return s.TargetID != nil && ln.BrandID != nil && *s.TargetID == *ln.BrandID
	default:
		return false
	}
}

// allocateDiscount 把减免按金额比例分摊到参与的行（数据模型 §7「优惠分摊」）。
//
//	每行 = ⌊减免 × 行金额 / 适用小计⌋，余数给金额最大的行，放不下顺延给次大的行。
//
// 「放不下」是真的会发生的：34 / 33 / 33 分三行减 99 分，按比例得 33 / 32 / 32，
// 余 2 分；全给最大行就是 35 分，比那一行的金额还多，chk_item_refund 会让它
// 将来一分都退不了。余数总能放下：各行剩余空间之和 = 小计 − 已分摊 ≥ 减免 − 已分摊。
func allocateDiscount(lines []couponLine, eligible []bool, subtotal, off int64) []int64 {
	out := make([]int64, len(lines))
	var given int64
	idx := make([]int, 0, len(lines))
	for i, ln := range lines {
		if !eligible[i] {
			continue
		}
		idx = append(idx, i)
		// off ≤ subtotal、amount ≤ subtotal，所以商 ≤ amount < 2^63，
		// 高位一定小于除数，bits.Div64 不会溢出 panic。
		hi, lo := bits.Mul64(uint64(off), uint64(ln.AmountCents))
		q, _ := bits.Div64(hi, lo, uint64(subtotal))
		out[i] = int64(q)
		given += out[i]
	}
	rest := off - given
	// 金额降序；相同金额按订单行顺序（稳定排序），让「余数给谁」是确定的。
	sort.SliceStable(idx, func(a, b int) bool {
		return lines[idx[a]].AmountCents > lines[idx[b]].AmountCents
	})
	for _, i := range idx {
		if rest == 0 {
			break
		}
		room := lines[i].AmountCents - out[i]
		if room > rest {
			room = rest
		}
		out[i] += room
		rest -= room
	}
	return out
}

// buyerClock 把一个时刻写成买家看得懂的「2026-10-01 00:00」。
//
// 这些文字会原样出现在买家端（Problem.detail），之前写的是 UTC 的 RFC3339，
// 北京时间的买家看到的是早 8 小时的钟点 —— 与买家端时间那个 bug 同一类。
// 这里是纯函数、拿不到这家店的时区设置，按默认店铺时区（Asia/Shanghai）写；
// 改过时区的店会有偏差，但不会再是 UTC。
func buyerClock(t time.Time) string {
	_, loc := reportLocation("")
	return t.In(loc).Format("2006-01-02 15:04")
}
