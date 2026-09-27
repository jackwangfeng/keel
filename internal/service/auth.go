package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 登录与令牌的业务错误。handler 按它们映射 HTTP 状态码，所以每一个都对应
// 契约里明写的一种响应，没有哪一个是「大概是 400 吧」。
var (
	// ErrInvalidCredentials 覆盖三件事：这个号码在本店没有账号、
	// 这个账号没有设置密码、密码不对。
	//
	// **合成一个是刻意的。** 契约里它们共用同一个 401（「验证码错误 / 密码错误 /
	// 该账号未设置密码」），而分开回的话，登录接口就成了一个不需要任何凭据的
	// 账号探测器：拿一批手机号打一遍，返回「该账号未设置密码」的就是本店用户。
	// 时间侧信道由 auth.VerifyNobody 堵住（见 password.go）。
	ErrInvalidCredentials = errors.New("手机号或口令不正确")

	// ErrAccountDisabled 对应契约里的 403（users.status = 2 封禁 / 3 注销）。
	ErrAccountDisabled = errors.New("账号已封禁或已注销")

	// ErrSMSLoginUnavailable：验证码登录这条路今天没有短信服务。
	// 它不是「参数错」，也不是「验证码错」——见 Login 里那段。
	ErrSMSLoginUnavailable = errors.New("验证码登录暂不可用")

	// ErrBadRequest 是请求体本身不成立（没给手机号、两种凭据都给了或都没给）。
	ErrBadRequest = errors.New("请求参数不合法")

	// ErrInvalidToken：refresh_token 验不过、过期了、被吊销了，或者它指向的
	// 会话已经不在了。对客户端是同一件事：重新登录。
	ErrInvalidToken = errors.New("令牌无效或已失效")

	// ErrTokenTenantMismatch：这串令牌是我们签的，但**不是签给这家店的**。
	//
	// 它与 ErrInvalidToken 分开，理由与 auth.Bearer 里那条一字不差：
	// 拒绝必须是鉴权语义的，不能退化成「在这家店查不到这个人」。
	// /auth/refresh 是公开接口，不走 bearer 中间件，所以这条校验在这里
	// **必须再做一遍** —— 那道中间件保护不到它。
	ErrTokenTenantMismatch = errors.New("令牌不属于当前商家")
)

// AuthRepository 是本服务需要的仓储能力。收接口而不是 *repository.Repo，
// 理由同 ProductRepository：这个接口上没有池、没有 Begin、没有别的出口。
type AuthRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// AuthService 实现 /auth/* 三条接口的业务规则。
type AuthService struct {
	repo   AuthRepository
	signer *auth.Signer
	log    *slog.Logger
}

func NewAuthService(r AuthRepository, s *auth.Signer, log *slog.Logger) *AuthService {
	if log == nil {
		log = slog.Default()
	}
	return &AuthService{repo: r, signer: s, log: log}
}

// LoginRequest 是 /auth/login 的入参。code 与 password 二选一（契约的 oneOf）。
type LoginRequest struct {
	Phone    string
	Code     string
	Password string
}

// Session 是签发出来的一组令牌加上它属于谁。
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int // access_token 剩余秒数，契约里是必填
	User         repository.User
}

