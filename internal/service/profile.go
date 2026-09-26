package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
)

// 个人信息（/me 与 /me/*，契约 User tag）。
//
// # 哪些实现了、哪些返回 501，以及为什么
//
// 契约在 /me 下有 6 个操作。判据只有一条：**这个操作要不要一个本项目没接的外部服务**。
//
//	GET    /me                     读 users           实现
//	PATCH  /me                     写 users           实现
//	GET    /me/identities          读 user_identities 实现（今天恒为空数组，但那是真的空 ——
//	                                                  表在，只是还没有任何写入口）
//	DELETE /me/identities/{p}      删 user_identities 实现（「最后一个凭据」那条跨表规则
//	                                                  只能由代码兜，数据模型 §9 写着）
//	POST   /me/identities/wechat   要拿 code 去微信换 openid / unionid → 501
//	POST   /me/phone               要校验新号码收到的短信验证码       → 501
//
// 后两条返回 501 而不是假装成功或回一个 401「验证码错误」，理由与
// ErrSMSLoginUnavailable 一字不差（auth.go）：401 会让客户端和用户以为是自己
// 输错了，于是重试、再重试；501 说的是「这条路服务端还没通」。
// 两条都挂在 internal/handler/contract_test.go 的 NotYetImplementedBody 里，
// 实现的人必须把那一行划掉，否则测试红。

var (
	// ErrWechatBindUnavailable：绑定微信要拿 code 去微信换 openid / unionid，
	// 本项目没有接微信开放平台。
	ErrWechatBindUnavailable = errors.New("绑定微信暂不可用：服务端没有接入微信开放平台")

	// ErrPhoneBindUnavailable：绑定 / 换绑手机号要校验新号码收到的短信验证码，
	// 本项目没有短信服务。与 ErrSMSLoginUnavailable 同一个缺口。
	ErrPhoneBindUnavailable = errors.New("绑定手机号暂不可用：服务端没有接入短信服务")

	// ErrLastCredential：解绑之后账号将没有任何可登录的凭据（既无密码也无其他身份）。
	// 契约 409 last-credential。数据模型 §9：这条约束跨了 users 与 user_identities
	// 两张表，CHECK 做不到，只能由代码兜 —— 就是这里。
	ErrLastCredential = errors.New("这是账号最后一个可登录的凭据，不能解绑")

	// ErrIdentityNotFound：这个买家在该 provider 下没有绑定。404。
	ErrIdentityNotFound = errors.New("没有绑定这个第三方身份")
)

// maxNickname 来自契约（PATCH /me 的 nickname maxLength: 32）。
// maxAvatarURL 契约没写；给一个宽松上限，理由同地址簿那几个。
const (
	maxNickname  = 32
	maxAvatarURL = 1024
)

// ProfileRepository 是本服务需要的仓储能力。
type ProfileRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// ProfileService 实现 /me 那一组。
type ProfileService struct{ repo ProfileRepository }

func NewProfileService(r ProfileRepository) *ProfileService { return &ProfileService{repo: r} }

// ProfilePatch 是 PATCH /me 的请求体。nil 表示不改。
type ProfilePatch struct {
	Nickname  *string
	AvatarURL *string
	Gender    *int
}

// Me 实现 GET /me。
//
// 买家已注销（软删）时返回 repository.ErrUserNotFound，handler 翻成 401：
// access_token 是无状态的，注销之后它在过期前仍然验得过签（数据模型 §9 买家会话
// 写着这个窗口），而「令牌指向的人已经不在了」对客户端就是「请重新登录」。
func (s *ProfileService) Me(ctx context.Context) (repository.User, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.User{}, err
	}
	var u repository.User
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		u, err = tx.FindUserByID(ctx, id.UserID)
		return err
	})
	return u, err
}

