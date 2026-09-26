package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/repository"
)

// 营销活动的后台：列表 / 新建 / 详情 / 修改 / 上下线。数据模型 §7「营销活动」。
//
// # 权限：与券管理同一行
//
// 活动直接决定订单实付，是资金面 —— 判据是 requireMerchantWide（全店范围：商家级管理员与
// 操作员，平台级切到本店时同样算），与券模板一模一样。大区 / 门店管理员不行：活动的范围可以
// 跨大区，一个只管华北的人不该能开一个全国满减。登记在 permission_test.go 的大表里。
//
// # 上线中不许改规则
//
// 上线中的活动随时可能正被一笔试算或下单读着。边读边改，试算按旧规则、下单按新规则算钱，
// 那正是「试算与下单必须共用一份计算」要防的事，只不过换成了数据而不是代码在分叉。
// 所以上线中只能改名、下线；改规则先下线（409 promotion-online）。
// 已成交的订单不受任何修改影响：订单行快照了单价与分摊额，订单上快照了命中的活动。

var (
	// ErrPromotionBadRequest：规则本身不成立（满 100 减 200、秒杀没配额、范围目标查不到……）。422。
	ErrPromotionBadRequest = errors.New("营销活动的配置不成立")

	// ErrPromotionNotFound：活动在本租户查不到。404。
	ErrPromotionNotFound = errors.New("营销活动不存在")

	// ErrPromotionOnline：活动上线中却想改规则。409 promotion-online。
	ErrPromotionOnline = errors.New("活动上线中，改规则请先下线")
)

const (
	scopeAdminPromotionCreate = "admin.promotions.create"

	maxPromotionNameRunes = 60
	maxPromotionTiers     = 10
	maxPromotionScopes    = 200
	maxPromotionSkus      = 200
)

// AdminPromotionService 实现 /admin/promotions 那四条接口。
type AdminPromotionService struct {
	repo CouponRepository
	now  func() time.Time
}

func NewAdminPromotionService(r CouponRepository) *AdminPromotionService {
	return &AdminPromotionService{repo: r, now: time.Now}
}

// PromotionPhase 是现算的阶段（契约 AdminPromotion.phase）。
type PromotionPhase string

const (
	PhaseOffline   PromotionPhase = "offline"
	PhaseScheduled PromotionPhase = "scheduled"
	PhaseRunning   PromotionPhase = "running"
	PhaseEnded     PromotionPhase = "ended"
)

func phaseOf(p repository.Promotion, now time.Time) PromotionPhase {
	switch {
	case p.Status != 1:
		return PhaseOffline
	case now.Before(p.StartsAt):
		return PhaseScheduled
	case !now.Before(p.EndsAt):
		return PhaseEnded
	default:
		return PhaseRunning
	}
}

// AdminPromotionView 是后台看到的一个活动：本体、阶梯、范围、活动商品、新人礼已发张数。
type AdminPromotionView struct {
	Promotion   repository.Promotion
	Phase       PromotionPhase
	Tiers       []repository.PromotionTier
	Scopes      []repository.CouponScope
	Skus        []repository.PromotionSku
	GiftGranted int32
}

// AdminPromotionPage 是 GET /admin/promotions 的一页。
type AdminPromotionPage struct {
	Items    []AdminPromotionView
	Total    int64
	Page     int
	PageSize int
}

func (s *AdminPromotionService) views(ctx context.Context, tx repository.Tx,
	ps []repository.Promotion) ([]AdminPromotionView, error) {
	ids := make([]int64, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID)
	}
	tiers, err := tx.ListPromotionTiers(ctx, ids)
	if err != nil {
		return nil, err
	}
	scopes, err := tx.ListPromotionScopes(ctx, ids)
	if err != nil {
		return nil, err
	}
	skus, err := tx.ListPromotionSkus(ctx, ids)
	if err != nil {
		return nil, err
	}
	granted, err := tx.CountGiftGrants(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]AdminPromotionView, 0, len(ps))
	for _, p := range ps {
		out = append(out, AdminPromotionView{
			Promotion: p, Phase: phaseOf(p, now), Tiers: tiers[p.ID], Scopes: scopes[p.ID],
			Skus: skus[p.ID], GiftGranted: granted[p.ID],
		})
	}
	return out, nil
}