// Login 实现 POST /auth/login。
//
// **只实现密码那一条路。** 验证码那条依赖短信服务，本项目今天没有，
// 所以它返回 ErrSMSLoginUnavailable，由 handler 翻成一个明确的 501 —— 而不是
// 「验证码错误」那个 401。两者的区别不是措辞：401 会让客户端和用户以为
// 是自己输错了，于是重试、再重试；501 说的是「这条路服务端还没通」。
// 这笔账挂在 internal/handler/contract_test.go 的 NotYetImplementedBody 里，
// 实现它的人必须把那一行划掉，否则测试红。
//
// 契约还写着「验证码登录且手机号未注册时**首登即注册**」。密码路径**没有**
// 这个语义，这里也刻意不顺手加上：那等于任何人输一个没注册过的手机号加一个
// 任意密码，就在这家店里建出一个账号——注册即是一个写路径，它该有自己的
// 频次限制与验证，而不是从登录接口的失败分支里长出来。
func (s *AuthService) Login(ctx context.Context, req LoginRequest) (Session, error) {
	phone := strings.TrimSpace(req.Phone)
	if phone == "" {
		return Session{}, fmt.Errorf("%w: 缺手机号", ErrBadRequest)
	}
	hasCode, hasPassword := req.Code != "", req.Password != ""
	switch {
	case hasCode && hasPassword:
		// 契约是 oneOf，不是 anyOf。两个都给时挑一个来用，等于让客户端
		// 在不知情的情况下走了另一条认证路径。
		return Session{}, fmt.Errorf("%w: code 与 password 只能给一个", ErrBadRequest)
	case !hasCode && !hasPassword:
		return Session{}, fmt.Errorf("%w: code 与 password 必须给一个", ErrBadRequest)
	case hasCode:
		return Session{}, ErrSMSLoginUnavailable
	}

	if err := s.checkLoginLock(ctx, phone); err != nil {
		return Session{}, err
	}

	var out Session
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		user, err := tx.FindUserByPhone(ctx, phone)
		switch {
		case errors.Is(err, repository.ErrUserNotFound):
			// 没有这个人，但**照样烧掉一次 KDF 的时间**，否则这个接口
			// 就是一个按响应时间工作的手机号枚举器（见 password.go）。
			return auth.VerifyNobody()
		case err != nil:
			return err
		}

		if user.PasswordHash == nil {
			// 仅第三方登录的账号（§9 明写 password_hash 可空）。同样要烧时间。
			return auth.VerifyNobody()
		}
		if err := auth.VerifyPassword(*user.PasswordHash, req.Password); err != nil {
			if errors.Is(err, auth.ErrPasswordMismatch) {
				return err
			}
			// 哈希读不懂：数据损坏或写入 bug。不要降级成一次普通的登录失败，
			// 那会让一整批用户「密码突然错了」而日志里一个字都没有。
			return fmt.Errorf("用户 %d 的口令哈希读不懂: %w", user.ID, err)
		}

		// 口令核对之后才看状态。反过来的话，封禁账号会在**不需要密码**的情况下
		// 回一个 403，于是任何人都能拿一批手机号问出「这个号在这家店被封了」。
		if user.Status != userStatusNormal {
			return ErrAccountDisabled
		}

		// 与签发同一个事务清掉失败记录：登录成功才算数。
		if err := tx.ClearLoginFailures(ctx, phone); err != nil {
			return err
		}
		out, err = s.issue(ctx, tx, user)
		return err
	})
	if err != nil {
		err = mapCredentialError(err)
		if errors.Is(err, ErrInvalidCredentials) {
			// 另开一个事务记失败：上面那个事务因为这次失败已经回滚了。
			s.recordLoginFailure(ctx, phone)
		}
		return Session{}, err
	}
	// 新人礼（营销活动类型 5）：首单前的买家登录成功即补发。放在登录事务**之外**、
	// 尽力而为 —— 发不出券不能让登录失败（promotion_gift.go 的文件头）。
	GrantNewBuyerGifts(ctx, s.repo, out.User.ID, time.Now(), s.log)
	return out, nil
}

// Refresh 实现 POST /auth/refresh。
//
// 这条接口在契约里是 security: []（公开的），所以 auth.Bearer 那道中间件
// **不在**它前面。于是「令牌必须属于本店」这条校验在这里要自己做一遍 ——
// 漏掉的话，A 店的 refresh_token 就能在 B 店换出 B 店的 access_token，
// 而那串新令牌是完全合法的：mid 是 B，签名是我们签的。
// 跨店的入口会从「拿旧令牌试试」变成「拿旧令牌换一把新钥匙」。
func (s *AuthService) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return Session{}, fmt.Errorf("%w: 缺 refresh_token", ErrBadRequest)
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return Session{}, err
	}

	claims, err := s.signer.ParseKind(refreshToken, auth.KindRefresh)
	if err != nil {
		return Session{}, ErrInvalidToken
	}
	if claims.MerchantID != merchantID {
		s.log.WarnContext(ctx, "刷新令牌的租户与本请求的租户不一致，拒绝",
			"token_merchant_id", claims.MerchantID, "request_merchant_id", merchantID)
		return Session{}, ErrTokenTenantMismatch
	}

	var out Session
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		sess, err := tx.FindLiveSession(ctx, auth.HashToken(refreshToken))
		if errors.Is(err, repository.ErrSessionNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if sess.UserID != claims.UserID {
			// 签名里的人和库里那一行的人对不上。这不该发生 —— 两者是同一次
			// 签发写下的。真发生了，说明签名密钥泄露或者会话表被改过，
			// 那都不是「刷新失败」这种小事，所以它要有自己的日志。
			s.log.ErrorContext(ctx, "刷新令牌的 uid 与会话行不一致",
				"token_user_id", claims.UserID, "session_user_id", sess.UserID)
			return ErrInvalidToken
		}

		user, err := tx.FindUserByID(ctx, sess.UserID)
		if errors.Is(err, repository.ErrUserNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		// 封禁与注销就发生在两次请求之间，所以刷新这一步必须重新看状态。
		// 不看的话，一个被封的账号靠刷新令牌可以一直用下去。
		if user.Status != userStatusNormal {
			return ErrAccountDisabled
		}

		out, err = s.rotate(ctx, tx, merchantID, sess.ID, auth.HashToken(refreshToken), user)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return out, nil
}

// Logout 实现 POST /auth/logout：吊销**当前这一个**会话。
//
// 当前会话是谁，由 access_token 里的 sid 说了算（auth.Identity.SessionID），
// 而那串令牌已经被 auth.Bearer 校验过租户了。所以这里不再重复那条校验 ——
// 重复一遍反而会让「到底谁负责这条校验」变成两个地方的事。
//
// 已经吊销过、或者会话根本不在，都算成功：退出登录必须是幂等的，
// 而客户端在网络抖动时会重发。
func (s *AuthService) Logout(ctx context.Context) error {
	id, err := auth.FromContext(ctx)
	if err != nil {
		return err
	}
	if id.SessionID <= 0 {
		// 令牌里没有 sid。今天签不出这种令牌，但旧版本签发的可能没有这一列。
		// 当成「没有可吊销的会话」，记一条日志 —— 静默成功会让「退出登录
		// 其实什么也没做」永远不被发现。
		s.log.WarnContext(ctx, "令牌里没有会话 id，退出登录无事可做", "user_id", id.UserID)
		return nil
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		err := tx.RevokeSession(ctx, id.SessionID)
		if errors.Is(err, repository.ErrSessionNotFound) {
			s.log.InfoContext(ctx, "退出登录：会话不存在或已吊销，按幂等处理",
				"user_id", id.UserID, "session_id", id.SessionID)
			return nil
		}
		return err
	})
}

