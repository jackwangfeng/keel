package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 券管理的后台：模板的列表 / 新建 / 修改 / 启停、适用范围、按手机号定向发放、
// 发放与核销统计。数据模型 §7。
//
// ===========================================================================
// 权限：本期只开放给商家级的管理员与操作员（role 1 或 2）
// ===========================================================================
//
// 券直接决定订单实付，是资金面。它和商品、基准价同一个判据：**全店范围**
// （requireMerchantWide，internal/service/authz.go）—— 管理员与操作员可以，
// 大区 / 门店管理员不行（券的适用范围可以跨大区，一个只管华北的人不该能发一张
// 全国通用的券）。平台级操作员切到某家店时也算全店范围，与商品、门店那些一致。
//
// 第一版这里是自己写的检查（平台级与 1、2 以外一律 403 staff-forbidden），理由是
// 分级权限那条线当时还没落地，先按最保守的口径放行。两条线合并之后换成统一的
// requireMerchantWide：同一件事只有一个判据、一种拒绝类型（role-forbidden），
// 并登记进 permission_test.go 那张「全部后台路由 × 各角色」的大表。
// 检查在业务层（requireCouponStaff），不在中间件：中间件只知道「有没有后台会话」，
// 「这个角色能不能碰券」是业务规则。

var (
	// ErrCouponBadRequest：请求体本身不成立（满 100 减 200、包邮券带了减免额、范围目标查不到……）。422。
	ErrCouponBadRequest = errors.New("券的配置不成立")

	// ErrCouponTemplateLocked：已经发出过券，却想改券面字段或范围。409。
	ErrCouponTemplateLocked = errors.New("这批券已经发出过，券面与范围不能再改")

	// ErrCouponTemplateDisabled：定向发放时模板已停用。409。
	ErrCouponTemplateDisabled = errors.New("券模板已停用")
)

const (
	scopeAdminCouponCreate = "admin.coupon_templates.create"
	scopeAdminCouponGrant  = "admin.coupon_templates.grants"

	maxCouponNameRunes = 60
	maxCouponValidDays = 3650
	maxCouponScopes    = 200
	maxGrantPhones     = 200
)

// AdminCouponService 实现券管理的七条接口。
type AdminCouponService struct {
	repo CouponRepository
	now  func() time.Time
}

func NewAdminCouponService(r CouponRepository) *AdminCouponService {
	return &AdminCouponService{repo: r, now: time.Now}
}

// requireCouponStaff 见文件头：就是 requireMerchantWide，留一个名字是为了让
// 这个文件里每个入口读起来都在说「这是券管理的判据」。
func requireCouponStaff(ctx context.Context) (auth.StaffIdentity, error) {
	return requireMerchantWide(ctx)
}

// AdminCouponView 是后台看到的一个模板：模板、范围、统计。
type AdminCouponView struct {
	Template repository.CouponTemplate
	Scopes   []repository.CouponScope
	Stats    repository.CouponStats
}

// AdminCouponPage 是 GET /admin/coupon-templates 的一页。
type AdminCouponPage struct {
	Items    []AdminCouponView
	Total    int64
	Page     int
	PageSize int
}

func (s *AdminCouponService) views(ctx context.Context, tx repository.Tx,
	tpls []repository.CouponTemplate) ([]AdminCouponView, error) {
	ids := make([]int64, 0, len(tpls))
	for _, t := range tpls {
		ids = append(ids, t.Rule.TemplateID)
	}
	scopes, err := tx.ListCouponScopes(ctx, ids)
	if err != nil {
		return nil, err
	}
	stats, err := tx.CouponTemplateStats(ctx, ids, s.now())
	if err != nil {
		return nil, err
	}
	out := make([]AdminCouponView, 0, len(tpls))
	for _, t := range tpls {
		out = append(out, AdminCouponView{
			Template: t, Scopes: scopes[t.Rule.TemplateID], Stats: stats[t.Rule.TemplateID],
		})
	}
	return out, nil
}

func (s *AdminCouponService) view(ctx context.Context, tx repository.Tx, id int64) (AdminCouponView, error) {
	t, err := tx.AdminGetCouponTemplate(ctx, id)
	if errors.Is(err, repository.ErrCouponTemplateNotFound) {
		return AdminCouponView{}, fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, id)
	}
	if err != nil {
		return AdminCouponView{}, err
	}
	vs, err := s.views(ctx, tx, []repository.CouponTemplate{t})
	if err != nil {
		return AdminCouponView{}, err
	}
	return vs[0], nil
}

