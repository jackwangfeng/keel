package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// M10 的四种提案（docs/AI经营-M10M11设计.md §1）。每种一对函数：Propose*（MCP 工具，AI 员工提，不执行）
// 与 exec*（批准后以 AI 员工的身份执行，调的是后台接口同一个 service 函数，幂等键固定为提案 id）。
//
// 上限是硬的、在提案层就拒：营销让利、商品文案、售后退款都是真金白银或对外可见的东西，
// 一个写错的提案不该连进待处理队列都进得去。自动执行（M11）另有店长设的、更紧的上限。

const (
	ProposalKindFlashPrice     = "flash_price"
	ProposalKindCoupon         = "coupon"
	ProposalKindProductCopy    = "product_copy"
	ProposalKindRefundDecision = "refund_decision"

	flashMaxSKUs        = 20
	flashMinRate        = 500 // 千分比：最多打五折
	flashMaxDuration    = 14 * 24 * time.Hour
	couponMaxDiscount   = 100_00
	couponMaxTotal      = 10000
	couponMaxValidDays  = 90
	productTitleMaxRune = 60
	productSubMaxRune   = 120
)

// ProposalMeta 是每种提案都有的三段话。
type ProposalMeta struct {
	Evidence       string
	ExpectedImpact string
}

func (m ProposalMeta) check() error {
	if err := checkProposalText("evidence", m.Evidence, 10, 8000); err != nil {
		return err
	}
	return checkProposalText("expected_impact", m.ExpectedImpact, 0, 2000)
}

// requireAgent：提案只能由 AI 员工提。
func requireAgent(ctx context.Context) (auth.StaffIdentity, error) {
	id, err := requireStaff(ctx)
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	if !id.IsAgent() {
		return auth.StaffIdentity{}, ErrAgentOnly
	}
	return id, nil
}

func badProposal(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrProposalBadRequest}, a...)...)
}

// ---------------------------------------------------------------------------
// flash_price：限时折扣
// ---------------------------------------------------------------------------

// FlashPriceItem 是限时折扣里的一个 SKU：折扣率是千分比（850 = 85 折），500–999。
type FlashPriceItem struct {
	SKUID        int64 `json:"sku_id"`
	DiscountRate int16 `json:"discount_rate"`
}

// FlashPricePayload 是 flash_price 提案的执行参数。StoreID 非空时活动只在这家店生效（范围：门店）。
type FlashPricePayload struct {
	Name     string           `json:"name"`
	StoreID  *int64           `json:"store_id,omitempty"`
	Items    []FlashPriceItem `json:"items"`
	StartsAt time.Time        `json:"starts_at"`
	EndsAt   time.Time        `json:"ends_at"`
}

