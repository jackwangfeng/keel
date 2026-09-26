package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 优惠券的买家侧：我的券、本单可用券、领券中心、领券。数据模型 §7。
//
// 「这张券能不能用、能减多少」只有一份实现（coupon_calc.go），这里一处都不重写：
// 本单可用券走 pricing.go 的 applicableCoupons，与试算、下单是同一个函数。

var (
	// ErrCouponTemplateNotFound：领券时模板不存在、不属于本店、已停用、或没设为可领。
	// 四种合用一个 404：领券中心里本来就看不到它们。
	ErrCouponTemplateNotFound = errors.New("券模板不存在或不可领")

	// ErrCouponSoldOut：领完了（总量已满），或定向发放时剩余不够整批。409。
	ErrCouponSoldOut = errors.New("券已经发完了")

	// ErrCouponClaimLimitReached：这个买家已达每人限领。409。
	ErrCouponClaimLimitReached = errors.New("已达每人限领")

	// ErrCouponClaimEnded：绝对时间模式下活动已结束（已过 valid_end_at）。409。
	ErrCouponClaimEnded = errors.New("这批券的活动已结束")
)

// couponScopeClaim 是领券在 idempotency_keys 里的作用域。
const couponScopeClaim = "coupons.claim"

// CouponRepository 是券服务需要的仓储能力。
type CouponRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// CouponService 实现买家侧的四条接口。
type CouponService struct {
	repo CouponRepository
	now  func() time.Time
}

func NewCouponService(r CouponRepository) *CouponService {
	return &CouponService{repo: r, now: time.Now}
}

// MyCoupon 是「我的券」里的一张：券、它的范围、展示用的状态。
type MyCoupon struct {
	Coupon repository.UserCoupon
	Scopes []repository.CouponScope
	// DisplayStatus 是对外的状态：未使用且已过 valid_end_at 的券显示为 4 已过期
	// （数据模型 §7：「已过期」现算，不靠定时任务改库）。
	DisplayStatus int16
}

// displayStatus 见 MyCoupon.DisplayStatus。
func displayStatus(c repository.UserCoupon, now time.Time) int16 {
	if c.Status == repository.UserCouponUnused && !now.Before(c.ValidEndAt) {
		return repository.UserCouponExpired
	}
	return c.Status
}

// MyCouponPage 是 GET /coupons 的一页。
type MyCouponPage struct {
	Items    []MyCoupon
	Total    int64
	Page     int
	PageSize int
}

// couponStatusFilters 是契约里 status 的四个取值。空串是「全部」。
var couponStatusFilters = map[string]bool{"": true, "available": true, "locked": true, "used": true, "expired": true}

// ListMine 实现 GET /coupons。
func (s *CouponService) ListMine(ctx context.Context, filter string, page, pageSize int) (MyCouponPage, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return MyCouponPage{}, err
	}
	if !couponStatusFilters[filter] {
		return MyCouponPage{}, fmt.Errorf("%w: status 只能是 available / locked / used / expired，实得 %q",
			ErrBadRequest, filter)
	}
	page, pageSize = clampPaging(page, pageSize)
	now := s.now()
	out := MyCouponPage{Items: []MyCoupon{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		items, total, err := tx.ListUserCoupons(ctx, id.UserID, filter, now,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		scopes, err := tx.ListCouponScopes(ctx, templateIDsOf(items))
		if err != nil {
			return err
		}
		out.Total = total
		for _, c := range items {
			out.Items = append(out.Items, MyCoupon{
				Coupon: c, Scopes: scopes[c.Rule.TemplateID], DisplayStatus: displayStatus(c, now),
			})
		}
		return nil
	})
	if err != nil {
		return MyCouponPage{}, err
	}
	return out, nil
}

func templateIDsOf(cs []repository.UserCoupon) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, c := range cs {
		if !seen[c.Rule.TemplateID] {
			seen[c.Rule.TemplateID] = true
			out = append(out, c.Rule.TemplateID)
		}
	}
	return out
}

// Applicable 实现 POST /coupons/applicable。
//
// 定价走 priceOrder（门店生效价、可售性校验与试算完全一样），然后逐张走
// evaluateCoupon —— 与试算里的 applicable_coupons 是同一个函数。
//
// addressID 可选（00056）：包邮券抵多少取决于运费，运费取决于地址。没带地址时
// 结果里没有包邮券（判不了），其余券型不受影响。
func (s *CouponService) Applicable(ctx context.Context, items []LineInput, storeID int64,
	addressID *int64) ([]ApplicableCoupon, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var out []ApplicableCoupon
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		sc, err := orderScope(ctx, tx, storeID)
		if err != nil {
			return err
		}
		var dest *FreightDestination
		if addressID != nil {
			addr, err := tx.FindAddress(ctx, *addressID, id.UserID)
			if errors.Is(err, repository.ErrAddressNotFound) {
				return fmt.Errorf("%w: address_id=%d", ErrAddressNotFound, *addressID)
			}
			if err != nil {
				return err
			}
			d := destinationOf(addr)
			dest = &d
		}
		q, err := priceOrder(ctx, tx, sc, dest, items, couponRequest{UserID: id.UserID, Now: now})
		if err != nil {
			return err
		}
		out, err = applicableCoupons(ctx, tx, sc, id.UserID, q, now)
		return err
	})
	return out, err
}