func (s *AdminPromotionService) view(ctx context.Context, tx repository.Tx, id int64) (AdminPromotionView, error) {
	p, err := tx.AdminGetPromotion(ctx, id)
	if errors.Is(err, repository.ErrPromotionNotFound) {
		return AdminPromotionView{}, fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
	}
	if err != nil {
		return AdminPromotionView{}, err
	}
	vs, err := s.views(ctx, tx, []repository.Promotion{p})
	if err != nil {
		return AdminPromotionView{}, err
	}
	return vs[0], nil
}

// List 实现 GET /admin/promotions。
func (s *AdminPromotionService) List(ctx context.Context, status, promoType *int16,
	page, pageSize int) (AdminPromotionPage, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionPage{}, err
	}
	if status != nil && *status != 0 && *status != 1 {
		return AdminPromotionPage{}, fmt.Errorf("%w: status 只能是 0 或 1", ErrPromotionBadRequest)
	}
	if promoType != nil && (*promoType < 1 || *promoType > 5) {
		return AdminPromotionPage{}, fmt.Errorf("%w: promotion_type 只能是 1 到 5", ErrPromotionBadRequest)
	}
	page, pageSize = clampPaging(page, pageSize)
	out := AdminPromotionPage{Items: []AdminPromotionView{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ps, total, err := tx.AdminListPromotions(ctx, status, promoType,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		out.Total = total
		out.Items, err = s.views(ctx, tx, ps)
		return err
	})
	if err != nil {
		return AdminPromotionPage{}, err
	}
	return out, nil
}

// Get 实现 GET /admin/promotions/{promotion_id}。
func (s *AdminPromotionService) Get(ctx context.Context, id int64) (AdminPromotionView, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionView{}, err
	}
	var out AdminPromotionView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		out, err = s.view(ctx, tx, id)
		return err
	})
	return out, err
}

// PromotionRules 是活动的规则部分：新建时全给，修改时 nil 表示不改。
type PromotionRules struct {
	Tiers  *[]repository.PromotionTier
	Scopes *[]repository.CouponScopeInput
	Skus   *[]repository.PromotionSkuInput
}

// PromotionInput 是新建请求。
type PromotionInput struct {
	Name            string
	Type            int16
	ThresholdUnit   int16
	StackWithCoupon bool
	StartsAt        time.Time
	EndsAt          time.Time
	GiftTemplateID  *int64
	Rules           PromotionRules
}