// ProposeFlashPrice 是 MCP 工具 propose_flash_price。营销是全店的：要全店范围的 AI 员工。
func (s *AgentProposalService) ProposeFlashPrice(ctx context.Context, in FlashPricePayload,
	meta ProposalMeta) (repository.AgentProposal, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AgentProposal{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 30 {
		return repository.AgentProposal{}, badProposal("name 取 1–30 字")
	}
	if len(in.Items) < 1 || len(in.Items) > flashMaxSKUs {
		return repository.AgentProposal{}, badProposal("items 取 1–%d 个 SKU", flashMaxSKUs)
	}
	now := time.Now()
	if in.StartsAt.Before(now.Add(-10*time.Minute)) || in.StartsAt.After(now.Add(7*24*time.Hour)) {
		return repository.AgentProposal{}, badProposal("starts_at 要在现在到 7 天之内")
	}
	if !in.EndsAt.After(in.StartsAt) || in.EndsAt.Sub(in.StartsAt) > flashMaxDuration {
		return repository.AgentProposal{}, badProposal("ends_at 要晚于 starts_at，且活动至多 14 天")
	}
	ids := make([]int64, 0, len(in.Items))
	minRate := int16(1000)
	for _, it := range in.Items {
		if it.DiscountRate < flashMinRate || it.DiscountRate > 999 {
			return repository.AgentProposal{}, badProposal("sku %d：discount_rate 取 %d–999（千分比，最多打五折）",
				it.SKUID, flashMinRate)
		}
		if slices.Contains(ids, it.SKUID) {
			return repository.AgentProposal{}, badProposal("sku %d 出现了两次", it.SKUID)
		}
		ids = append(ids, it.SKUID)
		minRate = min(minRate, it.DiscountRate)
	}
	if err := meta.check(); err != nil {
		return repository.AgentProposal{}, err
	}
	slices.Sort(ids)
	key := "flash:" + joinIDs(ids)
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		for _, sid := range ids {
			if _, err := tx.AdminFindSKU(ctx, sid); err != nil {
				return badProposal("sku %d 不存在", sid)
			}
		}
		where := "全店"
		if in.StoreID != nil {
			st, err := tx.FindStore(ctx, *in.StoreID)
			if err != nil {
				return badProposal("store_id=%d 不存在", *in.StoreID)
			}
			where = st.Name
		}
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		title := fmt.Sprintf("限时折扣「%s」（%s）：%d 个 SKU %s 起，%s–%s", in.Name, where, len(ids),
			rateText(minRate)+" 折", in.StartsAt.Format("01-02 15:04"), in.EndsAt.Format("01-02 15:04"))
		out, err = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID,
			Kind: ProposalKindFlashPrice, TargetKey: key, Payload: payload, Title: title,
			Evidence: strings.TrimSpace(meta.Evidence), ExpectedImpact: strings.TrimSpace(meta.ExpectedImpact)})
		return err
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, v := range ids {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return strings.Join(parts, ",")
}

func (s *AgentProposalService) execFlashPrice(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	if s.promos == nil {
		return ProposalResult{}, errors.New("营销活动服务没有接上")
	}
	var pl FlashPricePayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return ProposalResult{}, err
	}
	skus := make([]repository.PromotionSkuInput, 0, len(pl.Items))
	for _, it := range pl.Items {
		skus = append(skus, repository.PromotionSkuInput{SKUID: it.SKUID, DiscountRate: it.DiscountRate})
	}
	rules := PromotionRules{Skus: &skus}
	if pl.StoreID != nil {
		sc := []repository.CouponScopeInput{{ScopeType: repository.ScopeStore, TargetID: pl.StoreID, Include: true}}
		rules.Scopes = &sc
	}
	// 批得晚了、开始时间已过：从现在开始，结束时间不变（结束也过了就执行失败，由活动校验报出来）。
	start := pl.StartsAt
	if now := time.Now(); start.Before(now) {
		start = now
	}
	key := proposalIdemKey(p.ID)
	v, _, err := s.promos.Create(ctx, PromotionInput{Name: pl.Name, Type: repository.PromoLimitedPrice,
		StartsAt: start, EndsAt: pl.EndsAt, Rules: rules}, key)
	if err != nil {
		return ProposalResult{}, err
	}
	online := int16(1)
	if _, err := s.promos.Update(ctx, v.Promotion.ID, PromotionPatch{Status: &online}); err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Detail: map[string]any{"promotion_id": v.Promotion.ID}}, nil
}

// ---------------------------------------------------------------------------
// coupon：券模板
// ---------------------------------------------------------------------------

// CouponPayload 是 coupon 提案的执行参数：领取后 valid_days 天内有效的一张券。
// coupon_type：1 满减（threshold + discount_cents）/ 2 折扣（threshold + discount_rate 千分比 + 封顶）/ 3 立减。
type CouponPayload struct {
	Name             string `json:"name"`
	CouponType       int16  `json:"coupon_type"`
	ThresholdCents   int64  `json:"threshold_cents"`
	DiscountCents    int64  `json:"discount_cents"`
	DiscountRate     int16  `json:"discount_rate"`
	MaxDiscountCents int64  `json:"max_discount_cents"`
	ValidDays        int32  `json:"valid_days,omitempty"`
	// ValidStartAt / ValidEndAt 是固定可用时段（与 ValidDays 二选一；2026-09-28 起，之前只能「领后 N 天」，
	// 店长要「国庆 10/1–10/7 可用」时 AI 店长做不到）。
	ValidStartAt *time.Time `json:"valid_start_at,omitempty"`
	ValidEndAt   *time.Time `json:"valid_end_at,omitempty"`
	TotalCount   int32      `json:"total_count"`
	PerUserLimit int32      `json:"per_user_limit"`
	Claimable    bool       `json:"claimable"`
}