// List 实现 GET /admin/coupon-templates。
func (s *AdminCouponService) List(ctx context.Context, status *int16, page, pageSize int) (AdminCouponPage, error) {
	if _, err := requireCouponStaff(ctx); err != nil {
		return AdminCouponPage{}, err
	}
	if status != nil && *status != 0 && *status != 1 {
		return AdminCouponPage{}, fmt.Errorf("%w: status 只能是 0 或 1", ErrCouponBadRequest)
	}
	page, pageSize = clampPaging(page, pageSize)
	out := AdminCouponPage{Items: []AdminCouponView{}, Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		tpls, total, err := tx.AdminListCouponTemplates(ctx, status,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		out.Total = total
		out.Items, err = s.views(ctx, tx, tpls)
		return err
	})
	if err != nil {
		return AdminCouponPage{}, err
	}
	return out, nil
}

// Get 实现 GET /admin/coupon-templates/{template_id}。
func (s *AdminCouponService) Get(ctx context.Context, id int64) (AdminCouponView, error) {
	if _, err := requireCouponStaff(ctx); err != nil {
		return AdminCouponView{}, err
	}
	var out AdminCouponView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		out, err = s.view(ctx, tx, id)
		return err
	})
	return out, err
}

// CouponTemplateInput 是新建请求。
type CouponTemplateInput struct {
	Name             string
	CouponType       int16
	ThresholdCents   int64
	DiscountCents    int64
	DiscountRate     int16
	MaxDiscountCents int64
	ValidMode        int16
	ValidStartAt     *time.Time
	ValidEndAt       *time.Time
	ValidDays        int32
	TotalCount       int32
	PerUserLimit     int32
	Claimable        bool
}