// Update 实现 PATCH /me。
func (s *ProfileService) Update(ctx context.Context, in ProfilePatch) (repository.User, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return repository.User{}, err
	}
	var c fieldChecker
	var p repository.ProfilePatch
	if in.Nickname == nil && in.AvatarURL == nil && in.Gender == nil {
		// 契约 minProperties: 1。
		c.add("body", "nickname / avatar_url / gender 至少给一个")
	}
	if in.Nickname != nil {
		n := c.text("nickname", *in.Nickname, true, maxNickname)
		p.Nickname = &n
	}
	if in.AvatarURL != nil {
		// 空串即清掉头像。不校验它是不是一个我们自己的上传地址：契约里它就是一个
		// string，而客户端可能用微信头像的外链。它只会被原样回显给本人。
		p.SetAvatar = true
		p.AvatarURL = c.optText("avatar_url", in.AvatarURL, maxAvatarURL, nil)
		if p.AvatarURL != nil && strings.ContainsAny(*p.AvatarURL, " \t\r\n") {
			c.add("avatar_url", "不能包含空白字符")
		}
	}
	if in.Gender != nil {
		if *in.Gender < 0 || *in.Gender > 2 {
			c.add("gender", "只能是 0 / 1 / 2")
		} else {
			g := int16(*in.Gender)
			p.Gender = &g
		}
	}
	if err := c.err(); err != nil {
		return repository.User{}, err
	}
	var u repository.User
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		u, err = tx.UpdateProfile(ctx, id.UserID, p)
		return err
	})
	return u, err
}

// Identities 实现 GET /me/identities。
func (s *ProfileService) Identities(ctx context.Context) ([]repository.Identity, error) {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	var out []repository.Identity
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		out, err = tx.ListIdentities(ctx, id.UserID)
		return err
	})
	return out, err
}

// validProvider 与契约 IdentityProvider 的 enum 一致。
func validProvider(p int) bool { return p >= 1 && p <= 5 }

// Unbind 实现 DELETE /me/identities/{provider}。
//
// 锁买家行 → 删 → 判「还剩不剩凭据」→ 不剩就返回错误让整个事务回滚。
// 锁是为了两个并发的解绑（各解一个 provider）不会各自数到「还剩一个」然后都删掉。
// 「凭据」按契约的口径数：密码或其他第三方身份。手机号不算 —— 没有密码时它只能
// 走验证码登录，而那条路今天是 501（auth.go 的 ErrSMSLoginUnavailable）。
func (s *ProfileService) Unbind(ctx context.Context, provider int) error {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return err
	}
	if !validProvider(provider) {
		// 契约 IdentityProvider 之外的值不可能指向任何一条绑定：与「没有这个绑定」同一个 404。
		return fmt.Errorf("%w: provider=%d", ErrIdentityNotFound, provider)
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.LockUser(ctx, id.UserID); err != nil {
			return err
		}
		if err := tx.DeleteIdentities(ctx, id.UserID, int16(provider)); err != nil {
			if errors.Is(err, repository.ErrIdentityNotFound) {
				return ErrIdentityNotFound
			}
			return err
		}
		u, err := tx.FindUserByID(ctx, id.UserID)
		if err != nil {
			return err
		}
		if u.PasswordHash != nil {
			return nil
		}
		others, err := tx.CountIdentitiesExcept(ctx, id.UserID, int16(provider))
		if err != nil {
			return err
		}
		if others == 0 {
			return ErrLastCredential
		}
		return nil
	})
}

// BindWechat 实现 POST /me/identities/wechat —— 本期 501，见文件头。
func (s *ProfileService) BindWechat(ctx context.Context) error {
	if _, err := auth.FromContext(ctx); err != nil {
		return err
	}
	return ErrWechatBindUnavailable
}

// BindPhone 实现 POST /me/phone —— 本期 501，见文件头。
func (s *ProfileService) BindPhone(ctx context.Context) error {
	if _, err := auth.FromContext(ctx); err != nil {
		return err
	}
	return ErrPhoneBindUnavailable
}