// ProposeCoupon 是 MCP 工具 propose_coupon。
func (s *AgentProposalService) ProposeCoupon(ctx context.Context, in CouponPayload,
	meta ProposalMeta) (repository.AgentProposal, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AgentProposal{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 30 {
		return repository.AgentProposal{}, badProposal("name 取 1–30 字")
	}
	switch in.CouponType {
	case 1, 3:
		if in.DiscountCents < 1 || in.DiscountCents > couponMaxDiscount {
			return repository.AgentProposal{}, badProposal("discount_cents 取 1–%d（面额至多 100 元）", couponMaxDiscount)
		}
	case 2:
		if in.DiscountRate < flashMinRate || in.DiscountRate > 999 {
			return repository.AgentProposal{}, badProposal("discount_rate 取 %d–999（千分比，最多打五折）", flashMinRate)
		}
		if in.MaxDiscountCents < 1 || in.MaxDiscountCents > couponMaxDiscount {
			return repository.AgentProposal{}, badProposal("折扣券必须封顶：max_discount_cents 取 1–%d", couponMaxDiscount)
		}
	default:
		return repository.AgentProposal{}, badProposal("coupon_type 取 1 满减 / 2 折扣 / 3 立减")
	}
	validText, err := checkCouponValidity(in, time.Now())
	if err != nil {
		return repository.AgentProposal{}, err
	}
	// 与后台建券同一套规则（执行时就是调它建券）：提案阶段放过、批准时才被拒的券，
	// 之前会掉进执行失败（更早时卡死在 15）—— 2026-09-28 破坏性测试：满 100 减 200、负门槛、立减券带门槛都提得出来。
	if err := validateCouponTemplate(repository.CouponTemplateFields{Name: in.Name, CouponType: in.CouponType,
		ThresholdCents: in.ThresholdCents, DiscountCents: in.DiscountCents, DiscountRate: in.DiscountRate,
		MaxDiscountCents: in.MaxDiscountCents, ValidMode: couponValidMode(in), ValidStartAt: in.ValidStartAt,
		ValidEndAt: in.ValidEndAt, ValidDays: in.ValidDays, TotalCount: in.TotalCount, PerUserLimit: in.PerUserLimit,
		Claimable: in.Claimable, Status: 1}, 0); err != nil {
		return repository.AgentProposal{}, badProposal("%s", strings.TrimPrefix(err.Error(), ErrCouponBadRequest.Error()+": "))
	}
	if in.TotalCount < 1 || in.TotalCount > couponMaxTotal {
		return repository.AgentProposal{}, badProposal("total_count 取 1–%d", couponMaxTotal)
	}
	if in.PerUserLimit < 1 || in.PerUserLimit > 5 {
		return repository.AgentProposal{}, badProposal("per_user_limit 取 1–5")
	}
	if err := meta.check(); err != nil {
		return repository.AgentProposal{}, err
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	var what string
	switch in.CouponType {
	case 1:
		what = fmt.Sprintf("满 %s 减 %s", yuan(in.ThresholdCents), yuan(in.DiscountCents))
	case 2:
		what = fmt.Sprintf("满 %s 打 %s（封顶 %s）", yuan(in.ThresholdCents), rateText(in.DiscountRate)+" 折", yuan(in.MaxDiscountCents))
	default:
		what = "立减 " + yuan(in.DiscountCents)
	}
	title := fmt.Sprintf("发券「%s」：%s，%d 张，%s", in.Name, what, in.TotalCount, validText)
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out, e = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID, Kind: ProposalKindCoupon,
			TargetKey: "coupon:" + in.Name, Payload: payload, Title: title,
			Evidence: strings.TrimSpace(meta.Evidence), ExpectedImpact: strings.TrimSpace(meta.ExpectedImpact)})
		return e
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

// checkCouponValidity 校验发券提案的有效期（valid_days 与固定时段二选一），返回标题里那半句。
// 固定时段按提案里给的时区显示日期（AI 员工拿到的时间都是店铺时区，它照抄回来）。
func checkCouponValidity(in CouponPayload, now time.Time) (string, error) {
	fixed := in.ValidStartAt != nil || in.ValidEndAt != nil
	switch {
	case fixed && in.ValidDays != 0:
		return "", badProposal("有效期二选一：valid_days，或 valid_start_at + valid_end_at")
	case fixed:
		if in.ValidStartAt == nil || in.ValidEndAt == nil {
			return "", badProposal("固定时段要同时给 valid_start_at 与 valid_end_at")
		}
		start, end := *in.ValidStartAt, *in.ValidEndAt
		if !end.After(start) {
			return "", badProposal("valid_end_at 必须晚于 valid_start_at")
		}
		if !end.After(now) {
			return "", badProposal("valid_end_at 已经过去了")
		}
		if end.Sub(start) > couponMaxValidDays*24*time.Hour || end.Sub(now) > couponMaxValidDays*24*time.Hour {
			return "", badProposal("固定时段至多 %d 天，且要在 %d 天内结束", couponMaxValidDays, couponMaxValidDays)
		}
		last := end.Add(-time.Nanosecond) // 结束不含：10-08T00:00 结束即「用到 10-07」
		return fmt.Sprintf("%s 至 %s 可用", start.Format("01-02 15:04"), last.Format("01-02 15:04")), nil
	default:
		if in.ValidDays < 1 || in.ValidDays > couponMaxValidDays {
			return "", badProposal("valid_days 取 1–%d（或改用 valid_start_at + valid_end_at 固定时段）", couponMaxValidDays)
		}
		return fmt.Sprintf("领后 %d 天有效", in.ValidDays), nil
	}
}

// couponValidMode：1 绝对时间（固定时段）/ 2 领取后 N 天（券模板的 valid_mode）。
func couponValidMode(pl CouponPayload) int16 {
	if pl.ValidStartAt != nil {
		return 1
	}
	return 2
}

func (s *AgentProposalService) execCoupon(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	if s.coupons == nil {
		return ProposalResult{}, errors.New("券服务没有接上")
	}
	var pl CouponPayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return ProposalResult{}, err
	}
	v, _, err := s.coupons.Create(ctx, CouponTemplateInput{Name: pl.Name, CouponType: pl.CouponType,
		ThresholdCents: pl.ThresholdCents, DiscountCents: pl.DiscountCents, DiscountRate: pl.DiscountRate,
		MaxDiscountCents: pl.MaxDiscountCents, ValidMode: couponValidMode(pl), ValidStartAt: pl.ValidStartAt,
		ValidEndAt: pl.ValidEndAt, ValidDays: pl.ValidDays, TotalCount: pl.TotalCount,
		PerUserLimit: pl.PerUserLimit, Claimable: pl.Claimable}, proposalIdemKey(p.ID))
	if err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Detail: map[string]any{"coupon_template_id": v.Template.Rule.TemplateID}}, nil
}