// Create 实现 POST /admin/promotions。新建的活动一律是下线的。
func (s *AdminPromotionService) Create(ctx context.Context, in PromotionInput,
	idemKey string) (AdminPromotionView, bool, error) {
	staff, err := requireMerchantWide(ctx)
	if err != nil {
		return AdminPromotionView{}, false, err
	}
	f := repository.PromotionFields{
		Name: strings.TrimSpace(in.Name), Type: in.Type, ThresholdUnit: in.ThresholdUnit,
		StackWithCoupon: in.StackWithCoupon, GiftTemplateID: in.GiftTemplateID,
		StartsAt: in.StartsAt, EndsAt: in.EndsAt, Status: 0,
	}
	tiers, scopes, skus := derefRules(in.Rules)
	if err := validatePromotion(f, tiers, scopes, skus); err != nil {
		return AdminPromotionView{}, false, err
	}
	hash, err := adminRequestHash(nil, in)
	if err != nil {
		return AdminPromotionView{}, false, err
	}
	return idempotentTenantWrite(ctx, s.repo, scopeAdminPromotionCreate, repository.StaffSubject(staff.StaffID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (AdminPromotionView, error) {
			if err := checkPromotionTargets(ctx, tx, f, scopes, skus); err != nil {
				return AdminPromotionView{}, err
			}
			id, err := tx.AdminCreatePromotion(ctx, f)
			if err != nil {
				return AdminPromotionView{}, mapPromotionRepoErr(err)
			}
			if err := writePromotionRules(ctx, tx, id, in.Rules); err != nil {
				return AdminPromotionView{}, err
			}
			return s.view(ctx, tx, id)
		})
}

// PromotionPatch 是 PATCH 请求：nil 表示不改。
type PromotionPatch struct {
	Name            *string
	ThresholdUnit   *int16
	StackWithCoupon *bool
	StartsAt        *time.Time
	EndsAt          *time.Time
	GiftTemplateID  *int64
	Status          *int16
	Rules           PromotionRules
}

// ruleChanges：这次 PATCH 除了 name / status 之外有没有碰规则。
func (p PromotionPatch) touchesRules() bool {
	return p.ThresholdUnit != nil || p.StackWithCoupon != nil || p.StartsAt != nil ||
		p.EndsAt != nil || p.GiftTemplateID != nil || p.Rules.Tiers != nil ||
		p.Rules.Scopes != nil || p.Rules.Skus != nil
}

// Update 实现 PATCH /admin/promotions/{promotion_id}。
//
// 先锁活动行、再判「上线中不许改规则」、再合并与校验、最后整行写回与整组替换 ——
// 判定与写必须看到同一个 status（AdminLockPromotion 上的注释）。
func (s *AdminPromotionService) Update(ctx context.Context, id int64, p PromotionPatch) (AdminPromotionView, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return AdminPromotionView{}, err
	}
	if p.Status != nil && *p.Status != 0 && *p.Status != 1 {
		return AdminPromotionView{}, fmt.Errorf("%w: status 只能是 0 或 1", ErrPromotionBadRequest)
	}
	var out AdminPromotionView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		status, err := tx.AdminLockPromotion(ctx, id)
		if errors.Is(err, repository.ErrPromotionNotFound) {
			return fmt.Errorf("%w: promotion_id=%d", ErrPromotionNotFound, id)
		}
		if err != nil {
			return err
		}
		if status == 1 && p.touchesRules() {
			return fmt.Errorf("%w: 上线中的活动只能改名或下线", ErrPromotionOnline)
		}
		cur, err := s.view(ctx, tx, id)
		if err != nil {
			return err
		}
		f := promotionFieldsOf(cur.Promotion)
		if p.Name != nil {
			f.Name = strings.TrimSpace(*p.Name)
		}
		if p.ThresholdUnit != nil {
			f.ThresholdUnit = *p.ThresholdUnit
		}
		if p.StackWithCoupon != nil {
			f.StackWithCoupon = *p.StackWithCoupon
		}
		if p.StartsAt != nil {
			f.StartsAt = *p.StartsAt
		}
		if p.EndsAt != nil {
			f.EndsAt = *p.EndsAt
		}
		if p.GiftTemplateID != nil {
			f.GiftTemplateID = p.GiftTemplateID
		}
		if p.Status != nil {
			f.Status = *p.Status
		}

		// 合并之后的完整规则：没传的一组沿用现状。
		tiers, scopes, skus := cur.Tiers, scopeInputsOf(cur.Scopes), skuInputsOf(cur.Skus)
		if p.Rules.Tiers != nil {
			tiers = *p.Rules.Tiers
		}
		if p.Rules.Scopes != nil {
			scopes = *p.Rules.Scopes
		}
		if p.Rules.Skus != nil {
			skus = *p.Rules.Skus
			if err := checkSoldSkusKept(cur.Skus, skus); err != nil {
				return err
			}
		}
		if err := validatePromotion(f, tiers, scopes, skus); err != nil {
			return err
		}
		// 上线（0 → 1）时再核一遍「这个活动能不能真的生效」：规则不完整或已经过期的活动
		// 上线了也不会命中任何一单，而运营会以为它在跑。
		if status == 0 && f.Status == 1 {
			if err := validateGoLive(f, tiers, skus, s.now()); err != nil {
				return err
			}
		}
		if err := checkPromotionTargets(ctx, tx, f, scopes, skus); err != nil {
			return err
		}
		if err := tx.AdminUpdatePromotion(ctx, id, f); err != nil {
			return mapPromotionRepoErr(err)
		}
		if err := writePromotionRules(ctx, tx, id, p.Rules); err != nil {
			return err
		}
		out, err = s.view(ctx, tx, id)
		return err
	})
	return out, err
}