// userStatusNormal 是 users.status 的「正常」（数据模型 §9：1正常 2封禁 3已注销）。
const userStatusNormal int16 = 1

// issue 建会话并签发两串令牌。
func (s *AuthService) issue(ctx context.Context, tx repository.Tx, user repository.User) (Session, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return Session{}, err
	}

	// 先签 refresh_token，再拿它的 sha256 建会话行：会话的身份就是这串令牌的
	// 哈希，所以顺序不能反。refresh_token 本身不带 sid（那时还没有），
	// 它靠 hash 定位自己那一行。
	refresh, err := s.signer.Issue(merchantID, user.ID, 0, auth.KindRefresh, auth.RefreshTTL)
	if err != nil {
		return Session{}, err
	}
	sessionID, err := tx.CreateSession(ctx, user.ID,
		auth.HashToken(refresh), time.Now().UTC().Add(auth.RefreshTTL))
	if err != nil {
		return Session{}, err
	}

	// access_token 带上 sid，/auth/logout 才知道该吊销哪一个会话。
	access, err := s.signer.Issue(merchantID, user.ID, sessionID, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		return Session{}, err
	}
	if err := tx.TouchUserLogin(ctx, user.ID); err != nil {
		return Session{}, err
	}
	return Session{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(auth.AccessTTL / time.Second),
		User:         user,
	}, nil
}

// rotate 在刷新时换掉会话上的 refresh_token，并签一串新的 access_token。
func (s *AuthService) rotate(ctx context.Context, tx repository.Tx,
	merchantID, sessionID int64, oldHash []byte, user repository.User) (Session, error) {

	refresh, err := s.signer.Issue(merchantID, user.ID, 0, auth.KindRefresh, auth.RefreshTTL)
	if err != nil {
		return Session{}, err
	}
	// 轮换而不是新建：一个会话行始终对应一台设备，刷新不该在库里留下一串
	// 越积越多的死行。旧 hash 在这一句之后立刻失效（见 users.sql 里的说明）。
	if err := tx.RotateSession(ctx, sessionID, oldHash,
		auth.HashToken(refresh), time.Now().UTC().Add(auth.RefreshTTL)); err != nil {
		if errors.Is(err, repository.ErrSessionNotFound) {
			return Session{}, ErrInvalidToken
		}
		return Session{}, err
	}
	access, err := s.signer.Issue(merchantID, user.ID, sessionID, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		return Session{}, err
	}
	return Session{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(auth.AccessTTL / time.Second),
		User:         user,
	}, nil
}

// mapCredentialError 把 auth 层的「口令不匹配」折进业务侧那个统一的 401。
// 折叠发生在最后一步，而不是在每个分支里各写一遍 —— 少写一处的后果是
// 某一条失败路径泄露了它自己的原因。
func mapCredentialError(err error) error {
	if errors.Is(err, auth.ErrPasswordMismatch) {
		return ErrInvalidCredentials
	}
	return err
}

// checkLoginLock 在核对口令**之前**调用：锁定中直接拒绝，连 KDF 都不跑。
func (s *AuthService) checkLoginLock(ctx context.Context, phone string) error {
	if loginLockExempt(phone) {
		return nil
	}
	var until *time.Time
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		until, e = tx.LoginLockedUntil(ctx, phone)
		return e
	})
	if err != nil {
		return err
	}
	if until != nil {
		if left := time.Until(*until); left > 0 {
			return &ErrLoginLocked{RetryAfter: left}
		}
	}
	return nil
}

// recordLoginFailure 记一次口令错误。记不上只打日志：锁定是加固，不能因为它把登录本身搞挂。
func (s *AuthService) recordLoginFailure(ctx context.Context, phone string) {
	if loginLockExempt(phone) {
		return
	}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		_, e := tx.RecordLoginFailure(ctx, phone, loginMaxFailures, loginFailWindow, loginLockFor)
		return e
	})
	if err != nil {
		s.log.ErrorContext(ctx, "记录登录失败次数出错，这一次不计入锁定", "err", err)
	}
}