// Create 实现 POST /admin/coupon-templates。
func (s *AdminCouponService) Create(ctx context.Context, in CouponTemplateInput, idemKey string) (AdminCouponView, bool, error) {
	staff, err := requireCouponStaff(ctx)
	if err != nil {
		return AdminCouponView{}, false, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.PerUserLimit == 0 {
		in.PerUserLimit = 1
	}
	f := repository.CouponTemplateFields{
		Name: in.Name, CouponType: in.CouponType, ThresholdCents: in.ThresholdCents,
		DiscountCents: in.DiscountCents, DiscountRate: in.DiscountRate,
		MaxDiscountCents: in.MaxDiscountCents, ValidMode: in.ValidMode,
		ValidStartAt: in.ValidStartAt, ValidEndAt: in.ValidEndAt, ValidDays: in.ValidDays,
		TotalCount: in.TotalCount, PerUserLimit: in.PerUserLimit, Claimable: in.Claimable,
		Status: 1,
	}
	if err := validateCouponTemplate(f, 0); err != nil {
		return AdminCouponView{}, false, err
	}
	hash, err := adminRequestHash(nil, in)
	if err != nil {
		return AdminCouponView{}, false, err
	}
	return idempotentTenantWrite(ctx, s.repo, scopeAdminCouponCreate, repository.StaffSubject(staff.StaffID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (AdminCouponView, error) {
			id, err := tx.AdminCreateCouponTemplate(ctx, f)
			if err != nil {
				return AdminCouponView{}, mapCouponRepoErr(err)
			}
			return s.view(ctx, tx, id)
		})
}

// CouponTemplatePatch 是 PATCH 请求：nil 表示不改。
type CouponTemplatePatch struct {
	Name             *string
	CouponType       *int16
	ThresholdCents   *int64
	DiscountCents    *int64
	DiscountRate     *int16
	MaxDiscountCents *int64
	ValidMode        *int16
	ValidStartAt     *time.Time
	ValidEndAt       *time.Time
	ValidDays        *int32
	TotalCount       *int32
	PerUserLimit     *int32
	Claimable        *bool
	Status           *int16
}

// Update 实现 PATCH /admin/coupon-templates/{template_id}。
//
// 先锁模板行、再读、再合并、再判「已发出之后券面不能改」、最后整行写回 ——
// 判定与写必须看到同一个 issued_count（AdminLockCouponTemplate 上的注释）。
// 判的是**值有没有变**，不是「请求里带没带这个字段」：后台表单常常把整张表单
// 原样提交，带着没改过的券面字段不该被拒。
func (s *AdminCouponService) Update(ctx context.Context, id int64, p CouponTemplatePatch) (AdminCouponView, error) {
	if _, err := requireCouponStaff(ctx); err != nil {
		return AdminCouponView{}, err
	}
	var out AdminCouponView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		issued, err := tx.AdminLockCouponTemplate(ctx, id)
		if errors.Is(err, repository.ErrCouponTemplateNotFound) {
			return fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, id)
		}
		if err != nil {
			return err
		}
		cur, err := tx.AdminGetCouponTemplate(ctx, id)
		if err != nil {
			return err
		}
		old := fieldsOf(cur)
		f := old
		if p.Name != nil {
			f.Name = strings.TrimSpace(*p.Name)
		}
		if p.CouponType != nil {
			f.CouponType = *p.CouponType
		}
		if p.ThresholdCents != nil {
			f.ThresholdCents = *p.ThresholdCents
		}
		if p.DiscountCents != nil {
			f.DiscountCents = *p.DiscountCents
		}
		if p.DiscountRate != nil {
			f.DiscountRate = *p.DiscountRate
		}
		if p.MaxDiscountCents != nil {
			f.MaxDiscountCents = *p.MaxDiscountCents
		}
		if p.ValidMode != nil {
			// 换模式时整组替换：另一种模式的字段不能从旧值里漏过来。
			f.ValidMode = *p.ValidMode
			f.ValidStartAt, f.ValidEndAt, f.ValidDays = p.ValidStartAt, p.ValidEndAt, 0
			if p.ValidDays != nil {
				f.ValidDays = *p.ValidDays
			}
		} else {
			if p.ValidStartAt != nil {
				f.ValidStartAt = p.ValidStartAt
			}
			if p.ValidEndAt != nil {
				f.ValidEndAt = p.ValidEndAt
			}
			if p.ValidDays != nil {
				f.ValidDays = *p.ValidDays
			}
		}
		if p.TotalCount != nil {
			f.TotalCount = *p.TotalCount
		}
		if p.PerUserLimit != nil {
			f.PerUserLimit = *p.PerUserLimit
		}
		if p.Claimable != nil {
			f.Claimable = *p.Claimable
		}
		if p.Status != nil {
			f.Status = *p.Status
		}

		if issued > 0 && !sameFaceValue(old, f) {
			return fmt.Errorf("%w: 已发出 %d 张。券实例不快照规则，改券面等于悄悄改掉买家手里的券",
				ErrCouponTemplateLocked, issued)
		}
		if err := validateCouponTemplate(f, issued); err != nil {
			return err
		}
		if err := tx.AdminUpdateCouponTemplate(ctx, id, f); err != nil {
			return mapCouponRepoErr(err)
		}
		out, err = s.view(ctx, tx, id)
		return err
	})
	return out, err
}