func derefRules(r PromotionRules) ([]repository.PromotionTier, []repository.CouponScopeInput,
	[]repository.PromotionSkuInput) {
	var tiers []repository.PromotionTier
	var scopes []repository.CouponScopeInput
	var skus []repository.PromotionSkuInput
	if r.Tiers != nil {
		tiers = *r.Tiers
	}
	if r.Scopes != nil {
		scopes = *r.Scopes
	}
	if r.Skus != nil {
		skus = *r.Skus
	}
	return tiers, scopes, skus
}

// writePromotionRules 把传了的那几组整组替换进库。没传的不动。
func writePromotionRules(ctx context.Context, tx repository.Tx, id int64, r PromotionRules) error {
	if r.Tiers != nil {
		tiers := append([]repository.PromotionTier(nil), (*r.Tiers)...)
		sort.Slice(tiers, func(i, j int) bool { return tiers[i].Threshold < tiers[j].Threshold })
		if err := tx.ReplacePromotionTiers(ctx, id, tiers); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	if r.Scopes != nil {
		if err := tx.ReplacePromotionScopes(ctx, id, *r.Scopes); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	if r.Skus != nil {
		if err := tx.ReplacePromotionSkus(ctx, id, *r.Skus); err != nil {
			return mapPromotionRepoErr(err)
		}
	}
	return nil
}

func promotionFieldsOf(p repository.Promotion) repository.PromotionFields {
	return repository.PromotionFields{
		Name: p.Name, Type: p.Type, ThresholdUnit: p.ThresholdUnit, StackWithCoupon: p.StackWithCoupon,
		GiftTemplateID: p.GiftTemplateID, StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: p.Status,
	}
}

func scopeInputsOf(cur []repository.CouponScope) []repository.CouponScopeInput {
	out := make([]repository.CouponScopeInput, 0, len(cur))
	for _, c := range cur {
		out = append(out, repository.CouponScopeInput{ScopeType: c.ScopeType, TargetID: c.TargetID, Include: c.Include})
	}
	return out
}

func skuInputsOf(cur []repository.PromotionSku) []repository.PromotionSkuInput {
	out := make([]repository.PromotionSkuInput, 0, len(cur))
	for _, c := range cur {
		out = append(out, repository.PromotionSkuInput{
			SKUID: c.SKUID, PromoPriceCents: c.PromoPriceCents, DiscountRate: c.DiscountRate,
			PerUserLimit: c.PerUserLimit, StockQty: c.StockQty,
		})
	}
	return out
}

// checkSoldSkusKept：卖出过的 SKU 不能移出活动，配额不能低于已售数。
// 那部分配额已经兑现给了买家；移除它会让关单时放不回配额（releasePromotionLine 的那条日志）。
func checkSoldSkusKept(cur []repository.PromotionSku, next []repository.PromotionSkuInput) error {
	byID := map[int64]repository.PromotionSkuInput{}
	for _, n := range next {
		byID[n.SKUID] = n
	}
	for _, c := range cur {
		if c.SoldQty == 0 {
			continue
		}
		n, ok := byID[c.SKUID]
		if !ok {
			return fmt.Errorf("%w: sku %d 已按活动价卖出 %d 件，不能移出活动", ErrPromotionBadRequest, c.SKUID, c.SoldQty)
		}
		if n.StockQty > 0 && n.StockQty < c.SoldQty {
			return fmt.Errorf("%w: sku %d 已卖出 %d 件，配额不能改成 %d", ErrPromotionBadRequest,
				c.SKUID, c.SoldQty, n.StockQty)
		}
	}
	return nil
}

// validatePromotion 在进库之前把 chk_promotion_* 与跨表的形状规则用人话说一遍。
// 数据库的 CHECK 是最后一道；这里先挡，是为了让 422 的 detail 说得出是哪一条。
// 阶梯与活动类型的对应（满减配减免额、满折配折扣率）是跨表的，CHECK 管不到，只在这里。
func validatePromotion(f repository.PromotionFields, tiers []repository.PromotionTier,
	scopes []repository.CouponScopeInput, skus []repository.PromotionSkuInput) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrPromotionBadRequest, fmt.Sprintf(format, args...))
	}
	if f.Name == "" || utf8.RuneCountInString(f.Name) > maxPromotionNameRunes {
		return bad("name 必须是 1 到 %d 个字", maxPromotionNameRunes)
	}
	if f.StartsAt.IsZero() || f.EndsAt.IsZero() || !f.EndsAt.After(f.StartsAt) {
		return bad("ends_at 必须晚于 starts_at")
	}
	if len(tiers) > maxPromotionTiers || len(scopes) > maxPromotionScopes || len(skus) > maxPromotionSkus {
		return bad("阶梯至多 %d 档、范围至多 %d 条、活动商品至多 %d 个",
			maxPromotionTiers, maxPromotionScopes, maxPromotionSkus)
	}
	switch f.Type {
	case repository.PromoFullReduction, repository.PromoFullDiscount:
		if f.ThresholdUnit != repository.ThresholdByAmount && f.ThresholdUnit != repository.ThresholdByQty {
			return bad("满减满折要给 threshold_unit：1 金额（分）或 2 件数")
		}
		if len(skus) > 0 || f.GiftTemplateID != nil {
			return bad("满减满折不带 skus / gift_coupon_template_id")
		}
		seen := map[int64]bool{}
		for i, t := range tiers {
			if t.Threshold <= 0 {
				return bad("第 %d 档：threshold 必须大于 0", i+1)
			}
			if seen[t.Threshold] {
				return bad("第 %d 档：门槛 %d 重复了", i+1, t.Threshold)
			}
			seen[t.Threshold] = true
			if f.Type == repository.PromoFullReduction {
				if t.DiscountCents <= 0 || t.DiscountRate != 0 {
					return bad("第 %d 档：满减填 discount_cents（> 0），discount_rate 为 0", i+1)
				}
				if f.ThresholdUnit == repository.ThresholdByAmount && t.Threshold < t.DiscountCents {
					return bad("第 %d 档：「满 %s 减 %s」减的比门槛还多，是配置错误",
						i+1, yuan(t.Threshold), yuan(t.DiscountCents))
				}
			} else if t.DiscountRate < 1 || t.DiscountRate > 999 || t.DiscountCents != 0 {
				return bad("第 %d 档：满折填 discount_rate（千分比 1 到 999，900 = 9 折），discount_cents 为 0", i+1)
			}
		}
		// 阶梯越高、优惠要越大：「满 100 减 20、满 200 减 10」会让多买的人少减钱。
		sorted := append([]repository.PromotionTier(nil), tiers...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Threshold < sorted[j].Threshold })
		for i := 1; i < len(sorted); i++ {
			a, b := sorted[i-1], sorted[i]
			if f.Type == repository.PromoFullReduction && b.DiscountCents <= a.DiscountCents {
				return bad("门槛更高的一档（%d）减免必须更多", b.Threshold)
			}
			if f.Type == repository.PromoFullDiscount && b.DiscountRate >= a.DiscountRate {
				return bad("门槛更高的一档（%d）折扣必须更低", b.Threshold)
			}
		}
	case repository.PromoLimitedPrice, repository.PromoFlashSale:
		if f.ThresholdUnit != 0 || len(tiers) > 0 || f.GiftTemplateID != nil {
			return bad("限时折扣 / 秒杀不带 threshold_unit / tiers / gift_coupon_template_id")
		}
		seen := map[int64]bool{}
		for i, s := range skus {
			if s.SKUID <= 0 {
				return bad("第 %d 个活动商品：sku_id 必须为正", i+1)
			}
			if seen[s.SKUID] {
				return bad("第 %d 个活动商品：sku %d 出现了两次", i+1, s.SKUID)
			}
			seen[s.SKUID] = true
			if (s.PromoPriceCents > 0) == (s.DiscountRate > 0) {
				return bad("sku %d：promo_price_cents 与 discount_rate 二选一（另一个为 0）", s.SKUID)
			}
			if s.PromoPriceCents < 0 || s.DiscountRate < 0 || s.DiscountRate > 999 {
				return bad("sku %d：特价不能为负，折扣是千分比 1 到 999", s.SKUID)
			}
			if s.PerUserLimit < 0 || s.PerUserLimit > maxLineQuantity {
				return bad("sku %d：per_user_limit 在 0 到 %d 之间（0 不限）", s.SKUID, maxLineQuantity)
			}
			if f.Type == repository.PromoFlashSale && s.StockQty <= 0 {
				return bad("sku %d：秒杀必须给活动配额 stock_qty（> 0）", s.SKUID)
			}
			if f.Type == repository.PromoLimitedPrice && s.StockQty != 0 {
				return bad("sku %d：限时折扣不限配额，stock_qty 为 0；要限量请建秒杀", s.SKUID)
			}
		}
		// 商品已由 skus 点名，商品维度的范围没有意义；只认门店维度（5 大区、6 门店）。
		for i, sc := range scopes {
			if sc.ScopeType != repository.ScopeRegion && sc.ScopeType != repository.ScopeStore {
				return bad("第 %d 条范围：限时折扣 / 秒杀只认大区（5）与门店（6）", i+1)
			}
		}
	case repository.PromoNewBuyerGift:
		if f.ThresholdUnit != 0 || len(tiers) > 0 || len(skus) > 0 || len(scopes) > 0 {
			return bad("新人礼只带 gift_coupon_template_id")
		}
		if f.GiftTemplateID == nil || *f.GiftTemplateID <= 0 {
			return bad("新人礼要给 gift_coupon_template_id")
		}
	default:
		return bad("promotion_type 只能是 1 满减 / 2 满折 / 3 限时折扣 / 4 秒杀 / 5 新人礼")
	}
	return validateScopeShapes(scopes)
}

