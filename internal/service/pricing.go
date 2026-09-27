package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
//
// **营销活动与运费也在这里算**（promotion_calc.go / freight_calc.go，各只有一份实现）。
// 计价顺序固定为（数据模型 §7「优惠计算顺序」）：
//
//	门店最终价 → 营销活动（限时折扣 / 秒杀改单价，满减满折按行分摊）
//	→ 优惠券（门槛与计算基数是活动后金额）→ 运费（满额包邮按优惠后应付商品金额判）
//	→ 包邮券抵运费
//
// 券按活动后金额算（couponLinesOf 喂的是 amount − 活动分摊）：一件已经打了 8 折的商品，
// 不该再按原价去凑满减券的门槛。运费一段只看 FreightRequest 那四样输入，
// 不关心前面的优惠是怎么减出来的。

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

	// DiscountCents 是这一行分摊到的全部优惠（数据模型 §7「优惠分摊」）：满减满折 + 券。
	//
	// 两个来源各自按 allocateDiscount 分摊（同一份比例与余数规则），各行求和恒等于
	// Quote.DiscountCents。没有任何优惠时它是算出来的 0（与 freight 不同，它在响应里
	// 就该是 0 而不是缺席）。退款按 AmountCents − DiscountCents 这份净额退（§11），
	// 两个来源合在一列里，「一行退完 = 这一行实付」才恒等。
	DiscountCents int64

	// PromotionDiscountCents 是其中满减满折分摊到这一行的那一份（00058）。
	// 券那一份 = DiscountCents − PromotionDiscountCents。
	PromotionDiscountCents int64

	// ListPriceCents 是门店最终价（三层定价的结果）；PriceCents 是成交单价 ——
	// 命中限时折扣 / 秒杀时是活动价（min(门店价, 特价)），否则两者相等。
	ListPriceCents int64
	// PricePromotionID 是改了这一行单价的活动；没有为 nil。库存分支据此扣活动配额。
	PricePromotionID *int64

	// 券挑行的素材（与价格同一条查询取来）。不进 order_items 快照。
	brandID      *int64
	categoryPath *string
}