func fieldsOf(t repository.CouponTemplate) repository.CouponTemplateFields {
	return repository.CouponTemplateFields{
		Name: t.Rule.Name, CouponType: t.Rule.CouponType, ThresholdCents: t.Rule.ThresholdCents,
		DiscountCents: t.Rule.DiscountCents, DiscountRate: t.Rule.DiscountRate,
		MaxDiscountCents: t.Rule.MaxDiscountCents, ValidMode: t.ValidMode,
		ValidStartAt: t.ValidStartAt, ValidEndAt: t.ValidEndAt, ValidDays: t.ValidDays,
		TotalCount: t.TotalCount, PerUserLimit: t.PerUserLimit, Claimable: t.Claimable,
		Status: t.Status,
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// sameFaceValue：决定券面价值的字段（券型、门槛、减免、折扣、封顶、有效期）一个没变。
func sameFaceValue(a, b repository.CouponTemplateFields) bool {
	return a.CouponType == b.CouponType && a.ThresholdCents == b.ThresholdCents &&
		a.DiscountCents == b.DiscountCents && a.DiscountRate == b.DiscountRate &&
		a.MaxDiscountCents == b.MaxDiscountCents && a.ValidMode == b.ValidMode &&
		sameTime(a.ValidStartAt, b.ValidStartAt) && sameTime(a.ValidEndAt, b.ValidEndAt) &&
		a.ValidDays == b.ValidDays
}

// validateCouponTemplate 在进库之前把 chk_coupon_* 的每一条用人话说一遍。
// 数据库的 CHECK 是最后一道；这里先挡，是为了让 422 的 detail 说得出是哪一条。
func validateCouponTemplate(f repository.CouponTemplateFields, issued int32) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrCouponBadRequest, fmt.Sprintf(format, args...))
	}
	if f.Name == "" || utf8.RuneCountInString(f.Name) > maxCouponNameRunes {
		return bad("name 必须是 1 到 %d 个字", maxCouponNameRunes)
	}
	if f.ThresholdCents < 0 || f.DiscountCents < 0 || f.MaxDiscountCents < 0 {
		return bad("金额不能为负")
	}
	switch f.CouponType {
	case couponTypeFullReduction:
		if f.DiscountCents <= 0 {
			return bad("满减券的 discount_cents 必须大于 0")
		}
		if f.ThresholdCents < f.DiscountCents {
			return bad("满减券的门槛 %d 分小于减免 %d 分：「满 %d 减 %d」是配置错误",
				f.ThresholdCents, f.DiscountCents, f.ThresholdCents, f.DiscountCents)
		}
		if f.DiscountRate != 0 || f.MaxDiscountCents != 0 {
			return bad("满减券不带 discount_rate / max_discount_cents")
		}
	case couponTypeRateDiscount:
		if f.DiscountRate < 1 || f.DiscountRate > 999 {
			return bad("折扣券的 discount_rate 是千分比，必须在 1 到 999 之间（850 = 8.5 折），实得 %d", f.DiscountRate)
		}
		if f.DiscountCents != 0 {
			return bad("折扣券不带 discount_cents（封顶用 max_discount_cents）")
		}
	case couponTypeInstant:
		if f.DiscountCents <= 0 {
			return bad("立减券的 discount_cents 必须大于 0")
		}
		if f.ThresholdCents != 0 {
			return bad("立减券没有门槛；有门槛的请建满减券")
		}
		if f.DiscountRate != 0 || f.MaxDiscountCents != 0 {
			return bad("立减券不带 discount_rate / max_discount_cents")
		}
	case couponTypeFreeShipping:
		// 00056 起可建：抵的是运费，最多抵 max_discount_cents（0 = 运费全免）。
		// 门槛 threshold_cents 比适用商品小计，与满减券同一个口径，可以是 0。
		if f.DiscountCents != 0 || f.DiscountRate != 0 {
			return bad("包邮券抵的是运费，不带 discount_cents / discount_rate（最多抵多少用 max_discount_cents，0 = 全免）")
		}
	default:
		return bad("coupon_type 只能是 1 满减 / 2 折扣 / 3 立减 / 4 包邮，实得 %d", f.CouponType)
	}
	switch f.ValidMode {
	case 1:
		if f.ValidStartAt == nil || f.ValidEndAt == nil {
			return bad("绝对时间模式要给 valid_start_at 与 valid_end_at")
		}
		if !f.ValidEndAt.After(*f.ValidStartAt) {
			return bad("valid_end_at 必须晚于 valid_start_at")
		}
		if f.ValidDays != 0 {
			return bad("绝对时间模式不带 valid_days")
		}
	case 2:
		if f.ValidDays < 1 || f.ValidDays > maxCouponValidDays {
			return bad("领取后 N 天模式的 valid_days 必须在 1 到 %d 之间", maxCouponValidDays)
		}
		if f.ValidStartAt != nil || f.ValidEndAt != nil {
			return bad("领取后 N 天模式不带 valid_start_at / valid_end_at")
		}
	default:
		return bad("valid_mode 只能是 1 绝对时间 / 2 领取后 N 天")
	}
	if f.TotalCount < 0 {
		return bad("total_count 不能为负（0 表示不限）")
	}
	if f.TotalCount > 0 && f.TotalCount < issued {
		return bad("total_count %d 小于已发出的 %d 张", f.TotalCount, issued)
	}
	if f.PerUserLimit < 1 {
		return bad("per_user_limit 至少是 1")
	}
	if f.Status != 0 && f.Status != 1 {
		return bad("status 只能是 0 或 1")
	}
	return nil
}

func mapCouponRepoErr(err error) error {
	switch {
	case errors.Is(err, repository.ErrCouponRuleViolation), errors.Is(err, repository.ErrCouponScopeDuplicated):
		return fmt.Errorf("%w: %v", ErrCouponBadRequest, err)
	case errors.Is(err, repository.ErrCouponTemplateNotFound):
		return fmt.Errorf("%w: %v", ErrCouponTemplateNotFound, err)
	default:
		return err
	}
}

