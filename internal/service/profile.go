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
// maxAvatarURL 契约没写；给一个宽松上限（合格的头像地址是 /api/v1/uploads/{id}，远到不了它）。
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
	var newAvatar int64
	if in.AvatarURL != nil {
		// 空串即清掉头像；否则只收本人用 POST /uploads（purpose=2）传的那一个地址，
		// 与退款凭证同一套核对（claimAvatar）。外链不收：头像是所有人可读的，
		// 一个指向外站的头像等于让我们替任意地址做展示，而且没有归属可查、没有回收可言。
		p.SetAvatar = true
		if v := strings.TrimSpace(*in.AvatarURL); v != "" {
			id, ok := uploadIDFromURL(v)
			if !ok || len(v) > maxAvatarURL {
				c.add("avatar_url", errAvatarNotOwned)
			} else {
				newAvatar = id
				p.AvatarURL = &v
			}
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
		if p.SetAvatar {
			if err := swapAvatar(ctx, tx, id.UserID, newAvatar, p.AvatarURL); err != nil {
				return err
			}
		}
		u, err = tx.UpdateProfile(ctx, id.UserID, p)
		return err
	})
	return u, err
}

// errAvatarNotOwned 是 avatar_url 不合格时点名那个字段的说明。「不存在」「别人的」「用途不对」
// 「已被回收」一律这一句 —— 分开报就是一个能探出别人传过哪些文件的预言机（与退款凭证同理）。
const errAvatarNotOwned = "只收你用 POST /uploads（purpose=2 头像）传的头像地址，形如 /api/v1/uploads/{upload_id}"

// swapAvatar 换头像的那一半引用账：核对新头像是本人的 purpose=2 上传并标成已引用，
// 旧头像（如果是本人的上传）取消引用、交给孤儿回收（upload_gc.go）。
// 与改 users.avatar_url 同一个事务（数据模型 §13「孤儿回收的安全条件」）。
//
// 先锁买家行：两个并发的换头像各读各的「旧头像」，会把对方刚设上的那一个取消引用，
// 于是一个正在用的头像 24 小时后被删掉。锁住之后两次换头像串行，后一次读到的旧头像
// 就是前一次设上的那一个。
func swapAvatar(ctx context.Context, tx repository.Tx, userID, newID int64, newURL *string) error {
	if err := tx.LockUser(ctx, userID); err != nil {
		return err
	}
	cur, err := tx.FindUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if newID != 0 {
		up, err := tx.FindUpload(ctx, newID)
		if errors.Is(err, repository.ErrUploadNotFound) {
			up = repository.Upload{}
		} else if err != nil {
			return err
		}
		if up.ID == 0 || up.Purpose != repository.UploadPurposeAvatar || up.UserID == nil || *up.UserID != userID {
			return &InvalidFieldsError{Fields: []FieldProblem{{Field: "avatar_url", Message: errAvatarNotOwned}}}
		}
		if err := tx.MarkUploadReferenced(ctx, newID); err != nil {
			if errors.Is(err, repository.ErrUploadNotFound) {
				// 读到之后、标引用之前被孤儿回收删掉了（它的条件删除先拿到了行锁）。
				return &InvalidFieldsError{Fields: []FieldProblem{{Field: "avatar_url", Message: errAvatarNotOwned}}}
			}
			return err
		}
	}
	if cur.AvatarURL == nil || (newURL != nil && *newURL == *cur.AvatarURL) {
		return nil
	}
	// 旧头像是本人的上传才取消引用；这一版之前存下的外链不是上传，什么也不做。
	// UnmarkUploadReferenced 自己只动「这个买家的 purpose=2」，所以就算旧地址指着别人的文件也碰不到。
	if oldID, ok := uploadIDFromURL(*cur.AvatarURL); ok {
		return tx.UnmarkUploadReferenced(ctx, oldID, userID)
	}
	return nil
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