// ---------------------------------------------------------------------------
// product_copy：改标题 / 副标题
// ---------------------------------------------------------------------------

// ProductCopyPayload 是 product_copy 提案的执行参数；Before* 是提的时候的原文（给人对照，也进结果）。
type ProductCopyPayload struct {
	ProductID      int64   `json:"product_id"`
	Title          *string `json:"title,omitempty"`
	Subtitle       *string `json:"subtitle,omitempty"`
	BeforeTitle    string  `json:"before_title"`
	BeforeSubtitle *string `json:"before_subtitle,omitempty"`
}

// ProposeProductCopy 是 MCP 工具 propose_product_copy。商品是全店的：要全店范围的 AI 员工。
func (s *AgentProposalService) ProposeProductCopy(ctx context.Context, productID int64, title, subtitle *string,
	meta ProposalMeta) (repository.AgentProposal, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	if _, err := requireMerchantWide(ctx); err != nil {
		return repository.AgentProposal{}, err
	}
	if title == nil && subtitle == nil {
		return repository.AgentProposal{}, badProposal("title 与 subtitle 至少给一个")
	}
	if title != nil {
		t := strings.TrimSpace(*title)
		if n := utf8.RuneCountInString(t); n < 1 || n > productTitleMaxRune {
			return repository.AgentProposal{}, badProposal("title 取 1–%d 字", productTitleMaxRune)
		}
		title = &t
	}
	if subtitle != nil {
		t := strings.TrimSpace(*subtitle)
		if utf8.RuneCountInString(t) > productSubMaxRune {
			return repository.AgentProposal{}, badProposal("subtitle 至多 %d 字", productSubMaxRune)
		}
		subtitle = &t
	}
	if err := meta.check(); err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		prod, err := tx.AdminFindProduct(ctx, productID)
		if err != nil {
			return badProposal("product_id=%d 不存在", productID)
		}
		pl := ProductCopyPayload{ProductID: productID, Title: title, Subtitle: subtitle, BeforeTitle: prod.Title,
			BeforeSubtitle: prod.Subtitle}
		payload, err := json.Marshal(pl)
		if err != nil {
			return err
		}
		t := "改文案：「" + prod.Title + "」"
		if title != nil {
			t += " → 「" + *title + "」"
		} else {
			t += " 改副标题"
		}
		out, err = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID,
			Kind: ProposalKindProductCopy, TargetKey: "product:" + strconv.FormatInt(productID, 10), Payload: payload,
			Title: truncRunes(t, 200), Evidence: strings.TrimSpace(meta.Evidence),
			ExpectedImpact: strings.TrimSpace(meta.ExpectedImpact)})
		return err
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (s *AgentProposalService) execProductCopy(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	if s.catalog == nil {
		return ProposalResult{}, errors.New("商品服务没有接上")
	}
	var pl ProductCopyPayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return ProposalResult{}, err
	}
	after, err := s.catalog.UpdateProduct(ctx, pl.ProductID, repository.ProductPatch{Title: pl.Title, Subtitle: pl.Subtitle})
	if err != nil {
		return ProposalResult{}, err
	}
	d := map[string]any{"product_id": pl.ProductID, "before_title": pl.BeforeTitle, "after_title": after.Title}
	if after.Subtitle != nil {
		d["after_subtitle"] = *after.Subtitle
	}
	return ProposalResult{Detail: d}, nil
}