// SetScopes 实现 PUT /admin/coupon-templates/{template_id}/scopes（整组替换）。
//
// target_id 是多态列，外键管不到它（数据模型 §7），归属校验在这里：分类、商品、
// 大区、门店四种逐条在当前租户里查（RLS 之下别家的 id 与不存在的同形），
// 查不到的整组拒绝。品牌没有目录表，不校验存在性 —— 它只会去匹配本店商品的 brand_id。
func (s *AdminCouponService) SetScopes(ctx context.Context, id int64,
	in []repository.CouponScopeInput) (AdminCouponView, error) {
	if _, err := requireCouponStaff(ctx); err != nil {
		return AdminCouponView{}, err
	}
	if len(in) > maxCouponScopes {
		return AdminCouponView{}, fmt.Errorf("%w: 一张券至多 %d 条范围", ErrCouponBadRequest, maxCouponScopes)
	}
	type key struct {
		t  int16
		id int64
	}
	seen := map[key]bool{}
	byKind := map[repository.ScopeTargetKind][]int64{}
	for i, sc := range in {
		switch {
		case sc.ScopeType == repository.ScopeAll:
			if sc.TargetID != nil {
				return AdminCouponView{}, fmt.Errorf("%w: 第 %d 条：全场规则不带 target_id", ErrCouponBadRequest, i+1)
			}
			if !sc.Include {
				return AdminCouponView{}, fmt.Errorf("%w: 第 %d 条：全场不能是排除（那等于这张券哪儿都不能用）",
					ErrCouponBadRequest, i+1)
			}
		case sc.ScopeType >= repository.ScopeCategory && sc.ScopeType <= repository.ScopeStore:
			if sc.TargetID == nil || *sc.TargetID <= 0 {
				return AdminCouponView{}, fmt.Errorf("%w: 第 %d 条：scope_type=%d 必须给正的 target_id",
					ErrCouponBadRequest, i+1, sc.ScopeType)
			}
		default:
			return AdminCouponView{}, fmt.Errorf("%w: 第 %d 条：scope_type 只能是 1 到 6", ErrCouponBadRequest, i+1)
		}
		k := key{t: sc.ScopeType}
		if sc.TargetID != nil {
			k.id = *sc.TargetID
		}
		if seen[k] {
			return AdminCouponView{}, fmt.Errorf("%w: 第 %d 条：同一个目标出现了两次（同时包含与排除也算）",
				ErrCouponBadRequest, i+1)
		}
		seen[k] = true
		if kind, ok := targetKindOf(sc.ScopeType); ok {
			byKind[kind] = append(byKind[kind], *sc.TargetID)
		}
	}

	var out AdminCouponView
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		issued, err := tx.AdminLockCouponTemplate(ctx, id)
		if errors.Is(err, repository.ErrCouponTemplateNotFound) {
			return fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, id)
		}
		if err != nil {
			return err
		}
		if issued > 0 {
			cur, err := tx.ListCouponScopes(ctx, []int64{id})
			if err != nil {
				return err
			}
			if !sameScopes(cur[id], in) {
				return fmt.Errorf("%w: 已发出 %d 张。范围决定券能用在哪儿，改它等于改掉买家手里的券",
					ErrCouponTemplateLocked, issued)
			}
		}
		for kind, ids := range byKind {
			live, err := tx.LiveTargetIDs(ctx, kind, ids)
			if err != nil {
				return err
			}
			ok := map[int64]bool{}
			for _, v := range live {
				ok[v] = true
			}
			var missing []int64
			for _, v := range ids {
				if !ok[v] {
					missing = append(missing, v)
				}
			}
			if len(missing) > 0 {
				sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
				return fmt.Errorf("%w: %s %v 在本店查不到（不存在、已删除或属于别家店）",
					ErrCouponBadRequest, targetKindName(kind), missing)
			}
		}
		if err := tx.ReplaceCouponScopes(ctx, id, in); err != nil {
			return mapCouponRepoErr(err)
		}
		out, err = s.view(ctx, tx, id)
		return err
	})
	return out, err
}

func targetKindOf(t int16) (repository.ScopeTargetKind, bool) {
	switch t {
	case repository.ScopeCategory:
		return repository.TargetCategory, true
	case repository.ScopeProduct:
		return repository.TargetProduct, true
	case repository.ScopeRegion:
		return repository.TargetRegion, true
	case repository.ScopeStore:
		return repository.TargetStore, true
	default:
		return 0, false
	}
}