// ClaimableItem 是领券中心的一项。
type ClaimableItem struct {
	Template repository.ClaimableTemplate
	Scopes   []repository.CouponScope
}

// Remaining 是还剩几张；nil 表示不限量。
func (c ClaimableItem) Remaining() *int {
	if c.Template.TotalCount == 0 {
		return nil
	}
	n := int(c.Template.TotalCount - c.Template.IssuedCount)
	if n < 0 {
		n = 0
	}
	return &n
}

// ClaimablePage 是 GET /coupon-templates 的一页。
type ClaimablePage struct {
	Items    []ClaimableItem
	Total    int64
	Page     int
	PageSize int
}

// ListClaimable 实现 GET /coupon-templates。
func (s *CouponService) ListClaimable(ctx context.Context, page, pageSize int) (ClaimablePage, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return ClaimablePage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	now := s.now()
	out := ClaimablePage{Items: []ClaimableItem{}, Page: page, PageSize: pageSize}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		items, total, err := tx.ListClaimableTemplates(ctx, id.UserID, now,
			int32(pageSize), int32(offsetOf(page, pageSize)))
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(items))
		for _, it := range items {
			ids = append(ids, it.Rule.TemplateID)
		}
		scopes, err := tx.ListCouponScopes(ctx, ids)
		if err != nil {
			return err
		}
		out.Total = total
		for _, it := range items {
			out.Items = append(out.Items, ClaimableItem{Template: it, Scopes: scopes[it.Rule.TemplateID]})
		}
		return nil
	})
	if err != nil {
		return ClaimablePage{}, err
	}
	return out, nil
}

// Claim 实现 POST /coupon-templates/{template_id}/claim。
//
// 三条约束（总量、每人限领、结束时间）**全部在数据库层原子地挡住**，
// 论证在 00026 的文件头与 db/queries/coupons.sql 的 BumpTemplateForClaim 上。
// 这里只是把那几条语句按顺序放进同一个事务：
//
//  1. 占名额（条件 UPDATE，同时拿到模板行锁）；
//  2. 数这个人已经有几张（在行锁之后，看得见前一个领取者已提交的行）；
//  3. 落券实例。
//
// 任何一步失败都整体回滚，名额一起退回去 —— 包括幂等键的抢占，
// 所以客户端可以拿同一把钥匙原样重试。
func (s *CouponService) Claim(ctx context.Context, templateID int64, idemKey string) (MyCoupon, bool, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return MyCoupon{}, false, err
	}
	if idemKey == "" {
		return MyCoupon{}, false, ErrIdempotencyKeyMissing
	}
	hash, err := adminRequestHash([]int64{templateID}, nil)
	if err != nil {
		return MyCoupon{}, false, err
	}
	now := s.now()
	return idempotentTenantWrite(ctx, s.repo, couponScopeClaim, repository.BuyerSubject(id.UserID),
		idemKey, hash, archivedCreated, func(tx repository.Tx) (MyCoupon, error) {
			win, err := tx.BumpTemplateForClaim(ctx, templateID, now)
			if errors.Is(err, repository.ErrCouponTemplateExhausted) {
				return MyCoupon{}, classifyClaimFailure(ctx, tx, templateID, now, true)
			}
			if err != nil {
				return MyCoupon{}, err
			}
			held, err := tx.CountUserTemplateCoupons(ctx, templateID, id.UserID)
			if err != nil {
				return MyCoupon{}, err
			}
			if held >= win.PerUserLimit {
				return MyCoupon{}, fmt.Errorf("%w: 每人限领 %d 张，你已有 %d 张",
					ErrCouponClaimLimitReached, win.PerUserLimit, held)
			}
			cid, err := issueCoupon(ctx, tx, templateID, id.UserID, repository.CouponSourceClaim, win, now)
			if err != nil {
				return MyCoupon{}, err
			}
			return loadMyCoupon(ctx, tx, cid, id.UserID, now)
		})
}