// ---------------------------------------------------------------------------
// refund_decision：售后审核
// ---------------------------------------------------------------------------

// RefundDecisionPayload 是 refund_decision 提案的执行参数。Action：approve / reject（驳回必须带理由，
// 理由会给买家看）。
type RefundDecisionPayload struct {
	RefundNo     string  `json:"refund_no"`
	Action       string  `json:"action"`
	RejectReason *string `json:"reject_reason,omitempty"`
	AmountCents  int64   `json:"amount_cents"`
}

// ProposeRefundDecision 是 MCP 工具 propose_refund_decision：按订单的履约门店判权（与人审核同一个判据）。
func (s *AgentProposalService) ProposeRefundDecision(ctx context.Context, refundNo, action string, rejectReason *string,
	meta ProposalMeta) (repository.AgentProposal, error) {
	id, err := requireAgent(ctx)
	if err != nil {
		return repository.AgentProposal{}, err
	}
	switch action {
	case "approve":
		rejectReason = nil
	case "reject":
		if rejectReason == nil || strings.TrimSpace(*rejectReason) == "" {
			return repository.AgentProposal{}, badProposal("驳回必须给 reject_reason（买家看得到）")
		}
		if err := checkProposalText("reject_reason", *rejectReason, 1, 200); err != nil {
			return repository.AgentProposal{}, err
		}
		r := strings.TrimSpace(*rejectReason)
		rejectReason = &r
	default:
		return repository.AgentProposal{}, badProposal("action 取 approve / reject")
	}
	if err := meta.check(); err != nil {
		return repository.AgentProposal{}, err
	}
	var out repository.AgentProposal
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		rf, err := tx.FindRefundByNo(ctx, refundNo)
		if err != nil {
			return badProposal("refund_no=%s 不存在", refundNo)
		}
		if _, err := authorizeStore(ctx, tx, rf.StoreID, storeOperate); err != nil {
			return err
		}
		if rf.Status != repository.RefundPending {
			return badProposal("售后单 %s 不在待审核状态（status=%d）", refundNo, rf.Status)
		}
		pl := RefundDecisionPayload{RefundNo: refundNo, Action: action, RejectReason: rejectReason,
			AmountCents: rf.AmountCents}
		payload, err := json.Marshal(pl)
		if err != nil {
			return err
		}
		verb := "同意"
		if action == "reject" {
			verb = "驳回"
		}
		storeID := rf.StoreID
		out, err = insertProposal(ctx, tx, repository.NewAgentProposal{AgentStaffID: id.StaffID,
			Kind: ProposalKindRefundDecision, StoreID: &storeID, TargetKey: "refund:" + refundNo, Payload: payload,
			Title:    fmt.Sprintf("售后 %s：建议%s（%s）", refundNo, verb, yuan(rf.AmountCents)),
			Evidence: strings.TrimSpace(meta.Evidence), ExpectedImpact: strings.TrimSpace(meta.ExpectedImpact)})
		return err
	})
	return s.afterPropose(ctx, out, mapProposalErr(err))
}

