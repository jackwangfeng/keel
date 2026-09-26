package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 定价。**试算（POST /orders/preview）与真下单（POST /orders）共用这一份。**
//
// 共用不是为了少写几行。试算和真下单算出不同的钱是这条链路最严重的一类 bug：
// 用户看到一个价、付了另一个价，而两条代码路径各自都是自洽的、各自的测试都是
// 绿的。共用同一份实现是唯一能让这件事**不可能**发生的办法 —— 不是「约定两边
// 要保持一致」，而是根本没有第二边可以跑偏。
//
// 这条性质由 order_test.go 的 TestPreviewAndCreateAgreeOnTheMoney 钉住：
// 同一个请求先试算再下单，两次的应付金额必须逐分相等。
//
// **本轮（00020）这条纪律的边界变大了。** 三层定价之后「这件商品多少钱」
// 取决于哪家店，所以 priceOrder 多了一个 StoreScope，而它的取价口从
// skus.price_cents 换成了 sku_prices_by_store 视图 —— 那是全仓库唯一一处写
// COALESCE(门店价, 大区价, 基准价) 的地方，商品列表、详情、检索结果
// 也都经过它。四条读路径与这条写路径共用同一个公式，
// 于是「列表价与下单价分叉」这件事没有发生的余地。
//
// **00026 起券也在这里算**，而且只有一份实现（coupon_calc.go 的 evaluateCoupon）。
// 喂给它的正是这里按门店生效价算出来的每一行：券按「下单那家店的价」算，
// 试算与下单用的是同一个函数、同一份输入，「试算减 20、下单减 19」没有发生的余地。

// 请求规模的上限。
//
// 不是「防滥用」那种泛泛的限流，两个数各自挡住一件具体的事：
//   - maxOrderLines：一笔订单的行数决定了库存分支要拿多少行锁，而那是在
//     SAGA 分支里、在一个事务里发生的。无上限意味着一个请求就能把一张表锁很久。
//   - maxLineQuantity：数量最终会乘进 int64 的金额里。单价上限由 DDL 的
//     chk_price_nonneg 兜着下界，上界没人管；给数量一个上限，
//     「单价 * 数量」就不可能溢出到负数去骗过 chk_amount。
const (
	maxOrderLines   = 50
	maxLineQuantity = 999
)

// freightNotBilledThisRelease 说的是「**本期不计运费**」，不是「算出来是 0」。
//
// 运费模板在数据模型里没有落地（没有这张表，也没有区划到费率的映射），
// 所以这条链路**没有算过运费**。两者的差别对客户端是实打实的：
// 「算出来是 0」意味着这单包邮，「没算」意味着这个数还会变。
//
// 落到响应上的形状是 nil 而不是 0 —— 契约里 freight_cents 是可选字段，
// 没有就整个不出现（同 apiUser 里 last_login_at 的做法）。返回 0 的话，
// 客户端没有任何办法把「包邮」和「还没算」分开。
//
// 落到库里的仍然是 0：orders.freight_cents 是 NOT NULL，而 chk_amount 要求
// payable = goods + freight - discount 恒等。库里的 0 是一个占位，
// 它与响应里的「字段不存在」不矛盾 —— 一个记的是账，一个说的是这笔账算没算过。
//
// 这笔账挂在 contract_test.go 的 NotYetImplementedResponse 里，两个方向都会红。
var freightNotBilledThisRelease *int64 = nil

// freightForLedger 是写进 orders.freight_cents 的值。见上。
const freightForLedger int64 = 0

// LineInput 是请求体里的一行（契约的 OrderItemInput）。
type LineInput struct {
	SKUID    int64
	Quantity int32
}

// PricedLine 是定价之后的一行，同时也是 order_items 快照的素材。
type PricedLine struct {
	SKUID      int64
	ProductID  int64
	Title      string
	SpecValues []byte
	ImageURL   *string
	PriceCents int64
	Quantity   int32

	// AmountCents = PriceCents * Quantity。
	AmountCents int64

	// DiscountCents 是这一行分摊到的优惠（数据模型 §7「优惠分摊」）。
	//
	// 本期唯一的优惠来源是券：没带券时它是算出来的 0（与 freight 不同，它在响应里
	// 就该是 0 而不是缺席）；带券时是 allocateDiscount 按金额比例分下来的那一份，
	// 各行求和恒等于 Quote.DiscountCents。
	DiscountCents int64

	// 券挑行的素材（与价格同一条查询取来）。不进 order_items 快照。
	brandID      *int64
	categoryPath *string
}

// Quote 是一次试算的全部结果。
type Quote struct {
	Lines []PricedLine

	GoodsAmountCents int64

	// FreightCents 是 nil：本期不计运费，见 freightNotBilledThisRelease。
	FreightCents *int64

	DiscountCents int64
	PayableCents  int64

	// UserCouponID 是这次用上的券；没带券为 nil。
	UserCouponID *int64

	// ApplicableCoupons 只有试算才填：这个买家手里本单可用的全部券。
	ApplicableCoupons []ApplicableCoupon
}