// Quote 是一次试算的全部结果。
type Quote struct {
	Lines []PricedLine

	GoodsAmountCents int64

	// FreightCents 是运费（包邮券抵扣之前），FreightDiscountCents 是包邮券抵掉的部分
	// （已经算在 DiscountCents 里），Freight 是明细。
	//
	// 没有收货地址（POST /coupons/applicable 没带 address_id）时 Freight 为 nil、
	// FreightCents 为 0：那不是「包邮」，是「没算」—— 那条接口不回任何金额，
	// 只把包邮券从结果里拿掉（判不了它能不能用）。试算与下单一定有地址。
	FreightCents         int64
	FreightDiscountCents int64
	Freight              *FreightBreakdown

	// DiscountCents 是优惠合计 = 各行分摊的商品优惠之和 + FreightDiscountCents。
	DiscountCents int64
	PayableCents  int64

	// PromotionDiscountCents / CouponDiscountCents 是 DiscountCents 的两个来源（00058）。
	PromotionDiscountCents int64
	CouponDiscountCents    int64

	// Promotions 是各活动在这一单上的结果（命中了哪些、各减多少、还差多少凑满）。
	Promotions []PromotionHit

	// couponBlockers 是命中了、且不与券同享的活动名；非空时这一单不能用券。
	couponBlockers []string
	// limitViolations 是超出每人限购的行。试算与下单据此报 409（checkPromotionLimits），
	// 「本单可用券」不看它 —— 那条接口只回答券的问题。
	limitViolations []promoLimitViolation
	// freightNoCoupon 是「不用任何券」时的运费：本单可用券里包邮券能抵多少按它算 ——
	// 带上那张包邮券去试算时，商品上没有券的减免，满额包邮按不用券的金额判。
	// nil = 没有地址，包邮券判不了。
	freightNoCoupon *int64

	// UserCouponID 是这次用上的券；没带券为 nil。
	UserCouponID *int64

	// ApplicableCoupons 只有试算才填：这个买家手里本单可用的全部券。
	ApplicableCoupons []ApplicableCoupon

	// Store 是这次按哪家门店算的。契约把 OrderPreview.store_id 定成必返
	// （「试算与下单必须是同一家店，回显是客户端唯一能核对这件事的办法」），
	// 此前 handler 一直没填，回的是 0。
	Store repository.StoreScope
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

// ErrPromotionLimitExceeded：限时折扣 / 秒杀超出每人限购。契约的 409 promotion-limit-exceeded。
//
// 试算就报、下单再报（库存分支在行锁之下做最终判定，两笔并发订单只有一笔过得去）。
// 不静默按门店价卖超出的那几件：买家看到的是活动价，按另一个价成交是钱的问题。
var ErrPromotionLimitExceeded = errors.New("超出活动每人限购")

// ErrPromotionSoldOut：秒杀配额在试算之后被别人抢光了（库存分支的条件 UPDATE 受影响 0 行）。
// 契约的 409 promotion-sold-out。重新试算会按门店价报价（pickPriceOffer 跳过配额不够的报价）。
var ErrPromotionSoldOut = errors.New("活动配额已售罄")

// priceOrder 按请求行算出金额明细。**无副作用**：只读 skus / products。
//
// 它刻意不看库存。试算是金额试算，不是可售性承诺 —— 把库存并进来会让试算
// 看起来像一次预留，而 SAGA 的正向阶段才是真正的判定点（架构 §5 论证过：
// 超卖为零、少卖存在，正是因为扣减发生在那里而不是这里）。
//
// dest 是收货地址归到的省；nil = 没有地址（只有 POST /coupons/applicable 会这样调），
// 此时不算运费，带包邮券则报券不可用。
func priceOrder(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	dest *FreightDestination, items []LineInput, coupon couponRequest) (Quote, error) {
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
		Lines:      make([]PricedLine, 0, len(items)),
		Store:      sc,
		Promotions: []PromotionHit{},
	}
	promoIn := make([]promoLine, 0, len(items))
	for _, it := range items {
		sku := bySKU[it.SKUID]
		promoIn = append(promoIn, promoLine{
			SKUID: sku.ID, ProductID: sku.ProductID, BrandID: sku.BrandID,
			CategoryPath: sku.CategoryPath, ListPriceCents: sku.PriceCents, Quantity: it.Quantity,
		})
	}

	// ---- 营销活动：单价类改单价，满减满折按行分摊（promotion_calc.go）----
	lp, err := loadLivePromotions(ctx, tx, ids, coupon.Now)
	if err != nil {
		return Quote{}, err
	}
	var bought map[repository.PurchaseKey]int32
	if coupon.UserID != 0 {
		if bought, err = loadPurchases(ctx, tx, coupon.UserID, lp); err != nil {
			return Quote{}, err
		}
	}
	pr := computePromotions(promoIn, lp, sc, bought)

	for i, it := range items {
		sku := bySKU[it.SKUID]
		price := pr.UnitPrices[i]
		amount := price * int64(it.Quantity)
		q.Lines = append(q.Lines, PricedLine{
			SKUID:      sku.ID,
			ProductID:  sku.ProductID,
			Title:      sku.Title,
			SpecValues: sku.SpecValues,
			ImageURL:   lineImageURL(sku.ImageURL, sku.MainImageUploadID),
			PriceCents: price,
			Quantity:   it.Quantity,

			AmountCents: amount,
			// 满减满折的分摊；带券时下面 applyCoupon 再把券那一份加上去。
			DiscountCents:          pr.LineDiscounts[i],
			PromotionDiscountCents: pr.LineDiscounts[i],
			ListPriceCents:         sku.PriceCents,
			PricePromotionID:       pr.PricePromotionIDs[i],

			brandID:      sku.BrandID,
			categoryPath: sku.CategoryPath,
		})
		q.GoodsAmountCents += amount
	}
	q.DiscountCents = pr.DiscountCents
	q.PromotionDiscountCents = pr.DiscountCents
	q.Promotions = pr.Hits
	q.couponBlockers = pr.CouponBlockers
	q.limitViolations = pr.Violations

	// ---- 优惠券：满减 / 折扣 / 立减在这里分摊到行（按活动后金额）；包邮券只判门槛与范围，
	// 抵扣留到运费之后。q.DiscountCents 此刻是活动那一份，券的商品优惠加在它上面。----
	var freeShipping *repository.UserCoupon
	if coupon.ID != nil {
		fs, err := applyCoupon(ctx, tx, sc, &q, coupon)
		if err != nil {
			return Quote{}, err
		}
		freeShipping = fs
	}

	// ---- 运费：满额包邮按「优惠后应付商品金额」判 ----
	if dest != nil {
		fc, err := loadFreightContext(ctx, tx, sc, skuIDsOf(q.Lines))
		if err != nil {
			return Quote{}, err
		}
		fitems := freightItemsOf(q.Lines)
		// goodsPayable = 商品金额（已是活动价）− 满减满折 − 券的商品优惠。
		// 此刻 q.DiscountCents 正是「活动 + 券的商品优惠」（包邮券还没抵，见下）。
		goodsPayable := q.GoodsAmountCents - q.DiscountCents
		b, bad, err := fc.quote(dest.ProvinceCode, fitems, goodsPayable)
		if err != nil {
			return Quote{}, err
		}
		if len(bad) > 0 {
			return Quote{}, &UndeliverableError{Lines: bad}
		}
		q.Freight = &b
		q.FreightCents = b.FreightCents

		// 不用券时的运费（本单可用券里包邮券按它算）：活动优惠照减，只是不减券。
		// 没有券的商品优惠时就是上面这一个。
		noCoupon := b.FreightCents
		if q.DiscountCents > q.PromotionDiscountCents {
			nb, _, err := fc.quote(dest.ProvinceCode, fitems, q.GoodsAmountCents-q.PromotionDiscountCents)
			if err != nil {
				return Quote{}, err
			}
			noCoupon = nb.FreightCents
		}
		q.freightNoCoupon = &noCoupon
	}

	// ---- 包邮券抵运费：最多抵到 0；本单运费为 0 时这张券不可用 ----
	if freeShipping != nil {
		if q.Freight == nil {
			return Quote{}, fmt.Errorf("%w: 包邮券要按收货地址算运费，这次请求没有地址", ErrCouponNotApplicable)
		}
		off := freeShippingDeduction(freeShipping.Rule.MaxDiscountCents, q.FreightCents)
		if off <= 0 {
			return Quote{}, fmt.Errorf("%w: 本单运费为 0（已包邮或不计运费），包邮券抵不了钱", ErrCouponNotApplicable)
		}
		q.FreightDiscountCents = off
		q.Freight.FreightDiscountCents = off
		q.DiscountCents += off
		id := freeShipping.ID
		q.UserCouponID = &id
	}

	// 应付 = 商品 + 运费 - 优惠（数据模型 §5 的 chk_amount 就是这条恒等式）。
	// 优惠里含包邮券抵掉的运费，所以这条式子不用为运费另加一项。
	q.PayableCents = q.GoodsAmountCents + q.FreightCents - q.DiscountCents
	if q.PayableCents < 0 {
		// evaluateCoupon 已经把减免封顶在适用小计上，这里按构造不可达。但破了之后
		// chk_amount 仍然成立 —— 数据库拦不住一笔负数应付，所以这一道要留着。
		return Quote{}, fmt.Errorf("应付金额算成了 %d，小于 0", q.PayableCents)
	}
	return q, nil
}