// validateGoLive：上线时规则要完整、活动还没结束。
func validateGoLive(f repository.PromotionFields, tiers []repository.PromotionTier,
	skus []repository.PromotionSkuInput, now time.Time) error {
	switch {
	case (f.Type == repository.PromoFullReduction || f.Type == repository.PromoFullDiscount) && len(tiers) == 0:
		return fmt.Errorf("%w: 满减满折至少要有一档才能上线", ErrPromotionBadRequest)
	case (f.Type == repository.PromoLimitedPrice || f.Type == repository.PromoFlashSale) && len(skus) == 0:
		return fmt.Errorf("%w: 限时折扣 / 秒杀至少要有一个活动商品才能上线", ErrPromotionBadRequest)
	case !f.EndsAt.After(now):
		return fmt.Errorf("%w: 活动已经结束（ends_at 在过去），上线也不会生效", ErrPromotionBadRequest)
	}
	return nil
}

// validateScopeShapes 是范围的形状规则，与券的适用范围同一套（admin_coupon.go 的 SetScopes）。
func validateScopeShapes(in []repository.CouponScopeInput) error {
	type key struct {
		t  int16
		id int64
	}
	seen := map[key]bool{}
	for i, sc := range in {
		switch {
		case sc.ScopeType == repository.ScopeAll:
			if sc.TargetID != nil || !sc.Include {
				return fmt.Errorf("%w: 第 %d 条范围：全场规则不带 target_id，也不能是排除", ErrPromotionBadRequest, i+1)
			}
		case sc.ScopeType >= repository.ScopeCategory && sc.ScopeType <= repository.ScopeStore:
			if sc.TargetID == nil || *sc.TargetID <= 0 {
				return fmt.Errorf("%w: 第 %d 条范围：scope_type=%d 必须给正的 target_id",
					ErrPromotionBadRequest, i+1, sc.ScopeType)
			}
		default:
			return fmt.Errorf("%w: 第 %d 条范围：scope_type 只能是 1 到 6", ErrPromotionBadRequest, i+1)
		}
		k := key{t: sc.ScopeType}
		if sc.TargetID != nil {
			k.id = *sc.TargetID
		}
		if seen[k] {
			return fmt.Errorf("%w: 第 %d 条范围：同一个目标出现了两次", ErrPromotionBadRequest, i+1)
		}
		seen[k] = true
	}
	return nil
}