// couponRequest 是「这一单带没带券、是谁的券、按哪个时刻判有效期」。
//
// 试算与下单构造它的方式一模一样（order.go 的 couponOf），于是两边判
// 「这张券此刻能不能用」用的是同一个时刻来源。
type couponRequest struct {
	UserID int64
	ID     *int64
	Now    time.Time
}

// ErrCouponNotApplicable：带的券本单用不了。契约的 409 coupon-not-applicable。
//
// 成因有很多（不是你的、已锁定 / 已使用 / 已过期、门槛不够、范围不含这些商品或
// 这家店），合用一个 sentinel、原因写进 detail：客户端的处置只有一种（换一张券或
// 不用券），而原因是给人看的。「不是你的」与「不存在」合在同一句话里，
// 理由同 ErrAddressNotFound —— 分开报就是一个探测别人券包的口子。
//
// **它必须是一个错误，不能是「忽略这张券按原价算」**：用户以为用了券、实际按原价
// 成交是钱的问题，而客户端没有任何办法发现（这是原先 ErrCouponNotImplemented
// 那段注释里的原话，这条纪律在券接上之后原样保留）。
var ErrCouponNotApplicable = errors.New("这张优惠券本单不可用")

// ErrSKUUnavailable：请求里有 SKU 不可售（不存在、下架、所属商品是草稿或已软删、
// 或者干脆属于别的商家 —— RLS 让后者与「不存在」在这一层同形，这是对的：
// 下单接口不该能被用来探测别家的 SKU 是否存在）。
var ErrSKUUnavailable = errors.New("请求里有不可售的 SKU")

// priceOrder 按请求行算出金额明细。**无副作用**：只读 skus / products。
//
// 它刻意不看库存。试算是金额试算，不是可售性承诺 —— 把库存并进来会让试算
// 看起来像一次预留，而 SAGA 的正向阶段才是真正的判定点（架构 §5 论证过：
// 超卖为零、少卖存在，正是因为扣减发生在那里而不是这里）。
func priceOrder(ctx context.Context, tx repository.Tx, sc repository.StoreScope, items []LineInput,
	coupon couponRequest) (Quote, error) {
	if len(items) == 0 {
		return Quote{}, fmt.Errorf("%w: 订单至少要有一行", ErrBadRequest)
	}
	if len(items) > maxOrderLines {
		return Quote{}, fmt.Errorf("%w: 订单行数 %d 超过上限 %d",
			ErrBadRequest, len(items), maxOrderLines)
	}

	seen := make(map[int64]bool, len(items))
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it.SKUID <= 0 {
			return Quote{}, fmt.Errorf("%w: sku_id 必须为正，实得 %d", ErrBadRequest, it.SKUID)
		}
		if it.Quantity <= 0 || it.Quantity > maxLineQuantity {
			return Quote{}, fmt.Errorf("%w: sku %d 的数量 %d 不在 [1, %d] 内",
				ErrBadRequest, it.SKUID, it.Quantity, maxLineQuantity)
		}
		if seen[it.SKUID] {
			// 不合并同一个 SKU 的两行，直接拒。
			//
			// 合并是「友好」的做法，也是会骗人的：客户端本来想要 2 件和 3 件
			// 两行，合并之后订单上是一行 5 件，而它自己的购物车还是两行。
			// 之后任何一次按行退款都对不上。宁可让它显式失败。
			return Quote{}, fmt.Errorf("%w: sku %d 在请求里出现了两次", ErrBadRequest, it.SKUID)
		}
		seen[it.SKUID] = true
		ids = append(ids, it.SKUID)
	}

	rows, err := tx.ListSKUsForPricing(ctx, sc, ids)
	if err != nil {
		return Quote{}, err
	}
	bySKU := make(map[int64]repository.PriceableSKU, len(rows))
	for _, r := range rows {
		bySKU[r.ID] = r
	}

	// 缺哪些要一次说全，而不是撞到第一个就返回：客户端拿到「这 3 个不可售」
	// 才能一次把购物车理干净，拿到「这 1 个不可售」会来回试三遍。
	var missing []int64
	for _, id := range ids {
		if _, ok := bySKU[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
		return Quote{}, fmt.Errorf("%w: %v", ErrSKUUnavailable, missing)
	}

	q := Quote{
		Lines:        make([]PricedLine, 0, len(items)),
		FreightCents: freightNotBilledThisRelease,
	}
	for _, it := range items {
		sku := bySKU[it.SKUID]
		amount := sku.PriceCents * int64(it.Quantity)
		q.Lines = append(q.Lines, PricedLine{
			SKUID:      sku.ID,
			ProductID:  sku.ProductID,
			Title:      sku.Title,
			SpecValues: sku.SpecValues,
			ImageURL:   sku.ImageURL,
			PriceCents: sku.PriceCents,
			Quantity:   it.Quantity,

			AmountCents: amount,
			// 没带券时这个 0 是算出来的；带券时下面 applyCoupon 会写进分摊额。
			DiscountCents: 0,

			brandID:      sku.BrandID,
			categoryPath: sku.CategoryPath,
		})
		q.GoodsAmountCents += amount
	}

	q.DiscountCents = 0
	if coupon.ID != nil {
		if err := applyCoupon(ctx, tx, sc, &q, coupon); err != nil {
			return Quote{}, err
		}
	}

	// 应付 = 商品 + 运费 - 优惠（数据模型 §5 的 chk_amount 就是这条恒等式）。
	// 运费这一项用的是**落账值** freightForLedger，不是响应里那个 nil ——
	// 库里的恒等式必须成立，而「这笔运费算没算过」是另一件事。
	q.PayableCents = q.GoodsAmountCents + freightForLedger - q.DiscountCents
	if q.PayableCents < 0 {
		// evaluateCoupon 已经把减免封顶在适用小计上，这里按构造不可达。但破了之后
		// chk_amount 仍然成立 —— 数据库拦不住一笔负数应付，所以这一道要留着。
		return Quote{}, fmt.Errorf("应付金额算成了 %d，小于 0", q.PayableCents)
	}
	return q, nil
}