func targetKindName(k repository.ScopeTargetKind) string {
	switch k {
	case repository.TargetCategory:
		return "分类"
	case repository.TargetProduct:
		return "商品"
	case repository.TargetRegion:
		return "大区"
	default:
		return "门店"
	}
}

func sameScopes(cur []repository.CouponScope, in []repository.CouponScopeInput) bool {
	if len(cur) != len(in) {
		return false
	}
	type key struct {
		t   int16
		id  int64
		inc bool
	}
	m := map[key]int{}
	for _, c := range cur {
		k := key{t: c.ScopeType, inc: c.Include}
		if c.TargetID != nil {
			k.id = *c.TargetID
		}
		m[k]++
	}
	for _, c := range in {
		k := key{t: c.ScopeType, inc: c.Include}
		if c.TargetID != nil {
			k.id = *c.TargetID
		}
		m[k]--
		if m[k] < 0 {
			return false
		}
	}
	return true
}

// GrantItem 是定向发放结果的一行。
type GrantItem struct {
	Phone        string
	UserID       int64
	UserCouponID int64
	CouponCode   string
}

// GrantResult 是 POST /admin/coupon-templates/{template_id}/grants 的结果。
type GrantResult struct {
	TemplateID int64
	Items      []GrantItem
}

// Grant 实现按手机号定向发放。**整批全有或全无**（数据模型 §7「两种发券方式」）：
// 有一个手机号查不到买家，整批拒绝、一张都不发。
func (s *AdminCouponService) Grant(ctx context.Context, id int64, phones []string, idemKey string) (GrantResult, bool, error) {
	staff, err := requireCouponStaff(ctx)
	if err != nil {
		return GrantResult{}, false, err
	}
	clean := make([]string, 0, len(phones))
	for _, p := range phones {
		p = strings.TrimSpace(p)
		if p == "" {
			return GrantResult{}, false, fmt.Errorf("%w: 手机号不能为空", ErrCouponBadRequest)
		}
		clean = append(clean, p)
	}
	if len(clean) == 0 || len(clean) > maxGrantPhones {
		return GrantResult{}, false, fmt.Errorf("%w: 一批 1 到 %d 个手机号", ErrCouponBadRequest, maxGrantPhones)
	}
	hash, err := adminRequestHash([]int64{id}, clean)
	if err != nil {
		return GrantResult{}, false, err
	}
	now := s.now()
	return idempotentTenantWrite(ctx, s.repo, scopeAdminCouponGrant, repository.StaffSubject(staff.StaffID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (GrantResult, error) {
			// 先确认模板在（404 优先于「手机号查不到」：路径里指名的东西不存在是更根本的错）。
			if _, err := tx.ClaimTemplateState(ctx, id); err != nil {
				if errors.Is(err, repository.ErrCouponTemplateNotFound) {
					return GrantResult{}, fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, id)
				}
				return GrantResult{}, err
			}
			buyers, err := tx.FindBuyersByPhones(ctx, clean)
			if err != nil {
				return GrantResult{}, err
			}
			byPhone := map[string]int64{}
			for _, b := range buyers {
				byPhone[b.Phone] = b.ID
			}
			var unknown []string
			seenUnknown := map[string]bool{}
			for _, p := range clean {
				if _, ok := byPhone[p]; !ok && !seenUnknown[p] {
					seenUnknown[p] = true
					unknown = append(unknown, p)
				}
			}
			if len(unknown) > 0 {
				return GrantResult{}, fmt.Errorf("%w: 这些手机号在本店查不到买家，整批未发放：%s",
					ErrCouponBadRequest, strings.Join(unknown, "、"))
			}
			win, err := tx.BumpTemplateForGrant(ctx, id, int32(len(clean)), now)
			if errors.Is(err, repository.ErrCouponTemplateExhausted) {
				return GrantResult{}, classifyClaimFailure(ctx, tx, id, now, false)
			}
			if err != nil {
				return GrantResult{}, err
			}
			res := GrantResult{TemplateID: id, Items: make([]GrantItem, 0, len(clean))}
			for _, p := range clean {
				uid := byPhone[p]
				cid, err := issueCoupon(ctx, tx, id, uid, repository.CouponSourceGrant, win, now)
				if err != nil {
					return GrantResult{}, err
				}
				c, err := tx.FindUserCoupon(ctx, cid, uid)
				if err != nil {
					return GrantResult{}, err
				}
				res.Items = append(res.Items, GrantItem{Phone: p, UserID: uid, UserCouponID: cid, CouponCode: c.Code})
			}
			return res, nil
		})
}