func (s *AgentProposalService) execRefundDecision(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	if s.refunds == nil {
		return ProposalResult{}, errors.New("售后服务没有接上")
	}
	var pl RefundDecisionPayload
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		return ProposalResult{}, err
	}
	rf, _, err := s.refunds.Audit(ctx, pl.RefundNo, AuditRequest{Action: pl.Action, RejectReason: pl.RejectReason},
		proposalIdemKey(p.ID))
	if err != nil {
		return ProposalResult{}, err
	}
	return ProposalResult{Detail: map[string]any{"refund_no": rf.RefundNo, "refund_status": rf.Status}}, nil
}

// ---------------------------------------------------------------------------
// 执行分派
// ---------------------------------------------------------------------------

func proposalIdemKey(id int64) string { return "agent-proposal-" + strconv.FormatInt(id, 10) }

// execute 以 AI 员工的身份（ctx 里是它）执行一条已认领的提案。
func (s *AgentProposalService) execute(ctx context.Context, p repository.AgentProposal) (ProposalResult, error) {
	switch p.Kind {
	case ProposalKindInventoryAdjust:
		var pl InventoryAdjustPayload
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			return ProposalResult{}, err
		}
		reason := "AI 提案 #" + strconv.FormatInt(p.ID, 10) + "：" + pl.Reason
		after, _, err := adjustInventory(ctx, s.repo, s.inv, pl.StoreID, pl.SKUID,
			InventoryAdjustInput{Delta: pl.Delta, Reason: &reason}, proposalIdemKey(p.ID))
		if err != nil {
			return ProposalResult{}, err
		}
		before := after.AvailableQty - pl.Delta
		a := after.AvailableQty
		return ProposalResult{BeforeAvailable: &before, AfterAvailable: &a}, nil
	case ProposalKindFlashPrice:
		return s.execFlashPrice(ctx, p)
	case ProposalKindCoupon:
		return s.execCoupon(ctx, p)
	case ProposalKindProductCopy:
		return s.execProductCopy(ctx, p)
	case ProposalKindRefundDecision:
		return s.execRefundDecision(ctx, p)
	}
	return ProposalResult{}, fmt.Errorf("%w: 不认识的提案种类 %q", ErrProposalBadRequest, p.Kind)
}
