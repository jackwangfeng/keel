package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 营销活动在 repository 边界上的那一面。数据模型 §7「营销活动」，迁移 00058。
//
// 与 coupon.go 同一个分工：这一层**只取素材、只做条件更新**，不判定「这个活动在
// 这一单上减多少、哪几行参与」—— 那个判定只有一份实现（service/promotion_calc.go）。

var (
	// ErrPromotionNotFound：活动在本租户查不到（RLS 之下，别家的与不存在的同形）。
	ErrPromotionNotFound = errors.New("营销活动不存在")

	// ErrPromotionQuotaExhausted：活动配额的条件 UPDATE 受影响 0 行 —— 秒杀配额不够这一单，
	// 或这个 SKU 在建单之后被移出了活动。
	ErrPromotionQuotaExhausted = errors.New("活动配额不足")

	// ErrPromotionLimitReached：每人限购的累计 upsert 受影响 0 行。
	ErrPromotionLimitReached = errors.New("超出活动每人限购")

	// ErrPromotionRuleViolation：写活动时撞上了 chk_promotion_* 约束或子表的唯一约束。
	// service 已经把规则校验过一遍，走到这里说明两边的规则分叉了 —— 数据库是最后一道，
	// 翻成 422 而不是 500：那确实是请求不成立。
	ErrPromotionRuleViolation = errors.New("营销活动的规则不成立")
)

// 活动类型（数据模型 §7 promotions.promo_type）。
const (
	PromoFullReduction int16 = 1 // 满减
	PromoFullDiscount  int16 = 2 // 满折
	PromoLimitedPrice  int16 = 3 // 限时折扣（特价）
	PromoFlashSale     int16 = 4 // 秒杀
	PromoNewBuyerGift  int16 = 5 // 新人礼
)

// 满减满折的门槛单位。
const (
	ThresholdByAmount int16 = 1 // 金额（分）
	ThresholdByQty    int16 = 2 // 件数
)

// CouponSourceNewBuyerGift 是新人礼发出的券（user_coupons.source = 3，00058）。
const CouponSourceNewBuyerGift int16 = 3

