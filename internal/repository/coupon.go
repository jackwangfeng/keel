package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 营销域（优惠券）在 repository 边界上的那一面。数据模型 §7。
//
// 单独一个文件、单独一个接口（CouponTx），Tx 那份组合定义只多一行嵌入 ——
// 理由与 order.go 的文件头一样：Tx 同时被好几条并发任务碰到。
//
// 这一层**只取素材、只做条件更新**，不判定「这张券能不能用、能减多少」。
// 那个判定只有一份实现（service/coupon_calc.go），试算、下单、「本单可用券」
// 三处都经过它 —— 把一半条件写进这里的 WHERE，就有了第二份会跑偏的实现。

var (
	// ErrCouponNotFound：按 id 在本租户、本买家名下查不到这张券。
	//
	// 不区分「不存在」与「是别人的」，理由与 ErrAddressNotFound 一样：
	// 分开报就成了一个猜 id 探测别人券包的口子。
	ErrCouponNotFound = errors.New("优惠券不存在")

	// ErrCouponTemplateNotFound：券模板在本租户查不到（RLS 之下，别家的与不存在的同形）。
	ErrCouponTemplateNotFound = errors.New("券模板不存在")

	// ErrCouponTemplateExhausted：占名额的那条条件 UPDATE 受影响 0 行。
	// 成因有好几种（停用、不可领、领完、结束），由调用方回读模板分出来。
	ErrCouponTemplateExhausted = errors.New("券模板没有可发的名额")

	// ErrCouponRuleViolation：写模板时撞上了 chk_coupon_* 约束。
	//
	// 正常路径上 service 已经把字段组合校验过了，走到这里说明两边的规则分叉了 ——
	// 数据库是最后一道，它拒绝的东西翻成 422 而不是 500：那确实是请求不成立。
	ErrCouponRuleViolation = errors.New("券模板的字段组合不成立")

	// ErrCouponScopeDuplicated：同一条范围（类型 + 目标）在一组里出现两次（uk_coupon_scopes）。
	ErrCouponScopeDuplicated = errors.New("同一条适用范围出现了两次")
)

// 券实例的状态（数据模型 §7 的状态机）。常量，不是散落的字面量。
const (
	UserCouponUnused  int16 = 1
	UserCouponLocked  int16 = 2
	UserCouponUsed    int16 = 3
	UserCouponExpired int16 = 4
)

// 券实例的来源。
const (
	CouponSourceClaim int16 = 1 // 领券中心
	CouponSourceGrant int16 = 2 // 商家定向发放
)

// 适用范围的类型。
const (
	ScopeAll      int16 = 1
	ScopeCategory int16 = 2
	ScopeProduct  int16 = 3
	ScopeBrand    int16 = 4
	ScopeRegion   int16 = 5
	ScopeStore    int16 = 6
)

// CouponRule 是模板上决定券面价值的那几列。券实例不快照规则（§7），从模板读。
type CouponRule struct {
	TemplateID       int64
	Name             string
	CouponType       int16
	ThresholdCents   int64
	DiscountCents    int64
	DiscountRate     int16
	MaxDiscountCents int64
}

// UserCoupon 是一张券实例连同它的规则。
type UserCoupon struct {
	ID           int64
	Code         string
	UserID       int64
	Source       int16
	Status       int16
	ValidStartAt time.Time
	ValidEndAt   time.Time
	UsedAt       *time.Time
	CreatedAt    time.Time
	Rule         CouponRule
}

// CouponScope 是一条适用范围规则，连同计算与展示要的素材。
type CouponScope struct {
	TemplateID int64
	ScopeType  int16
	TargetID   *int64
	Include    bool
	// CategoryPath 只有分类规则、且分类未软删时才有值。分类规则按它的前缀判子孙。
	CategoryPath *string
	// TargetName 是展示名；品牌与已软删的目标为 nil。
	TargetName *string
}

// CouponScopeInput 是后台写范围时的一条。
type CouponScopeInput struct {
	ScopeType int16
	TargetID  *int64
	Include   bool
}

// ValidWindow 是占名额那一步回来的有效期素材，调用方据此算出这张券的窗口。
type ValidWindow struct {
	ValidMode    int16
	ValidStartAt *time.Time
	ValidEndAt   *time.Time
	ValidDays    int32
	PerUserLimit int32
}

// ClaimTemplateState 是领取失败之后回读的模板状态，用来分出失败原因。
type ClaimTemplateState struct {
	Status      int16
	Claimable   bool
	TotalCount  int32
	IssuedCount int32
	ValidMode   int16
	ValidEndAt  *time.Time
}