// classifyClaimFailure 在占名额失败之后回读模板，分出契约里的几种响应。
// forClaim 为假时是定向发放：不看 claimable，停用单独报。
func classifyClaimFailure(ctx context.Context, tx repository.Tx, templateID int64,
	now time.Time, forClaim bool) error {
	st, err := tx.ClaimTemplateState(ctx, templateID)
	if errors.Is(err, repository.ErrCouponTemplateNotFound) {
		return fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, templateID)
	}
	if err != nil {
		return err
	}
	switch {
	case forClaim && (st.Status != 1 || !st.Claimable):
		return fmt.Errorf("%w: template_id=%d", ErrCouponTemplateNotFound, templateID)
	case !forClaim && st.Status != 1:
		return fmt.Errorf("%w: template_id=%d", ErrCouponTemplateDisabled, templateID)
	case st.ValidMode == 1 && st.ValidEndAt != nil && !now.Before(*st.ValidEndAt):
		return fmt.Errorf("%w: 已于 %s 结束", ErrCouponClaimEnded, st.ValidEndAt.UTC().Format(time.RFC3339))
	default:
		return fmt.Errorf("%w: 总量 %d，已发出 %d", ErrCouponSoldOut, st.TotalCount, st.IssuedCount)
	}
}

// issueCoupon 按模板的有效期模式算出这张券的窗口并落库。领取与定向发放共用。
func issueCoupon(ctx context.Context, tx repository.Tx, templateID, userID int64, source int16,
	win repository.ValidWindow, now time.Time) (int64, error) {
	var start, end time.Time
	switch win.ValidMode {
	case 1:
		if win.ValidStartAt == nil || win.ValidEndAt == nil {
			return 0, fmt.Errorf("模板 %d 是绝对时间模式却没有起止时间", templateID)
		}
		start, end = *win.ValidStartAt, *win.ValidEndAt
	case 2:
		start = now
		end = now.Add(time.Duration(win.ValidDays) * 24 * time.Hour)
	default:
		return 0, fmt.Errorf("模板 %d 的有效期模式 %d 无法识别", templateID, win.ValidMode)
	}
	code, err := newCouponCode()
	if err != nil {
		return 0, err
	}
	return tx.InsertUserCoupon(ctx, repository.NewUserCoupon{
		Code: code, TemplateID: templateID, UserID: userID, Source: source,
		ValidStartAt: start, ValidEndAt: end,
	})
}

func loadMyCoupon(ctx context.Context, tx repository.Tx, couponID, userID int64, now time.Time) (MyCoupon, error) {
	c, err := tx.FindUserCoupon(ctx, couponID, userID)
	if err != nil {
		return MyCoupon{}, err
	}
	scopes, err := tx.ListCouponScopes(ctx, []int64{c.Rule.TemplateID})
	if err != nil {
		return MyCoupon{}, err
	}
	return MyCoupon{Coupon: c, Scopes: scopes[c.Rule.TemplateID], DisplayStatus: displayStatus(c, now)}, nil
}

// couponCodeRandomBytes：券码的随机部分。8 字节 = 64 bit，全局唯一由 UNIQUE 兜底。
const couponCodeRandomBytes = 8

// newCouponCode 生成券码：C + 16 位大写十六进制。
//
// 它是「我们自己生成的不可枚举编号」（数据模型 §2 那条分界线），全局唯一。
// 熵源坏了就报错，绝不回落到时间戳 —— 理由同 newOrderNo。
func newCouponCode() (string, error) {
	var b [couponCodeRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成券码失败: %w", err)
	}
	return "C" + strings.ToUpper(hex.EncodeToString(b[:])), nil
}

// idempotentTenantWrite 是「抢幂等键 → 干活 → 存档」的单事务形态，主体由调用方给。
//
// 与 admin_idempotency.go 的 idempotentWrite 是同一个形状，那一个绑死在
// AdminCatalogService 与 StaffSubject 上；券这边买家（领券）与后台（建模板、定向发放）
// 两种主体都有，所以主体从参数进来。重放复用 replayArchived，存档格式一字不差。
func idempotentTenantWrite[T any](ctx context.Context, repo CouponRepository,
	scope string, subj repository.IdempotencySubject, idemKey, hash string, code int32,
	fn func(tx repository.Tx) (T, error)) (T, bool, error) {

	var zero T
	if idemKey == "" {
		return zero, false, ErrIdempotencyKeyMissing
	}
	var out T
	replayed := false
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		claimed, err := tx.ClaimIdempotencyKey(ctx, scope, subj, idemKey, hash)
		if err != nil {
			return err
		}
		if !claimed {
			v, err := replayArchived[T](ctx, tx, scope, subj, idemKey, hash)
			if err != nil {
				return err
			}
			out, replayed = v, true
			return nil
		}
		v, err := fn(tx)
		if err != nil {
			return err
		}
		body, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := tx.FinishIdempotencyKey(ctx, scope, subj, idemKey,
			repository.IdempotencySucceeded, &code, body); err != nil {
			return err
		}
		out = v
		return nil
	})
	if err != nil {
		return zero, false, err
	}
	return out, replayed, nil
}