// couponLinesOf 把定价结果变成券计算的输入。试算、下单、「本单可用券」共用。
func couponLinesOf(lines []PricedLine) []couponLine {
	out := make([]couponLine, len(lines))
	for i, ln := range lines {
		out[i] = couponLine{
			ProductID:    ln.ProductID,
			BrandID:      ln.brandID,
			CategoryPath: ln.categoryPath,
			AmountCents:  ln.AmountCents,
		}
	}
	return out
}

// applyCoupon 把请求里那张券算进 q：总减免、每行分摊、回显券 id。
func applyCoupon(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	q *Quote, coupon couponRequest) error {
	c, err := tx.FindUserCoupon(ctx, *coupon.ID, coupon.UserID)
	if errors.Is(err, repository.ErrCouponNotFound) {
		return fmt.Errorf("%w: user_coupon_id=%d 不存在或不属于你", ErrCouponNotApplicable, *coupon.ID)
	}
	if err != nil {
		return err
	}
	scopes, err := tx.ListCouponScopes(ctx, []int64{c.Rule.TemplateID})
	if err != nil {
		return err
	}
	v := evaluateCoupon(c, scopes[c.Rule.TemplateID], sc, couponLinesOf(q.Lines), coupon.Now)
	if !v.Applicable {
		return fmt.Errorf("%w: %s", ErrCouponNotApplicable, v.Reason)
	}
	for i := range q.Lines {
		q.Lines[i].DiscountCents = v.LineDiscounts[i]
	}
	q.DiscountCents = v.DiscountCents
	id := c.ID
	q.UserCouponID = &id
	return nil
}

// ApplicableCoupon 是「本单可用券」的一项：券、它的范围、用在本单上能减多少。
type ApplicableCoupon struct {
	Coupon        repository.UserCoupon
	Scopes        []repository.CouponScope
	DiscountCents int64
}

// applicableCoupons 列出这个买家手里本单可用的全部券，按减免降序、过期时间升序。
//
// 逐张走的是 evaluateCoupon —— 与 applyCoupon 同一个函数、同一份行输入，所以这里说
// 「能减 20」，带上这张券试算就是减 20（handler 的测试逐张核对这一条）。
func applicableCoupons(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	userID int64, lines []PricedLine, now time.Time) ([]ApplicableCoupon, error) {
	coupons, err := tx.ListUsableUserCoupons(ctx, userID, now)
	if err != nil {
		return nil, err
	}
	if len(coupons) == 0 {
		return []ApplicableCoupon{}, nil
	}
	tplSeen := map[int64]bool{}
	var tpls []int64
	for _, c := range coupons {
		if !tplSeen[c.Rule.TemplateID] {
			tplSeen[c.Rule.TemplateID] = true
			tpls = append(tpls, c.Rule.TemplateID)
		}
	}
	scopes, err := tx.ListCouponScopes(ctx, tpls)
	if err != nil {
		return nil, err
	}
	in := couponLinesOf(lines)
	out := []ApplicableCoupon{}
	for _, c := range coupons {
		v := evaluateCoupon(c, scopes[c.Rule.TemplateID], sc, in, now)
		if !v.Applicable {
			continue
		}
		out = append(out, ApplicableCoupon{
			Coupon: c, Scopes: scopes[c.Rule.TemplateID], DiscountCents: v.DiscountCents,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].DiscountCents != out[j].DiscountCents {
			return out[i].DiscountCents > out[j].DiscountCents
		}
		return out[i].Coupon.ValidEndAt.Before(out[j].Coupon.ValidEndAt)
	})
	return out, nil
}