// checkPromotionLimits 把「超出每人限购」翻成 ErrPromotionLimitExceeded。试算与下单共用。
func (q Quote) checkPromotionLimits() error {
	if len(q.limitViolations) == 0 {
		return nil
	}
	v := q.limitViolations[0]
	return fmt.Errorf("%w: sku %d 每人限购 %d 件，已买 %d 件，这一单要 %d 件",
		ErrPromotionLimitExceeded, v.SKUID, v.Limit, v.Bought, v.Asked)
}

// loadLivePromotions 取「此刻生效」的活动素材。没有任何生效活动时只花一条查询。
//
// 新人礼（类型 5）不参与计价，在这里就滤掉。
func loadLivePromotions(ctx context.Context, tx repository.Tx, skuIDs []int64,
	now time.Time) (livePromotions, error) {
	lp := livePromotions{Promos: map[int64]repository.Promotion{}}
	live, err := tx.ListLivePromotions(ctx, now)
	if err != nil {
		return livePromotions{}, err
	}
	var ids []int64
	for _, p := range live {
		if p.Type == repository.PromoNewBuyerGift {
			continue
		}
		lp.Promos[p.ID] = p
		ids = append(ids, p.ID)
	}
	if len(ids) == 0 {
		return lp, nil
	}
	if lp.Tiers, err = tx.ListPromotionTiers(ctx, ids); err != nil {
		return livePromotions{}, err
	}
	if lp.Scopes, err = tx.ListPromotionScopes(ctx, ids); err != nil {
		return livePromotions{}, err
	}
	lp.Offers = map[int64][]repository.PriceOffer{}
	if len(skuIDs) > 0 {
		offers, err := tx.ListLivePriceOffers(ctx, skuIDs, now)
		if err != nil {
			return livePromotions{}, err
		}
		for _, o := range offers {
			if _, ok := lp.Promos[o.PromotionID]; ok {
				lp.Offers[o.SKUID] = append(lp.Offers[o.SKUID], o)
			}
		}
	}
	return lp, nil
}

// loadPurchases 取这个买家在带限购的那些活动里已经买了几件。没有限购时不查。
func loadPurchases(ctx context.Context, tx repository.Tx, userID int64,
	lp livePromotions) (map[repository.PurchaseKey]int32, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, os := range lp.Offers {
		for _, o := range os {
			if o.PerUserLimit > 0 && !seen[o.PromotionID] {
				seen[o.PromotionID] = true
				ids = append(ids, o.PromotionID)
			}
		}
	}
	if len(ids) == 0 {
		return map[repository.PurchaseKey]int32{}, nil
	}
	return tx.ListUserPromotionPurchases(ctx, userID, ids)
}