// checkPromotionTargets：范围目标、活动商品、新人礼的券模板都在当前租户里逐个查一遍
// （RLS 之下别家的 id 与不存在的同形）。多态的 target_id 与 promotion_skus 以外的引用，
// 外键管不到的那部分在这里。
func checkPromotionTargets(ctx context.Context, tx repository.Tx, f repository.PromotionFields,
	scopes []repository.CouponScopeInput, skus []repository.PromotionSkuInput) error {
	byKind := map[repository.ScopeTargetKind][]int64{}
	for _, sc := range scopes {
		if kind, ok := targetKindOf(sc.ScopeType); ok {
			byKind[kind] = append(byKind[kind], *sc.TargetID)
		}
	}
	for kind, ids := range byKind {
		live, err := tx.LiveTargetIDs(ctx, kind, ids)
		if err != nil {
			return err
		}
		if missing := missingIDs(ids, live); len(missing) > 0 {
			return fmt.Errorf("%w: %s %v 在本店查不到（不存在、已删除或属于别家店）",
				ErrPromotionBadRequest, targetKindName(kind), missing)
		}
	}
	if len(skus) > 0 {
		ids := make([]int64, 0, len(skus))
		for _, s := range skus {
			ids = append(ids, s.SKUID)
		}
		live, err := tx.LiveSkuIDs(ctx, ids)
		if err != nil {
			return err
		}
		if missing := missingIDs(ids, live); len(missing) > 0 {
			return fmt.Errorf("%w: SKU %v 在本店查不到", ErrPromotionBadRequest, missing)
		}
	}
	if f.GiftTemplateID != nil {
		live, err := tx.LiveCouponTemplateIDs(ctx, []int64{*f.GiftTemplateID})
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return fmt.Errorf("%w: 券模板 %d 在本店查不到", ErrPromotionBadRequest, *f.GiftTemplateID)
		}
	}
	return nil
}

func missingIDs(want, live []int64) []int64 {
	ok := map[int64]bool{}
	for _, v := range live {
		ok[v] = true
	}
	var out []int64
	for _, v := range want {
		if !ok[v] {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func mapPromotionRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrPromotionRuleViolation):
		return fmt.Errorf("%w: %v", ErrPromotionBadRequest, err)
	case errors.Is(err, repository.ErrPromotionNotFound):
		return fmt.Errorf("%w: %v", ErrPromotionNotFound, err)
	default:
		return err
	}
}