// ClaimableTemplate 是领券中心的一项。
type ClaimableTemplate struct {
	Rule         CouponRule
	ValidMode    int16
	ValidStartAt *time.Time
	ValidEndAt   *time.Time
	ValidDays    int32
	TotalCount   int32
	IssuedCount  int32
	PerUserLimit int32
	ClaimedByMe  int32
}

// CouponTemplate 是后台视角的券模板。
type CouponTemplate struct {
	Rule         CouponRule
	ValidMode    int16
	ValidStartAt *time.Time
	ValidEndAt   *time.Time
	ValidDays    int32
	TotalCount   int32
	IssuedCount  int32
	PerUserLimit int32
	Claimable    bool
	Status       int16
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CouponTemplateFields 是模板上可写的全部列。新建与整行写回共用。
type CouponTemplateFields struct {
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
	Status           int16
}

// CouponStats 是一个模板的发放与核销统计。
type CouponStats struct {
	Issued, Claimed, Granted, Unused, Locked, Used, Expired int32
}

// BuyerRef 是按手机号找到的买家。
type BuyerRef struct {
	ID    int64
	Phone string
}

// NewUserCoupon 是落一张券实例要写的列。
type NewUserCoupon struct {
	Code         string
	TemplateID   int64
	UserID       int64
	Source       int16
	ValidStartAt time.Time
	ValidEndAt   time.Time
}

// ScopeTargetKind 是需要做归属校验的四种范围目标。
type ScopeTargetKind int

const (
	TargetCategory ScopeTargetKind = iota
	TargetProduct
	TargetRegion
	TargetStore
)

// CouponTx 是优惠券这一面。
type CouponTx interface {
	// FindUserCoupon 取当前买家的一张券连同规则。查不到返回 ErrCouponNotFound。
	FindUserCoupon(ctx context.Context, id, userID int64) (UserCoupon, error)
	// ListUsableUserCoupons 取当前买家在 now 这一刻可用（未使用、在有效期内）的券。
	ListUsableUserCoupons(ctx context.Context, userID int64, now time.Time) ([]UserCoupon, error)
	// ListCouponScopes 取一批模板的范围规则，按模板分组。没有规则的模板不在 map 里。
	ListCouponScopes(ctx context.Context, templateIDs []int64) (map[int64][]CouponScope, error)
	// ListUserCoupons 是 GET /coupons 的一页。filter 为空串或 available/locked/used/expired。
	ListUserCoupons(ctx context.Context, userID int64, filter string, now time.Time,
		limit, offset int32) ([]UserCoupon, int64, error)

	// ListClaimableTemplates 是领券中心的一页。
	ListClaimableTemplates(ctx context.Context, userID int64, now time.Time,
		limit, offset int32) ([]ClaimableTemplate, int64, error)
	// BumpTemplateForClaim 占一个领取名额。没占到返回 ErrCouponTemplateExhausted。
	BumpTemplateForClaim(ctx context.Context, templateID int64, now time.Time) (ValidWindow, error)
	// ClaimTemplateState 回读模板状态以分出领取失败的原因。查不到返回 ErrCouponTemplateNotFound。
	ClaimTemplateState(ctx context.Context, templateID int64) (ClaimTemplateState, error)
	// CountUserTemplateCoupons 这个买家持有该模板多少张。
	CountUserTemplateCoupons(ctx context.Context, templateID, userID int64) (int32, error)
	// InsertUserCoupon 落一张券实例，返回它的 id。
	InsertUserCoupon(ctx context.Context, n NewUserCoupon) (int64, error)
	// BumpTemplateForGrant 为定向发放占 n 个名额。没占到返回 ErrCouponTemplateExhausted。
	BumpTemplateForGrant(ctx context.Context, templateID int64, n int32, now time.Time) (ValidWindow, error)
	// FindBuyersByPhones 按手机号找本店的买家。
	FindBuyersByPhones(ctx context.Context, phones []string) ([]BuyerRef, error)

	// LockUserCoupon 券分支正向：1 → 2，返回受影响行数。
	LockUserCoupon(ctx context.Context, couponID, userID, orderID int64) (int64, error)
	// UnlockCouponForOrder 2 → 1（补偿、超时关单），返回受影响行数。
	UnlockCouponForOrder(ctx context.Context, orderID int64) (int64, error)
	// ConsumeCouponForOrder 2 → 3（支付回调），返回受影响行数。
	ConsumeCouponForOrder(ctx context.Context, orderID int64) (int64, error)
	// CouponStatusForOrder 这一单占着的券的 id 与状态；没有返回 ErrCouponNotFound。
	CouponStatusForOrder(ctx context.Context, orderID int64) (int64, int16, error)

	// 后台。
	AdminListCouponTemplates(ctx context.Context, status *int16, limit, offset int32) ([]CouponTemplate, int64, error)
	AdminGetCouponTemplate(ctx context.Context, id int64) (CouponTemplate, error)
	// AdminLockCouponTemplate 锁住模板行并返回已发出数。查不到返回 ErrCouponTemplateNotFound。
	AdminLockCouponTemplate(ctx context.Context, id int64) (int32, error)
	AdminCreateCouponTemplate(ctx context.Context, f CouponTemplateFields) (int64, error)
	AdminUpdateCouponTemplate(ctx context.Context, id int64, f CouponTemplateFields) error
	CouponTemplateStats(ctx context.Context, templateIDs []int64, now time.Time) (map[int64]CouponStats, error)
	ReplaceCouponScopes(ctx context.Context, templateID int64, scopes []CouponScopeInput) error
	// LiveTargetIDs 返回 ids 里在本租户存在且未软删的那些。
	LiveTargetIDs(ctx context.Context, kind ScopeTargetKind, ids []int64) ([]int64, error)
}

func firstNonNil(vs ...*string) *string {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func optTS(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return ts(*t)
}

// couponRow 是四条「券 + 规则」查询共有的列。sqlc 给每条查询各生成一个行类型，
// 列一模一样；收成一个函数，免得四处各写一遍而某一处漏掉一列。
type couponRow struct {
	ID               int64
	CouponCode       string
	TemplateID       int64
	UserID           int64
	Source           int16
	Status           int16
	ValidStartAt     pgtype.Timestamptz
	ValidEndAt       pgtype.Timestamptz
	UsedAt           pgtype.Timestamptz
	CreatedAt        pgtype.Timestamptz
	Name             string
	CouponType       int16
	ThresholdCents   int64
	DiscountCents    int64
	DiscountRate     int16
	MaxDiscountCents int64
}

func (r couponRow) domain() UserCoupon {
	return UserCoupon{
		ID:           r.ID,
		Code:         r.CouponCode,
		UserID:       r.UserID,
		Source:       r.Source,
		Status:       r.Status,
		ValidStartAt: r.ValidStartAt.Time,
		ValidEndAt:   r.ValidEndAt.Time,
		UsedAt:       optTime(r.UsedAt),
		CreatedAt:    r.CreatedAt.Time,
		Rule: CouponRule{
			TemplateID:       r.TemplateID,
			Name:             r.Name,
			CouponType:       r.CouponType,
			ThresholdCents:   r.ThresholdCents,
			DiscountCents:    r.DiscountCents,
			DiscountRate:     r.DiscountRate,
			MaxDiscountCents: r.MaxDiscountCents,
		},
	}
}

func (t tenantTx) FindUserCoupon(ctx context.Context, id, userID int64) (UserCoupon, error) {
	r, err := t.q.GetUserCouponWithRule(ctx, db.GetUserCouponWithRuleParams{ID: id, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return UserCoupon{}, fmt.Errorf("user_coupon %d: %w", id, ErrCouponNotFound)
	}
	if err != nil {
		return UserCoupon{}, err
	}
	return couponRow(r).domain(), nil
}

func (t tenantTx) ListUsableUserCoupons(ctx context.Context, userID int64, now time.Time) ([]UserCoupon, error) {
	rows, err := t.q.ListUsableUserCoupons(ctx, db.ListUsableUserCouponsParams{UserID: userID, Now: ts(now)})
	if err != nil {
		return nil, err
	}
	out := make([]UserCoupon, 0, len(rows))
	for _, r := range rows {
		out = append(out, couponRow(r).domain())
	}
	return out, nil
}

func (t tenantTx) ListCouponScopes(ctx context.Context, templateIDs []int64) (map[int64][]CouponScope, error) {
	out := map[int64][]CouponScope{}
	if len(templateIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ListCouponScopes(ctx, templateIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.TemplateID] = append(out[r.TemplateID], CouponScope{
			TemplateID:   r.TemplateID,
			ScopeType:    r.ScopeType,
			TargetID:     r.TargetID,
			Include:      r.Include,
			CategoryPath: r.CategoryPath,
			TargetName:   firstNonNil(r.CategoryName, r.ProductTitle, r.RegionName, r.StoreName),
		})
	}
	return out, nil
}

func (t tenantTx) ListUserCoupons(ctx context.Context, userID int64, filter string, now time.Time,
	limit, offset int32) ([]UserCoupon, int64, error) {
	rows, err := t.q.ListUserCoupons(ctx, db.ListUserCouponsParams{
		UserID: userID, StatusFilter: filter, Now: ts(now), PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountUserCoupons(ctx, db.CountUserCouponsParams{
		UserID: userID, StatusFilter: filter, Now: ts(now),
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]UserCoupon, 0, len(rows))
	for _, r := range rows {
		out = append(out, couponRow(r).domain())
	}
	return out, total, nil
}

func (t tenantTx) ListClaimableTemplates(ctx context.Context, userID int64, now time.Time,
	limit, offset int32) ([]ClaimableTemplate, int64, error) {
	rows, err := t.q.ListClaimableTemplates(ctx, db.ListClaimableTemplatesParams{
		UserID: userID, Now: ts(now), PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.CountClaimableTemplates(ctx, ts(now))
	if err != nil {
		return nil, 0, err
	}
	out := make([]ClaimableTemplate, 0, len(rows))
	for _, r := range rows {
		out = append(out, ClaimableTemplate{
			Rule: CouponRule{
				TemplateID: r.ID, Name: r.Name, CouponType: r.CouponType,
				ThresholdCents: r.ThresholdCents, DiscountCents: r.DiscountCents,
				DiscountRate: r.DiscountRate, MaxDiscountCents: r.MaxDiscountCents,
			},
			ValidMode:    r.ValidMode,
			ValidStartAt: optTime(r.ValidStartAt),
			ValidEndAt:   optTime(r.ValidEndAt),
			ValidDays:    r.ValidDays,
			TotalCount:   r.TotalCount,
			IssuedCount:  r.IssuedCount,
			PerUserLimit: r.PerUserLimit,
			ClaimedByMe:  r.ClaimedByMe,
		})
	}
	return out, total, nil
}

func (t tenantTx) BumpTemplateForClaim(ctx context.Context, templateID int64, now time.Time) (ValidWindow, error) {
	r, err := t.q.BumpTemplateForClaim(ctx, db.BumpTemplateForClaimParams{ID: templateID, Now: ts(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ValidWindow{}, fmt.Errorf("template %d: %w", templateID, ErrCouponTemplateExhausted)
	}
	if err != nil {
		return ValidWindow{}, err
	}
	return ValidWindow{
		ValidMode: r.ValidMode, ValidStartAt: optTime(r.ValidStartAt), ValidEndAt: optTime(r.ValidEndAt),
		ValidDays: r.ValidDays, PerUserLimit: r.PerUserLimit,
	}, nil
}

func (t tenantTx) ClaimTemplateState(ctx context.Context, templateID int64) (ClaimTemplateState, error) {
	r, err := t.q.GetTemplateForClaim(ctx, templateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaimTemplateState{}, fmt.Errorf("template %d: %w", templateID, ErrCouponTemplateNotFound)
	}
	if err != nil {
		return ClaimTemplateState{}, err
	}
	return ClaimTemplateState{
		Status: r.Status, Claimable: r.Claimable, TotalCount: r.TotalCount,
		IssuedCount: r.IssuedCount, ValidMode: r.ValidMode, ValidEndAt: optTime(r.ValidEndAt),
	}, nil
}

func (t tenantTx) CountUserTemplateCoupons(ctx context.Context, templateID, userID int64) (int32, error) {
	return t.q.CountUserTemplateCoupons(ctx, db.CountUserTemplateCouponsParams{
		TemplateID: templateID, UserID: userID,
	})
}

func (t tenantTx) InsertUserCoupon(ctx context.Context, n NewUserCoupon) (int64, error) {
	r, err := t.q.InsertUserCoupon(ctx, db.InsertUserCouponParams{
		CouponCode:   n.Code,
		TemplateID:   n.TemplateID,
		UserID:       n.UserID,
		Source:       n.Source,
		ValidStartAt: ts(n.ValidStartAt),
		ValidEndAt:   ts(n.ValidEndAt),
	})
	if err != nil {
		return 0, err
	}
	return r.ID, nil
}

func (t tenantTx) BumpTemplateForGrant(ctx context.Context, templateID int64, n int32, now time.Time) (ValidWindow, error) {
	r, err := t.q.BumpTemplateForGrant(ctx, db.BumpTemplateForGrantParams{ID: templateID, N: n, Now: ts(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ValidWindow{}, fmt.Errorf("template %d: %w", templateID, ErrCouponTemplateExhausted)
	}
	if err != nil {
		return ValidWindow{}, err
	}
	return ValidWindow{
		ValidMode: r.ValidMode, ValidStartAt: optTime(r.ValidStartAt), ValidEndAt: optTime(r.ValidEndAt),
		ValidDays: r.ValidDays,
	}, nil
}

func (t tenantTx) FindBuyersByPhones(ctx context.Context, phones []string) ([]BuyerRef, error) {
	rows, err := t.q.FindUsersByPhones(ctx, phones)
	if err != nil {
		return nil, err
	}
	out := make([]BuyerRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, BuyerRef{ID: r.ID, Phone: r.Phone})
	}
	return out, nil
}

func (t tenantTx) LockUserCoupon(ctx context.Context, couponID, userID, orderID int64) (int64, error) {
	return t.q.LockUserCoupon(ctx, db.LockUserCouponParams{ID: couponID, UserID: userID, OrderID: &orderID})
}

func (t tenantTx) UnlockCouponForOrder(ctx context.Context, orderID int64) (int64, error) {
	return t.q.UnlockCouponForOrder(ctx, &orderID)
}

func (t tenantTx) ConsumeCouponForOrder(ctx context.Context, orderID int64) (int64, error) {
	return t.q.ConsumeCouponForOrder(ctx, &orderID)
}

func (t tenantTx) CouponStatusForOrder(ctx context.Context, orderID int64) (int64, int16, error) {
	r, err := t.q.GetCouponStatusForOrder(ctx, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("order %d: %w", orderID, ErrCouponNotFound)
	}
	if err != nil {
		return 0, 0, err
	}
	return r.ID, r.Status, nil
}

// templateRow 同 couponRow：两条后台读查询的列一模一样。
type templateRow struct {
	ID               int64
	Name             string
	CouponType       int16
	ThresholdCents   int64
	DiscountCents    int64
	DiscountRate     int16
	MaxDiscountCents int64
	ValidMode        int16
	ValidStartAt     pgtype.Timestamptz
	ValidEndAt       pgtype.Timestamptz
	ValidDays        int32
	TotalCount       int32
	IssuedCount      int32
	PerUserLimit     int32
	Claimable        bool
	Status           int16
	CreatedAt        pgtype.Timestamptz
	UpdatedAt        pgtype.Timestamptz
}

func (r templateRow) domain() CouponTemplate {
	return CouponTemplate{
		Rule: CouponRule{
			TemplateID: r.ID, Name: r.Name, CouponType: r.CouponType,
			ThresholdCents: r.ThresholdCents, DiscountCents: r.DiscountCents,
			DiscountRate: r.DiscountRate, MaxDiscountCents: r.MaxDiscountCents,
		},
		ValidMode:    r.ValidMode,
		ValidStartAt: optTime(r.ValidStartAt),
		ValidEndAt:   optTime(r.ValidEndAt),
		ValidDays:    r.ValidDays,
		TotalCount:   r.TotalCount,
		IssuedCount:  r.IssuedCount,
		PerUserLimit: r.PerUserLimit,
		Claimable:    r.Claimable,
		Status:       r.Status,
		CreatedAt:    r.CreatedAt.Time,
		UpdatedAt:    r.UpdatedAt.Time,
	}
}

func (t tenantTx) AdminListCouponTemplates(ctx context.Context, status *int16, limit, offset int32) ([]CouponTemplate, int64, error) {
	rows, err := t.q.AdminListCouponTemplates(ctx, db.AdminListCouponTemplatesParams{
		Status: status, PageLimit: limit, PageOffset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	total, err := t.q.AdminCountCouponTemplates(ctx, status)
	if err != nil {
		return nil, 0, err
	}
	out := make([]CouponTemplate, 0, len(rows))
	for _, r := range rows {
		out = append(out, templateRow(r).domain())
	}
	return out, total, nil
}

func (t tenantTx) AdminGetCouponTemplate(ctx context.Context, id int64) (CouponTemplate, error) {
	r, err := t.q.AdminGetCouponTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return CouponTemplate{}, fmt.Errorf("template %d: %w", id, ErrCouponTemplateNotFound)
	}
	if err != nil {
		return CouponTemplate{}, err
	}
	return templateRow(r).domain(), nil
}

func (t tenantTx) AdminLockCouponTemplate(ctx context.Context, id int64) (int32, error) {
	r, err := t.q.AdminLockCouponTemplate(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("template %d: %w", id, ErrCouponTemplateNotFound)
	}
	if err != nil {
		return 0, err
	}
	return r.IssuedCount, nil
}

// couponCheckViolation 把 chk_coupon_* 的 23514 翻成 ErrCouponRuleViolation。
// 按约束名前缀挑，而不是「凡是 23514」：别的 CHECK 撞上了是另一种 bug，不该被说成请求不成立。
func couponCheckViolation(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		len(pgErr.ConstraintName) > 4 && pgErr.ConstraintName[:4] == "chk_" {
		return fmt.Errorf("%w（%s）", ErrCouponRuleViolation, pgErr.ConstraintName)
	}
	return err
}

func (t tenantTx) AdminCreateCouponTemplate(ctx context.Context, f CouponTemplateFields) (int64, error) {
	id, err := t.q.AdminCreateCouponTemplate(ctx, db.AdminCreateCouponTemplateParams{
		Name: f.Name, CouponType: f.CouponType, ThresholdCents: f.ThresholdCents,
		DiscountCents: f.DiscountCents, DiscountRate: f.DiscountRate,
		MaxDiscountCents: f.MaxDiscountCents, ValidMode: f.ValidMode,
		ValidStartAt: optTS(f.ValidStartAt), ValidEndAt: optTS(f.ValidEndAt),
		ValidDays: f.ValidDays, TotalCount: f.TotalCount, PerUserLimit: f.PerUserLimit,
		Claimable: f.Claimable,
	})
	if err != nil {
		return 0, couponCheckViolation(err)
	}
	return id, nil
}

func (t tenantTx) AdminUpdateCouponTemplate(ctx context.Context, id int64, f CouponTemplateFields) error {
	n, err := t.q.AdminUpdateCouponTemplate(ctx, db.AdminUpdateCouponTemplateParams{
		ID: id, Name: f.Name, CouponType: f.CouponType, ThresholdCents: f.ThresholdCents,
		DiscountCents: f.DiscountCents, DiscountRate: f.DiscountRate,
		MaxDiscountCents: f.MaxDiscountCents, ValidMode: f.ValidMode,
		ValidStartAt: optTS(f.ValidStartAt), ValidEndAt: optTS(f.ValidEndAt),
		ValidDays: f.ValidDays, TotalCount: f.TotalCount, PerUserLimit: f.PerUserLimit,
		Claimable: f.Claimable, Status: f.Status,
	})
	if err != nil {
		return couponCheckViolation(err)
	}
	if n != 1 {
		return fmt.Errorf("template %d: %w", id, ErrCouponTemplateNotFound)
	}
	return nil
}

func (t tenantTx) CouponTemplateStats(ctx context.Context, templateIDs []int64, now time.Time) (map[int64]CouponStats, error) {
	out := map[int64]CouponStats{}
	if len(templateIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.CouponTemplateStats(ctx, db.CouponTemplateStatsParams{TemplateIds: templateIDs, Now: ts(now)})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.TemplateID] = CouponStats{
			Issued: r.Issued, Claimed: r.Claimed, Granted: r.Granted,
			Unused: r.Unused, Locked: r.Locked, Used: r.Used, Expired: r.Expired,
		}
	}
	return out, nil
}

func (t tenantTx) ReplaceCouponScopes(ctx context.Context, templateID int64, scopes []CouponScopeInput) error {
	if err := t.q.DeleteCouponScopes(ctx, templateID); err != nil {
		return err
	}
	for _, s := range scopes {
		err := t.q.InsertCouponScope(ctx, db.InsertCouponScopeParams{
			TemplateID: templateID, ScopeType: s.ScopeType, TargetID: s.TargetID, Include: s.Include,
		})
		if isUniqueViolation(err, "uk_coupon_scopes") {
			return ErrCouponScopeDuplicated
		}
		if err != nil {
			return couponCheckViolation(err)
		}
	}
	return nil
}

func (t tenantTx) LiveTargetIDs(ctx context.Context, kind ScopeTargetKind, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	switch kind {
	case TargetCategory:
		return t.q.LiveCategoryIDs(ctx, ids)
	case TargetProduct:
		return t.q.LiveProductIDs(ctx, ids)
	case TargetRegion:
		return t.q.LiveRegionIDs(ctx, ids)
	case TargetStore:
		return t.q.LiveStoreIDs(ctx, ids)
	default:
		return nil, fmt.Errorf("范围目标类型 %d 无法校验", kind)
	}
}