// couponLinesOf 把定价结果变成券计算的输入。试算、下单、「本单可用券」共用。
//
// 金额是**活动后金额**：AmountCents − 满减满折分摊到这一行的那一份（单价类活动已经在
// AmountCents 里）。券的门槛比的、折扣乘的、分摊按的都是它 —— 于是一行上活动与券的
// 分摊之和不会超过这一行的金额，chk_item_promotion_discount 与 chk_item_refund 都成立。
func couponLinesOf(lines []PricedLine) []couponLine {
	out := make([]couponLine, len(lines))
	for i, ln := range lines {
		out[i] = couponLine{
			ProductID:    ln.ProductID,
			BrandID:      ln.brandID,
			CategoryPath: ln.categoryPath,
			AmountCents:  ln.AmountCents - ln.PromotionDiscountCents,
		}
	}
	return out
}

// skuIDsOf 是定价结果里的 sku_id，按行序。
func skuIDsOf(lines []PricedLine) []int64 {
	out := make([]int64, len(lines))
	for i, ln := range lines {
		out[i] = ln.SKUID
	}
	return out
}

// applyCoupon 把请求里那张券算进 q：总减免、每行分摊、回显券 id。
//
// 包邮券（coupon_type = 4）在这里只判「能不能用」（状态、有效期、范围、门槛），
// 不减商品的钱：它抵的是运费，而运费要等券之后才算得出来（计价顺序）。
// 这种券原样返回，由 priceOrder 在运费之后抵扣。
func applyCoupon(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	q *Quote, coupon couponRequest) (*repository.UserCoupon, error) {
	c, err := tx.FindUserCoupon(ctx, *coupon.ID, coupon.UserID)
	if errors.Is(err, repository.ErrCouponNotFound) {
		return nil, fmt.Errorf("%w: user_coupon_id=%d 不存在或不属于你", ErrCouponNotApplicable, *coupon.ID)
	}
	if err != nil {
		return nil, err
	}
	scopes, err := tx.ListCouponScopes(ctx, []int64{c.Rule.TemplateID})
	if err != nil {
		return nil, err
	}
	if len(q.couponBlockers) > 0 {
		// 放在找到券之后：先报「不是你的券」，再报「本单不能用券」—— 反过来的话，
		// 命中互斥活动的单子就成了一个不报「券存不存在」的探测口，而别的单子报。
		return nil, fmt.Errorf("%w: 本单命中的活动「%s」不与优惠券同享",
			ErrCouponNotApplicable, strings.Join(q.couponBlockers, "」「"))
	}
	v := evaluateCoupon(c, scopes[c.Rule.TemplateID], sc, couponLinesOf(q.Lines), coupon.Now)
	if !v.Applicable {
		return nil, fmt.Errorf("%w: %s", ErrCouponNotApplicable, v.Reason)
	}
	if v.FreeShipping {
		return &c, nil
	}
	for i := range q.Lines {
		q.Lines[i].DiscountCents += v.LineDiscounts[i]
	}
	q.DiscountCents += v.DiscountCents
	q.CouponDiscountCents = v.DiscountCents
	id := c.ID
	q.UserCouponID = &id
	return nil, nil
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
//
// 本单命中了不与券同享的活动时直接回空：那时带哪一张券试算都是 409。
// 包邮券按 freightNoCoupon（不用任何券时的运费）算能抵多少：带上它去试算时商品上
// 没有券的减免，运费正是这一个数。nil（没有地址）时包邮券一张都不出现 —— 判不了。
func applicableCoupons(ctx context.Context, tx repository.Tx, sc repository.StoreScope,
	userID int64, q Quote, now time.Time) ([]ApplicableCoupon, error) {
	if len(q.couponBlockers) > 0 {
		return []ApplicableCoupon{}, nil
	}
	lines, freightNoCoupon := q.Lines, q.freightNoCoupon
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
		off := v.DiscountCents
		if v.FreeShipping {
			if freightNoCoupon == nil {
				continue
			}
			off = freeShippingDeduction(c.Rule.MaxDiscountCents, *freightNoCoupon)
			if off <= 0 {
				continue
			}
		}
		out = append(out, ApplicableCoupon{
			Coupon: c, Scopes: scopes[c.Rule.TemplateID], DiscountCents: off,
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

// lineImageURL 是一行商品该显示的图：SKU 自己有图用 SKU 的，否则用商品主图（product_images 第一张）。
// 订单行的 image_snapshot 与购物车行都走它，两处的退路是同一条规则。
func lineImageURL(skuImage *string, mainUploadID int64) *string {
	if skuImage != nil && *skuImage != "" {
		return skuImage
	}
	if mainUploadID > 0 {
		u := UploadURL(mainUploadID)
		return &u
	}
	return nil
}

// LineImageURL 给 handler 用（购物车行）。
func LineImageURL(skuImage *string, mainUploadID int64) *string {
	return lineImageURL(skuImage, mainUploadID)
}
