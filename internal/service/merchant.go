package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 开店（契约 POST /admin/merchants）。M4 Task 4。
//
// 它挂在 StaffService 上而不是自己一个服务，理由是它做的事有 **三分之二**
// 已经在这里了：建一个管理员、签一串一次性登录链接 token、把明文交给
// handler 打进日志。那三步与 CreateStaff 逐字相同，而它们分叉的那天
// （比如登录链接的 TTL 改了一处没改另一处）不会有任何东西变红。
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
	// 与 StaffCreated.LoginToken 同一条纪律：**它不进响应体**（契约里 201 的
	// schema 是 Merchant，连 Staff 都没有），本轮没有邮件服务，所以它进进程
	// 日志。理由见 §14 那段「打印到 stdout 而不是发邮件」。
	LoginToken string
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
func (s *StaffService) OpenShop(ctx context.Context, code, name, adminEmail string) (ShopOpened, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return ShopOpened{}, err
	}
	if !id.Platform() || !id.IsAdmin() {
		return ShopOpened{}, fmt.Errorf("%w（当前身份：platform=%v role=%d）",
			ErrPlatformOnly, id.Platform(), id.Role)
	}

	code = strings.TrimSpace(code)
	if !merchantCodePattern.MatchString(code) {
		return ShopOpened{}, fmt.Errorf(
			"%w: code %q 不满足契约的 ^[a-z0-9][a-z0-9-]{1,30}$", ErrStaffBadRequest, code)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ShopOpened{}, fmt.Errorf("%w: 缺 name", ErrStaffBadRequest)
	}
	adminEmail = strings.TrimSpace(adminEmail)
	if adminEmail == "" {
		return ShopOpened{}, fmt.Errorf("%w: 缺 admin_email", ErrStaffBadRequest)
	}

	var out ShopOpened
	creator := id.StaffID
	m, err := s.repo.WithNewTenant(ctx, code, name,
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
			if _, err := tx.CreateStaffToken(ctx, st.ID, auth.HashStaffToken(token),
				repository.StaffTokenEmailLink,
				s.now().UTC().Add(auth.StaffEmailLinkTTL)); err != nil {
				return err
			}
			out.Admin, out.LoginToken = st, token
			return nil
		})
	if err != nil {
		return ShopOpened{}, err
	}
	out.Merchant = m
	return out, nil
}