// Promotion 是一个活动本体（不含阶梯、范围、活动商品）。
type Promotion struct {
	ID              int64
	Name            string
	Type            int16
	ThresholdUnit   int16
	StackWithCoupon bool
	GiftTemplateID  *int64
	StartsAt        time.Time
	EndsAt          time.Time
	Status          int16
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// PromotionTier 是满减 / 满折的一档。
type PromotionTier struct {
	Threshold     int64
	DiscountCents int64
	DiscountRate  int16
}

// PriceOffer 是一个 SKU 在一个限时折扣 / 秒杀里的报价与配额。
type PriceOffer struct {
	PromotionID     int64
	SKUID           int64
	PromoPriceCents int64
	DiscountRate    int16
	PerUserLimit    int32
	StockQty        int32
	SoldQty         int32

	// 以下两列只有 ListLivePriceOffersForProducts 填（商品标签要按门店价算折扣类的特价）。
	ProductID       int64
	StorePriceCents int64
}

// PromotionSku 是后台看到的一个活动商品。
type PromotionSku struct {
	PriceOffer
	SKUCode string
	Title   string
}

// PromotionSkuInput 是后台写活动商品的一条。
type PromotionSkuInput struct {
	SKUID           int64
	PromoPriceCents int64
	DiscountRate    int16
	PerUserLimit    int32
	StockQty        int32
}

// ProductPromotionFacts 是满减满折按范围挑商品要的素材（与 PriceableSKU 那两列同源）。
type ProductPromotionFacts struct {
	ProductID    int64
	BrandID      *int64
	CategoryPath *string
}

// PurchaseKey 是每人限购累计的键（活动 × SKU；买家由调用方固定）。
type PurchaseKey struct {
	PromotionID int64
	SKUID       int64
}

// PromotionFields 是活动上可写的全部列。新建与整行写回共用。
type PromotionFields struct {
	Name            string
	Type            int16
	ThresholdUnit   int16
	StackWithCoupon bool
	GiftTemplateID  *int64
	StartsAt        time.Time
	EndsAt          time.Time
	Status          int16
}

// PromotionTx 是营销活动这一面。
type PromotionTx interface {
	// ListLivePromotions 此刻生效（上线且在有效期内）的全部活动。
	ListLivePromotions(ctx context.Context, now time.Time) ([]Promotion, error)
	// ListPromotionTiers 一批活动的阶梯，按门槛升序、按活动分组。
	ListPromotionTiers(ctx context.Context, promotionIDs []int64) (map[int64][]PromotionTier, error)
	// ListPromotionScopes 一批活动的范围规则，按活动分组。复用 CouponScope 这个形状
	// （TemplateID 一栏放的是活动 id）：范围语义只有一套，计算复用 coupon_calc.go。
	ListPromotionScopes(ctx context.Context, promotionIDs []int64) (map[int64][]CouponScope, error)
	// ListLivePriceOffers 一批 SKU 身上此刻生效的限时折扣 / 秒杀报价。
	ListLivePriceOffers(ctx context.Context, skuIDs []int64, now time.Time) ([]PriceOffer, error)
	// ListLivePriceOffersForProducts 一批商品的全部 SKU 身上此刻生效的报价，连同这家店的门店价。
	ListLivePriceOffersForProducts(ctx context.Context, storeID int64, productIDs []int64,
		now time.Time) ([]PriceOffer, error)
	// ListProductPromotionFacts 一批商品的品牌与分类 path。
	ListProductPromotionFacts(ctx context.Context, productIDs []int64) (map[int64]ProductPromotionFacts, error)
	// ListUserPromotionPurchases 这个买家在这些活动里各 SKU 已买的件数。
	ListUserPromotionPurchases(ctx context.Context, userID int64, promotionIDs []int64) (map[PurchaseKey]int32, error)

	// ReservePromotionQuota 库存分支正向：扣活动配额，并在同一把行锁之下累计每人限购。
	// 配额不够返回 ErrPromotionQuotaExhausted，超限返回 ErrPromotionLimitReached。
	ReservePromotionQuota(ctx context.Context, promotionID, skuID, userID int64, qty int32) error
	// ReleasePromotionQuota 补偿 / 超时关单 / 买家取消：把配额与限购放回。
	// 返回是否真的放回了配额（假：这个 SKU 已被移出活动，或计数已经不够减）。
	ReleasePromotionQuota(ctx context.Context, promotionID, skuID, userID int64, qty int32) (bool, error)

	// 新人礼。
	UserHasPlacedOrder(ctx context.Context, userID int64) (bool, error)
	HasGiftGrant(ctx context.Context, promotionID, userID int64) (bool, error)
	// InsertGiftGrant 返回是否插入（假：这个买家在这个活动里已经领过）。
	InsertGiftGrant(ctx context.Context, promotionID, userID, userCouponID int64) (bool, error)

	// 后台。
	AdminListPromotions(ctx context.Context, status, promoType *int16, limit, offset int32) ([]Promotion, int64, error)
	AdminGetPromotion(ctx context.Context, id int64) (Promotion, error)
	// AdminLockPromotion 锁住活动行并返回它的 status。查不到返回 ErrPromotionNotFound。
	AdminLockPromotion(ctx context.Context, id int64) (int16, error)
	AdminCreatePromotion(ctx context.Context, f PromotionFields) (int64, error)
	AdminUpdatePromotion(ctx context.Context, id int64, f PromotionFields) error
	ReplacePromotionTiers(ctx context.Context, promotionID int64, tiers []PromotionTier) error
	ReplacePromotionScopes(ctx context.Context, promotionID int64, scopes []CouponScopeInput) error
	// ReplacePromotionSkus 整组替换活动商品：逐条 upsert（保留 sold_qty），删掉不在名单里且没卖过的。
	ReplacePromotionSkus(ctx context.Context, promotionID int64, skus []PromotionSkuInput) error
	ListPromotionSkus(ctx context.Context, promotionIDs []int64) (map[int64][]PromotionSku, error)
	CountGiftGrants(ctx context.Context, promotionIDs []int64) (map[int64]int32, error)
	// LiveSkuIDs 返回 ids 里在本租户存在且未软删的 SKU。
	LiveSkuIDs(ctx context.Context, ids []int64) ([]int64, error)
	// LiveCouponTemplateIDs 返回 ids 里在本租户存在的券模板。
	LiveCouponTemplateIDs(ctx context.Context, ids []int64) ([]int64, error)
}

// promotionRow 是三条活动读查询共有的列。
type promotionRow struct {
	ID              int64
	Name            string
	PromoType       int16
	ThresholdUnit   int16
	StackWithCoupon bool
	GiftTemplateID  *int64
	StartsAt        time.Time
	EndsAt          time.Time
	Status          int16
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (r promotionRow) domain() Promotion {
	return Promotion{
		ID: r.ID, Name: r.Name, Type: r.PromoType, ThresholdUnit: r.ThresholdUnit,
		StackWithCoupon: r.StackWithCoupon, GiftTemplateID: r.GiftTemplateID,
		StartsAt: r.StartsAt, EndsAt: r.EndsAt, Status: r.Status,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (t tenantTx) ListLivePromotions(ctx context.Context, now time.Time) ([]Promotion, error) {
	rows, err := t.q.ListLivePromotions(ctx, ts(now))
	if err != nil {
		return nil, err
	}
	out := make([]Promotion, 0, len(rows))
	for _, r := range rows {
		out = append(out, Promotion{
			ID: r.ID, Name: r.Name, Type: r.PromoType, ThresholdUnit: r.ThresholdUnit,
			StackWithCoupon: r.StackWithCoupon, GiftTemplateID: r.GiftTemplateID,
			StartsAt: r.StartsAt.Time, EndsAt: r.EndsAt.Time, Status: 1,
		})
	}
	return out, nil
}

func (t tenantTx) ListPromotionTiers(ctx context.Context, promotionIDs []int64) (map[int64][]PromotionTier, error) {
	out := map[int64][]PromotionTier{}
	if len(promotionIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListPromotionTiers(ctx, promotionIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PromotionID] = append(out[r.PromotionID], PromotionTier{
			Threshold: r.Threshold, DiscountCents: r.DiscountCents, DiscountRate: r.DiscountRate,
		})
	}
	return out, nil
}

func (t tenantTx) ListPromotionScopes(ctx context.Context, promotionIDs []int64) (map[int64][]CouponScope, error) {
	out := map[int64][]CouponScope{}
	if len(promotionIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListPromotionScopes(ctx, promotionIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PromotionID] = append(out[r.PromotionID], CouponScope{
			TemplateID:   r.PromotionID,
			ScopeType:    r.ScopeType,
			TargetID:     r.TargetID,
			Include:      r.Include,
			CategoryPath: r.CategoryPath,
			TargetName:   firstNonNil(r.CategoryName, r.ProductTitle, r.RegionName, r.StoreName),
		})
	}
	return out, nil
}

func (t tenantTx) ListLivePriceOffers(ctx context.Context, skuIDs []int64, now time.Time) ([]PriceOffer, error) {
	if len(skuIDs) == 0 {
		return nil, nil
	}
	rows, err := t.q.ListLivePriceOffers(ctx, db.ListLivePriceOffersParams{SkuIds: skuIDs, Now: ts(now)})
	if err != nil {
		return nil, err
	}
	out := make([]PriceOffer, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceOffer{
			PromotionID: r.PromotionID, SKUID: r.SkuID, PromoPriceCents: r.PromoPriceCents,
			DiscountRate: r.DiscountRate, PerUserLimit: r.PerUserLimit, StockQty: r.StockQty,
			SoldQty: r.SoldQty,
		})
	}
	return out, nil
}

func (t tenantTx) ListLivePriceOffersForProducts(ctx context.Context, storeID int64, productIDs []int64,
	now time.Time) ([]PriceOffer, error) {
	if len(productIDs) == 0 {
		return nil, nil
	}
	rows, err := t.q.ListLivePriceOffersForProducts(ctx, db.ListLivePriceOffersForProductsParams{
		StoreID: storeID, ProductIds: productIDs, Now: ts(now),
	})
	if err != nil {
		return nil, err
	}
	out := make([]PriceOffer, 0, len(rows))
	for _, r := range rows {
		out = append(out, PriceOffer{
			PromotionID: r.PromotionID, SKUID: r.SkuID, PromoPriceCents: r.PromoPriceCents,
			DiscountRate: r.DiscountRate, PerUserLimit: r.PerUserLimit, StockQty: r.StockQty,
			SoldQty: r.SoldQty, ProductID: r.ProductID, StorePriceCents: r.StorePriceCents,
		})
	}
	return out, nil
}

func (t tenantTx) ListProductPromotionFacts(ctx context.Context, productIDs []int64) (map[int64]ProductPromotionFacts, error) {
	out := map[int64]ProductPromotionFacts{}
	if len(productIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListProductPromotionFacts(ctx, productIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ID] = ProductPromotionFacts{ProductID: r.ID, BrandID: r.BrandID, CategoryPath: r.CategoryPath}
	}
	return out, nil
}

func (t tenantTx) ListUserPromotionPurchases(ctx context.Context, userID int64,
	promotionIDs []int64) (map[PurchaseKey]int32, error) {
	out := map[PurchaseKey]int32{}
	if len(promotionIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListUserPromotionPurchases(ctx, db.ListUserPromotionPurchasesParams{
		UserID: userID, PromotionIds: promotionIDs,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[PurchaseKey{PromotionID: r.PromotionID, SKUID: r.SkuID}] = r.Qty
	}
	return out, nil
}

// ReservePromotionQuota 见接口上的注释。两条语句的顺序是论证的一部分：
// 先拿 promotion_skus 那一行的行锁（条件 UPDATE），再在锁之下累计每人限购 ——
// 同一个买家的两笔并发订单因此在那把锁上排队，后到者的 upsert 看得见前者已提交的件数。
func (t tenantTx) ReservePromotionQuota(ctx context.Context, promotionID, skuID, userID int64, qty int32) error {
	limit, err := t.q.ReservePromotionSku(ctx, db.ReservePromotionSkuParams{
		Qty: qty, PromotionID: promotionID, SkuID: skuID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("promotion %d sku %d 要 %d 件: %w", promotionID, skuID, qty, ErrPromotionQuotaExhausted)
	}
	if err != nil {
		return err
	}
	if limit == 0 {
		return nil
	}
	n, err := t.q.AddPromotionPurchase(ctx, db.AddPromotionPurchaseParams{
		PromotionID: promotionID, SkuID: skuID, UserID: userID, Qty: qty, PerUserLimit: limit,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("promotion %d sku %d 每人限购 %d 件，这一单要 %d 件: %w",
			promotionID, skuID, limit, qty, ErrPromotionLimitReached)
	}
	return nil
}

func (t tenantTx) ReleasePromotionQuota(ctx context.Context, promotionID, skuID, userID int64, qty int32) (bool, error) {
	n, err := t.q.ReleasePromotionSku(ctx, db.ReleasePromotionSkuParams{
		Qty: qty, PromotionID: promotionID, SkuID: skuID,
	})
	if err != nil {
		return false, err
	}
	// 限购那一行只有 per_user_limit > 0 时才存在；不存在时受影响 0 行是正常路径。
	if _, err := t.q.ReleasePromotionPurchase(ctx, db.ReleasePromotionPurchaseParams{
		Qty: qty, PromotionID: promotionID, SkuID: skuID, UserID: userID,
	}); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (t tenantTx) UserHasPlacedOrder(ctx context.Context, userID int64) (bool, error) {
	return t.q.UserHasPlacedOrder(ctx, userID)
}

func (t tenantTx) HasGiftGrant(ctx context.Context, promotionID, userID int64) (bool, error) {
	return t.q.HasGiftGrant(ctx, db.HasGiftGrantParams{PromotionID: promotionID, UserID: userID})
}

func (t tenantTx) InsertGiftGrant(ctx context.Context, promotionID, userID, userCouponID int64) (bool, error) {
	n, err := t.q.InsertGiftGrant(ctx, db.InsertGiftGrantParams{
		PromotionID: promotionID, UserID: userID, UserCouponID: userCouponID,
	})
	return n == 1, err
}

func (t tenantTx) AdminListPromotions(ctx context.Context, status, promoType *int16,
	limit, offset int32) ([]Promotion, int64, error) {
	rows, err := t.q.AdminListPromotions(ctx, db.AdminListPromotionsParams{
		Status: status, PromoType: promoType, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.AdminCountPromotions(ctx, db.AdminCountPromotionsParams{Status: status, PromoType: promoType})
	if err != nil {
		return nil, 0, err
	}
	out := make([]Promotion, 0, len(rows))
	for _, r := range rows {
		out = append(out, promotionRow{
			ID: r.ID, Name: r.Name, PromoType: r.PromoType, ThresholdUnit: r.ThresholdUnit,
			StackWithCoupon: r.StackWithCoupon, GiftTemplateID: r.GiftTemplateID,
			StartsAt: r.StartsAt.Time, EndsAt: r.EndsAt.Time, Status: r.Status,
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		}.domain())
	}
	return out, total, nil
}

func (t tenantTx) AdminGetPromotion(ctx context.Context, id int64) (Promotion, error) {
	r, err := t.q.AdminGetPromotion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Promotion{}, fmt.Errorf("promotion %d: %w", id, ErrPromotionNotFound)
	}
	if err != nil {
		return Promotion{}, err
	}
	return promotionRow{
		ID: r.ID, Name: r.Name, PromoType: r.PromoType, ThresholdUnit: r.ThresholdUnit,
		StackWithCoupon: r.StackWithCoupon, GiftTemplateID: r.GiftTemplateID,
		StartsAt: r.StartsAt.Time, EndsAt: r.EndsAt.Time, Status: r.Status,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
	}.domain(), nil
}

func (t tenantTx) AdminLockPromotion(ctx context.Context, id int64) (int16, error) {
	r, err := t.q.AdminLockPromotion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("promotion %d: %w", id, ErrPromotionNotFound)
	}
	if err != nil {
		return 0, err
	}
	return r.Status, nil
}

// promotionViolation 把 chk_promotion* / uk_promotion_* 翻成 ErrPromotionRuleViolation。
// 按约束名前缀挑，理由同 couponCheckViolation：别的约束撞上了是另一种 bug。
func promotionViolation(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	name := pgErr.ConstraintName
	switch {
	case pgErr.Code == "23514" && len(name) >= 13 && name[:13] == "chk_promotion":
		return fmt.Errorf("%w（%s）", ErrPromotionRuleViolation, name)
	case pgErr.Code == "23505" && len(name) >= 12 && name[:12] == "uk_promotion":
		return fmt.Errorf("%w（%s：同一条出现了两次）", ErrPromotionRuleViolation, name)
	}
	return err
}

func (t tenantTx) AdminCreatePromotion(ctx context.Context, f PromotionFields) (int64, error) {
	id, err := t.q.AdminCreatePromotion(ctx, db.AdminCreatePromotionParams{
		Name: f.Name, PromoType: f.Type, ThresholdUnit: f.ThresholdUnit,
		StackWithCoupon: f.StackWithCoupon, GiftTemplateID: f.GiftTemplateID,
		StartsAt: ts(f.StartsAt), EndsAt: ts(f.EndsAt),
	})
	if err != nil {
		return 0, promotionViolation(err)
	}
	return id, nil
}

func (t tenantTx) AdminUpdatePromotion(ctx context.Context, id int64, f PromotionFields) error {
	n, err := t.q.AdminUpdatePromotion(ctx, db.AdminUpdatePromotionParams{
		ID: id, Name: f.Name, ThresholdUnit: f.ThresholdUnit, StackWithCoupon: f.StackWithCoupon,
		GiftTemplateID: f.GiftTemplateID, StartsAt: ts(f.StartsAt), EndsAt: ts(f.EndsAt),
		Status: f.Status,
	})
	if err != nil {
		return promotionViolation(err)
	}
	if n != 1 {
		return fmt.Errorf("promotion %d: %w", id, ErrPromotionNotFound)
	}
	return nil
}

func (t tenantTx) ReplacePromotionTiers(ctx context.Context, promotionID int64, tiers []PromotionTier) error {
	if err := t.q.DeletePromotionTiers(ctx, promotionID); err != nil {
		return err
	}
	for _, tr := range tiers {
		if err := t.q.InsertPromotionTier(ctx, db.InsertPromotionTierParams{
			PromotionID: promotionID, Threshold: tr.Threshold,
			DiscountCents: tr.DiscountCents, DiscountRate: tr.DiscountRate,
		}); err != nil {
			return promotionViolation(err)
		}
	}
	return nil
}

func (t tenantTx) ReplacePromotionScopes(ctx context.Context, promotionID int64, scopes []CouponScopeInput) error {
	if err := t.q.DeletePromotionScopes(ctx, promotionID); err != nil {
		return err
	}
	for _, s := range scopes {
		if err := t.q.InsertPromotionScope(ctx, db.InsertPromotionScopeParams{
			PromotionID: promotionID, ScopeType: s.ScopeType, TargetID: s.TargetID, Include: s.Include,
		}); err != nil {
			return promotionViolation(err)
		}
	}
	return nil
}

func (t tenantTx) ReplacePromotionSkus(ctx context.Context, promotionID int64, skus []PromotionSkuInput) error {
	keep := make([]int64, 0, len(skus))
	for _, s := range skus {
		if err := t.q.UpsertPromotionSku(ctx, db.UpsertPromotionSkuParams{
			PromotionID: promotionID, SkuID: s.SKUID, PromoPriceCents: s.PromoPriceCents,
			DiscountRate: s.DiscountRate, PerUserLimit: s.PerUserLimit, StockQty: s.StockQty,
		}); err != nil {
			return promotionViolation(err)
		}
		keep = append(keep, s.SKUID)
	}
	return t.q.DeletePromotionSkusExcept(ctx, db.DeletePromotionSkusExceptParams{
		PromotionID: promotionID, KeepSkuIds: keep,
	})
}

func (t tenantTx) ListPromotionSkus(ctx context.Context, promotionIDs []int64) (map[int64][]PromotionSku, error) {
	out := map[int64][]PromotionSku{}
	if len(promotionIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListPromotionSkusAdmin(ctx, promotionIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PromotionID] = append(out[r.PromotionID], PromotionSku{
			PriceOffer: PriceOffer{
				PromotionID: r.PromotionID, SKUID: r.SkuID, PromoPriceCents: r.PromoPriceCents,
				DiscountRate: r.DiscountRate, PerUserLimit: r.PerUserLimit, StockQty: r.StockQty,
				SoldQty: r.SoldQty,
			},
			SKUCode: r.SkuCode, Title: r.Title,
		})
	}
	return out, nil
}

func (t tenantTx) CountGiftGrants(ctx context.Context, promotionIDs []int64) (map[int64]int32, error) {
	out := map[int64]int32{}
	if len(promotionIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.CountGiftGrants(ctx, promotionIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PromotionID] = r.Granted
	}
	return out, nil
}

func (t tenantTx) LiveSkuIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return t.q.LiveSkuIDs(ctx, ids)
}

func (t tenantTx) LiveCouponTemplateIDs(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return t.q.LiveCouponTemplateIDs(ctx, ids)
}
