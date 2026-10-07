package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 开店（契约 POST /admin/merchants）。M4 Task 4。
//
// 它挂在 StaffService 上而不是自己一个服务，理由是它做的事有 **三分之二**
// 已经在这里了：建一个管理员、签一串一次性登录链接 token、把明文交给
// handler。那三步与 CreateStaff 逐字相同，而它们分叉的那天
// （比如登录链接的 TTL 改了一处没改另一处）不会有任何东西变红。
// 分叉的是**明文去哪儿**：CreateStaff 那一串进日志之外的地方都没有（要它就重签），
// 这一串进 201 的响应体（理由见 ShopOpened.LoginToken 与契约 MerchantOpened）。
//
// 不同的只有第一步：这家店此刻还不存在。

// ErrPlatformOnly：这条接口只有**平台级管理员**能调（契约里那个 403）。
//
// 与 ErrStaffForbidden 分开，因为它们说的不是同一件事：后者是「你不是管理员」，
// 这一个是「你是管理员，但你是某一家店的管理员」。合成一个的话，商家老板
// 调开店接口会收到「需要管理员权限」—— 一句让他去检查自己角色的假话。
var ErrPlatformOnly = errors.New("只有平台级管理员能开店")

// merchantCodePattern 与契约 MerchantCreateRequest.code 的 pattern 逐字一致。
//
// **服务端必须自己校一遍**，不能指望客户端遵守契约：这一列会变成域名的一段
// （{code}.KEEL_BASE_DOMAIN）与路径的一段（/s/{code}）。放一个带点或带斜杠的
// code 进库，租户解析那一层就再也解不出它来 —— 而那时店已经建好了，
// 应用侧在 merchants 上没有 UPDATE 也没有 DELETE，改不了也删不掉。
var merchantCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}$`)

// ShopOpened 是开一家店的产物。
type ShopOpened struct {
	Merchant repository.Merchant

	// Admin 是这家店的第一个管理员（role = 1）。
	Admin repository.Staff

	// LoginToken 是给他的一次性登录链接 token 的明文。
	//
	// 它**进开店那条的 201 响应体**（契约 MerchantOpened）。与 StaffCreated 那条
	// 相反，理由是这条多一层「别人够不着」：平台会话看不见商家的员工
	// （`POST /admin/staff/{id}/login-token` 对一个商家级 staff 回 404），
	// 而这家新店除了他没有别人 —— 不进响应体的话，把门交给店主的唯一路子就是翻容器日志。
	//
	// 但它**不进幂等存档**：archiveIdempotent 存的是 repository.Merchant 那一份，
	// 一次性凭据的明文不进数据库。所以重放时这两个字段是空的，handler 据此决定给不给。
	LoginToken string

	// LoginTokenExpireAt 是那一串的失效时间（auth.StaffEmailLinkTTL，15 分钟）。
	// 带着它而不是让 handler 自己再算一遍：那等于两处各写一份 TTL。
	LoginTokenExpireAt time.Time
}

// OpenShop 实现 POST /admin/merchants。
//
// 调用者必须是**平台级**（merchant_id 为空）**且 role = 1**，契约原话。
// 两个条件缺一不可，而且它们挡的是两件事：
//
//   - 不是平台级 → 商家管理员能开店，等于任何一个租户都能给自己再开一家店，
//     而店的数量是这个部署的容量维度。
//   - 不是管理员 → 平台级操作员（role = 2）能开店。§14 把平台级也分了两层，
//     开店是那一层里最重的一个动作。
//
// **鉴权在这一层，不在 handler。** handler 那边只有一次
// auth.StaffFromContext，而「谁能调这条接口」是业务规则不是传输细节；
// 放在 handler 里的话，下一条平台级接口的作者要么重抄一遍，要么忘了抄。
//
// ===========================================================================
// 幂等（00028 之后）
// ===========================================================================
//
// 存档落在平台作用域（idempotency_keys 里 merchant_id 为 NULL 的那一抽屉），
// 抢占与存档都在 repository.WithNewTenant 的 ① 平台作用域那一段里做，
// 与建店同一个事务 —— 理由写在 WithNewTenant 的 guard 那一段。
// 存的是 Merchant（契约 201 响应体 `MerchantOpened` 的那一份底子），
// **不含**第一个管理员的登录 token：重放不签第二串，也不回放第一串
// （那串明文不能进数据库）。所以重放时 handler 手上只有店，没有凭据。
//
// 第二个返回值为 true 表示这是一次重放。
func (s *StaffService) OpenShop(ctx context.Context, code, name, adminEmail, idemKey string) (ShopOpened, bool, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return ShopOpened{}, false, err
	}
	if !id.Platform() || !id.IsAdmin() {
		return ShopOpened{}, false, fmt.Errorf("%w（当前身份：platform=%v role=%d）",
			ErrPlatformOnly, id.Platform(), id.Role)
	}
	if idemKey == "" {
		return ShopOpened{}, false, ErrIdempotencyKeyMissing
	}

	code = strings.TrimSpace(code)
	if !merchantCodePattern.MatchString(code) {
		return ShopOpened{}, false, fmt.Errorf(
			"%w: code %q 不满足契约的 ^[a-z0-9][a-z0-9-]{1,30}$", ErrStaffBadRequest, code)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ShopOpened{}, false, fmt.Errorf("%w: 缺 name", ErrStaffBadRequest)
	}
	adminEmail = strings.TrimSpace(adminEmail)
	if adminEmail == "" {
		return ShopOpened{}, false, fmt.Errorf("%w: 缺 admin_email", ErrStaffBadRequest)
	}
	hash, err := adminRequestHash(nil, struct {
		Code       string `json:"code"`
		Name       string `json:"name"`
		AdminEmail string `json:"admin_email"`
	}{code, name, adminEmail})
	if err != nil {
		return ShopOpened{}, false, err
	}
	subj := repository.StaffSubject(id.StaffID)

	var replay repository.Merchant
	guard := repository.NewTenantGuard{
		Claim: func(tx repository.IdempotencyTx) (bool, error) {
			claimed, err := tx.ClaimIdempotencyKey(ctx, scopeAdminMerchantCreate, subj, idemKey, hash)
			if err != nil || claimed {
				return claimed, err
			}
			v, err := replayArchived[repository.Merchant](ctx, tx,
				scopeAdminMerchantCreate, subj, idemKey, hash)
			if err != nil {
				return false, err
			}
			replay = v
			return false, nil
		},
		Archive: func(m repository.Merchant, tx repository.IdempotencyTx) error {
			return archiveIdempotent(ctx, tx, scopeAdminMerchantCreate, subj, idemKey, archivedCreated, m)
		},
	}

	var out ShopOpened
	creator := id.StaffID
	m, created, err := s.repo.WithNewTenant(ctx, code, name, guard,
		func(m repository.Merchant, tx repository.StaffTx) error {
			// 这一段跑在**新店的**租户作用域里（repository.WithNewTenant），
			// 所以这里一个 merchant_id 都没有：那一列的值由
			// DEFAULT staff_scope_merchant() 给出。与 CreateStaff 完全同构 ——
			// 「新员工的租户从作用域继承，不从参数来」这条规则只有一份实现。
			//
			// role 写死 1 管理员：契约的字段名就叫 admin_email，而一家
			// 一个管理员都没有的店，它的员工列表谁也打不开。
			st, err := tx.CreateStaff(ctx, adminEmail, "", auth.StaffRoleAdmin, &creator)
			if err != nil {
				if errors.Is(err, repository.ErrStaffEmailTaken) {
					// 新店里一个人都没有，所以撞的只可能是
					// uk_staff_email_platform（平台级那条：email 唯一、
					// WHERE merchant_id IS NULL）—— 也就是说这个邮箱已经是
					// 某个平台操作员的了。回 409 而不是 500：调用者换一个
					// 邮箱就能成功，那是一次客户端能自己解决的失败。
					return ErrStaffEmailTaken
				}
				return err
			}
			token, err := auth.NewOpaqueToken()
			if err != nil {
				return err
			}
			expire := s.now().UTC().Add(auth.StaffEmailLinkTTL)
			if _, err := tx.CreateStaffToken(ctx, st.ID, auth.HashStaffToken(token),
				repository.StaffTokenEmailLink, expire); err != nil {
				return err
			}
			out.Admin, out.LoginToken, out.LoginTokenExpireAt = st, token, expire
			return nil
		})
	if err != nil {
		return ShopOpened{}, false, err
	}
	if !created {
		return ShopOpened{Merchant: replay}, true, nil
	}
	out.Merchant = m
	return out, false, nil
}
