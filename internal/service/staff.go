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
)

// 后台身份的业务错误。每一个都对应契约里明写的一种响应，没有哪一个是
// 「大概是 400 吧」—— handler 那边逐条翻成状态码。
var (
	// ErrStaffBadRequest 是请求体本身不成立（缺 token、缺 email、role 不在枚举里）。
	ErrStaffBadRequest = errors.New("请求参数不合法")

	// ErrStaffTokenInvalid：一次性 token 验不过、过期了、被用过了。
	// 契约里 bootstrap 与 session 两条的 401 都是这一个。
	ErrStaffTokenInvalid = errors.New("token 无效、已过期或已被使用")

	// ErrBootstrapClosed：库里已经有平台级管理员，而且一串没用过的引导 token
	// 都没有 —— 引导通道关了。契约里那个 409。
	ErrBootstrapClosed = errors.New("引导通道已关闭")

	// ErrStaffForbidden：调用者不是管理员（契约里 /admin/staff 与
	// /admin/staff/{id} 的 403）。
	ErrStaffForbidden = errors.New("需要管理员权限")

	// ErrStaffNotFound：目标不在调用者的作用域里。
	//
	// 契约 /admin/ 那一段的约定 3：查不到当前租户名下的那一个一律 404 而不是
	// 403 —— 403 会让自增 id 空间变成一个跨租户的存在性探针。
	ErrStaffNotFound = errors.New("操作员不存在")

	// ErrStaffEmailTaken：该作用域内邮箱已存在（契约里那个 409）。
	ErrStaffEmailTaken = errors.New("该邮箱已经有人在用")

	// ErrLastAdmin：这一改会让该租户一个在岗管理员都不剩（契约里那个 409）。
	// 数据模型 §14 那条「进不了数据库的约束」，只能在这一层拦。
	ErrLastAdmin = errors.New("不能让该租户失去最后一个在岗管理员")

	// ErrEmailServiceUnavailable：邮箱登录链接这条路今天发不出去。
	//
	// 它不是「邮箱不存在」，也不是「频控」—— 本项目一个邮件服务都没接。
	// 与买家那边的 ErrSMSLoginUnavailable 完全同构，handler 翻成 501。
	ErrEmailServiceUnavailable = errors.New("邮箱登录链接暂不可用")
)

// bootstrapPlaceholderEmail 是引导账号建出来时的占位邮箱。
//
// 用 .invalid 这个 RFC 2606 保留后缀：它按定义永远不可能是一个真实地址，
// 于是「这一行的邮箱还没补」是一件看一眼就知道的事，而不是要去比对某个
// 约定俗成的字符串。POST /admin/auth/bootstrap 会把它换成本人的邮箱，
// 而那条 UPDATE 的 WHERE 里带着这个占位符 —— 也就是说它只换得掉一次。
const bootstrapPlaceholderEmail = "bootstrap@keel.invalid"

// StaffRepository 是本服务需要的仓储能力。
//
// 两个入口分别对应两种作用域，这不是冗余：平台级操作员的那些行
// merchant_id 为 NULL，租户作用域看不见它们；而租户作用域下的那些行
// 平台作用域也看不见（00017）。选哪一个由**调用者的身份**决定，
// 不由请求里的任何东西决定。
// 第三个入口 WithNewTenant 是开店专用的（merchant.go）：它建出那家店，
// 再把作用域切到**这家新店**。它不能由前两个拼出来 —— WithTenant 取的是请求
// Host 那家店，WithPlatform 里 staff_scope_merchant() 是 NULL（建出来的会是
// 一个平台级管理员）。完整论证在 repository.WithNewTenant 上。
type StaffRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	WithPlatform(ctx context.Context, fn func(repository.StaffTx) error) error
	WithNewTenant(ctx context.Context, code, name string,
		fn func(repository.Merchant, repository.StaffTx) error) (repository.Merchant, error)
}

// StaffService 实现 /admin/auth/* 与 /admin/staff* 那几条接口。
type StaffService struct {
	repo   StaffRepository
	signer *auth.Signer
	log    *slog.Logger
	now    func() time.Time
}

func NewStaffService(r StaffRepository, s *auth.Signer, log *slog.Logger) *StaffService {
	if log == nil {
		log = slog.Default()
	}
	return &StaffService{repo: r, signer: s, log: log, now: time.Now}
}

// StaffSessionResult 是一次登录的产物：一串会话 token 加上它属于谁。
type StaffSessionResult struct {
	Token    string
	ExpireAt time.Time
	Staff    repository.Staff
}

// ---------------------------------------------------------------------------
// 引导：进程启动时那一次
// ---------------------------------------------------------------------------

// EnsureBootstrapAdmin 在**库里一个在岗平台级管理员都没有**时，建一个平台级
// 管理员并签发一串 kind=1 引导 token，返回它的明文。已经有人了就返回 ""。
//
// ===========================================================================
// 「凭什么能调 POST /admin/auth/bootstrap」的答案在这个函数里，不在那条路由上
// ===========================================================================
//
// 那条接口是 security: []（未认证的）。如果它自己能凭空建出一个管理员，
// 那么任何一个能打到这个端口的人都能给自己开一个后台全权账号 —— 那不是
// 「引导」，那是一个没有门的后门。
//
// 所以职责是这么切的：
//
//	**建账号这件事只发生在进程启动时，在进程内部，不接受任何外部输入。**
//	那条接口只做一件事：拿一串**只有能读到容器 stdout 的人才见过**的
//	一次性 token，换一个会话。
//
// 于是它的准入条件是「你手里有那串 token」，而那串 token：
//
//   - 只在库里一个在岗平台管理员都没有时才会被生成（也就是每个部署一次）；
//   - 明文只出现在进程的日志里，一次，之后库里只有它的 sha256；
//   - 24 小时过期（§14），用掉即失效（used_at 一置就作废）；
//   - 用掉之后引导通道整个关闭，再打那条接口一律 409。
//
// §14 为「打印到 stdout 而不是发邮件」写过完整论证：全新部署时 SMTP 大概率
// 还没配，如果登录的唯一入口是邮件，操作员第一次就进不去后台 ——
// 而本项目的卖点是 docker compose up 一条命令。Jupyter 与 Gitea 都这么做。
// 代价是容器日志里会短暂出现一个高权限凭据，那正是上面那三条的由来。
//
// ### 判据是「没有在岗的平台级管理员」，不是「库里一个 staff 都没有」
//
// §14 的原话是后者。改成前者有两个理由：
//
//   - **后者在 RLS 之下根本观测不到。** keel_app 的任何一次 count 都只数得到
//     当前作用域里的行，而平台作用域只看得见 merchant_id 为 NULL 的那些。
//     要数出「全库有没有 staff」只能用一条能绕过 RLS 的连接，
//     而那正是 internal/db.Guard 存在的理由所要禁止的。
//   - **前者才是这件事真正要问的问题。** 引导要建的是一个平台级管理员；
//     几家店各自有自己的管理员，不代表这个部署有人能做跨租户运维。
//     用后者的话，一个「商家都在、平台管理员被误删了」的部署再也引导不回来。
//
// 代价写明白：商家级 staff 已经存在、而平台管理员不在时，这个判据会**重新
// 打开**引导通道并往日志里打一串新 token。那不是越权 —— 能读容器日志的人
// 本来就拥有这个部署。
func (s *StaffService) EnsureBootstrapAdmin(ctx context.Context) (string, error) {
	var plaintext string
	err := s.repo.WithPlatform(ctx, func(tx repository.StaffTx) error {
		state, err := tx.BootstrapChannelState(ctx, bootstrapPlaceholderEmail)
		if err != nil {
			return err
		}
		if state.Admins > 0 {
			// 已经有人真正登录过后台了。**不管有没有活着的引导 token** ——
			// 这里再签一串等于每次重启都往日志里丢一把后台全权的钥匙。
			return nil
		}
		if state.LiveTokens > 0 {
			// 还没人登录过，但上一串引导 token 还在有效期内：窗口开着，
			// 不签第二串。同一条理由 —— 重启一次多一把钥匙。
			return nil
		}

		// 走到这里只有两种情况，都等于「这个部署还没人进得了后台，也没有
		// 任何办法进去」：
		//   · 第一次启动，占位账号都还没有 —— 建一个；
		//   · 占位账号在，但它的引导 token 过期前没人用 —— **给它补签一串**。
		//
		// 第二种是第一版漏掉的：那时判据是「在岗管理员 > 0」，占位账号自己就
		// 算一个，于是 token 一过期这个部署的后台就永久锁死了（邮件登录在没接
		// SMTP 时回 501）。补签不违背上面那两条「不签」的顾虑：从来没人兑换过、
		// 也没有活着的 token，这个部署在鉴权上仍然是「刚装好」的状态，和第一次
		// 启动一模一样。
		staffID := state.PlaceholderID
		if staffID == 0 {
			st, err := tx.CreateStaff(ctx, bootstrapPlaceholderEmail, "", auth.StaffRoleAdmin, nil)
			if err != nil {
				return err
			}
			staffID = st.ID
		}
		token, err := auth.NewOpaqueToken()
		if err != nil {
			return err
		}
		if _, err := tx.CreateStaffToken(ctx, staffID, auth.HashStaffToken(token),
			repository.StaffTokenBootstrap, s.now().UTC().Add(auth.StaffBootstrapTTL)); err != nil {
			return err
		}
		plaintext = token
		return nil
	})
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// ---------------------------------------------------------------------------
// 三条 /admin/auth/*
// ---------------------------------------------------------------------------

// Bootstrap 实现 POST /admin/auth/bootstrap：拿引导 token 换会话，并补上邮箱。
//
// 全程在**平台作用域**里：引导账号按定义是平台级的（merchant_id 为 NULL），
// 租户作用域里那一行根本不可见。请求打在哪个 Host 上与它无关。
func (s *StaffService) Bootstrap(ctx context.Context, token, email string) (StaffSessionResult, error) {
	token, email = strings.TrimSpace(token), strings.TrimSpace(email)
	if token == "" {
		return StaffSessionResult{}, fmt.Errorf("%w: 缺 token", ErrStaffBadRequest)
	}
	if email == "" {
		// 契约里 email 是 required：引导账号必须在这一步补上找回通道，
		// 否则这个部署的后台就只剩一串会过期的会话 token，丢了就再也进不去。
		return StaffSessionResult{}, fmt.Errorf("%w: 缺 email", ErrStaffBadRequest)
	}
	if email == bootstrapPlaceholderEmail {
		// 拿占位符当自己的邮箱提交，等于把「只能补一次」这条规则绕过去：
		// 那条 UPDATE 的 WHERE 里比的就是它，补成它自己之后还能再补一次。
		return StaffSessionResult{}, fmt.Errorf("%w: 这个邮箱是占位符，请填自己的", ErrStaffBadRequest)
	}

	var out StaffSessionResult
	err := s.repo.WithPlatform(ctx, func(tx repository.StaffTx) error {
		hit, err := tx.FindLiveOneTimeToken(ctx, auth.HashStaffToken(token),
			repository.StaffTokenBootstrap)
		if errors.Is(err, repository.ErrStaffTokenNotFound) {
			// token 对不上。回 401 还是 409，取决于引导窗口开没开：
			// 窗口还开着的时候一串错 token 就只是一串错 token；
			// 窗口关了之后任何 token 都只说明「引导通道已关闭」，
			// 而那是契约里 409 的原话。
			// Admins 不含占位账号：占位账号在、token 过期了的时候，这里回的是 401
			// 而不是 409「通道已关闭」—— 通道并没有关，重启一次就会补签。
			state, serr := tx.BootstrapChannelState(ctx, bootstrapPlaceholderEmail)
			if serr != nil {
				return serr
			}
			if state.LiveTokens == 0 && state.Admins > 0 {
				return ErrBootstrapClosed
			}
			return ErrStaffTokenInvalid
		}
		if err != nil {
			return err
		}

		// 用掉即失效。**先消费再签会话**：反过来的话，两个并发请求都能拿到
		// 会话，而 ConsumeStaffToken 的 :one 正是为了让第二个在这里失败。
		if err := tx.ConsumeStaffToken(ctx, hit.TokenID); err != nil {
			if errors.Is(err, repository.ErrStaffTokenNotFound) {
				return ErrStaffTokenInvalid
			}
			return err
		}

		st, err := tx.SetStaffEmail(ctx, hit.Staff.ID, email, bootstrapPlaceholderEmail)
		if errors.Is(err, repository.ErrStaffNotFound) {
			// 邮箱已经被补过了，而引导 token 却还活着 —— 那说明有人手工
			// 动过库，或者引导流程被跑了两遍。不要把它当成成功：
			// 这条路的产物是一把后台全权的钥匙。
			s.log.WarnContext(ctx, "引导 token 有效，但那个账号的邮箱已经补过了",
				"staff_id", hit.Staff.ID)
			return ErrStaffTokenInvalid
		}
		if err != nil {
			if errors.Is(err, repository.ErrStaffEmailTaken) {
				return ErrStaffEmailTaken
			}
			return err
		}

		out, err = s.issueSession(ctx, tx, st)
		return err
	})
	if err != nil {
		return StaffSessionResult{}, err
	}
	return out, nil
}

// RequestEmailLink 实现 POST /admin/auth/email-link。
//
// ===========================================================================
// 本轮它返回 501，而不是一个假装受理了的 202
// ===========================================================================
//
// 契约说「无论邮箱是否存在都返回 202 —— 用返回值区分这个邮箱注册过没有
// 等于送给攻击者一个账号枚举接口」。那条推理对，而且本实现没有推翻它：
// 这里**连查都不查**，不管邮箱是什么都走同一条路，所以枚举面依然是零。
//
// 但 202 的含义是「已受理」，而本项目一个邮件服务都没接。回 202 是一句
// 在邮件服务上线之前都不会被纠正的假话：操作员会去收件箱等一封永远不来的信，
// 而服务端这一侧没有任何东西显示出问题 —— 没有失败的任务、没有错误日志、
// 没有一条待发队列。
//
// 所以走买家那边验证码登录的同一条路（service.ErrSMSLoginUnavailable →
// 501 + contract_test.go 里一笔两个方向都锁住的欠账）：说清楚「这条路
// 服务端还没通」，而不是假装失败或假装成功。
//
// 本轮后台仍然进得去，因为另外两条入口是通的：引导 token 走 stdout，
// 新员工的登录链接由 POST /admin/staff 打进进程日志（见 CreateStaff）。
// 两条都是 §14 为「SMTP 还没配」准备的同一个办法。
func (s *StaffService) RequestEmailLink(ctx context.Context, email string) error {
	if strings.TrimSpace(email) == "" {
		return fmt.Errorf("%w: 缺 email", ErrStaffBadRequest)
	}
	return ErrEmailServiceUnavailable
}

// ExchangeEmailLink 实现 POST /admin/auth/session：拿一次性链接 token 换会话。
//
// ===========================================================================
// 为什么这里要试两个作用域，以及那不是一个洞
// ===========================================================================
//
// 一串 kind=2 的 token 可能属于一个商家级操作员，也可能属于一个平台级操作员，
// 而它是**不透明**的 —— 里面没有任何可读字段（auth.NewOpaqueToken），
// 所以在查库之前没有任何办法知道该开哪个作用域。
//
// 先试请求 Host 那家店的租户作用域，再试平台作用域。两次都是按 token_hash
// 的点查（那一列全局唯一），拿不到 token 的人一次也打不动。
//
// **跨店那条线仍然是关着的**，而且守它的是 RLS 本身：A 店的链接 token 在
// B 店的租户作用域里查不到（staff_tokens 的策略要求它的 staff 属于本作用域），
// 落到平台作用域也查不到（那一行的 merchant_id 不是 NULL），于是 401。
//
// 这条拒绝确实是「查不到」形态的，而不是会话 token 那种「令牌里的租户对不上」
// 的鉴权语义拒绝 —— 这里必须说明白：一次性 token 是不透明的，它**没有**
// 一个可以拿来比对的租户声明，所以那种拒绝在这条路上无从谈起。
// token.go 文件头反对的是「明明有可比对的声明却不比、靠查不到来兜」，
// 而不是这种情况。
func (s *StaffService) ExchangeEmailLink(ctx context.Context, token string) (StaffSessionResult, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return StaffSessionResult{}, fmt.Errorf("%w: 缺 token", ErrStaffBadRequest)
	}
	hash := auth.HashStaffToken(token)

	out, err := s.exchangeIn(ctx, false, hash)
	if errors.Is(err, ErrStaffTokenInvalid) {
		out, err = s.exchangeIn(ctx, true, hash)
	}
	return out, err
}

// exchangeIn 在一个作用域里做「找到 → 消费 → 签会话」。
func (s *StaffService) exchangeIn(ctx context.Context, platform bool, hash string) (StaffSessionResult, error) {
	var out StaffSessionResult
	err := s.inScope(ctx, platform, func(tx repository.StaffTx) error {
		hit, err := tx.FindLiveOneTimeToken(ctx, hash, repository.StaffTokenEmailLink)
		if errors.Is(err, repository.ErrStaffTokenNotFound) {
			return ErrStaffTokenInvalid
		}
		if err != nil {
			return err
		}
		if hit.Staff.Status != auth.StaffStatusActive {
			// 被停用的人点开一封旧链接。契约里这条接口只有 200 / 401，
			// 所以这里回 401 而不是 403 —— 而且那也是对的：
			// 一个停用账号「登录失败」是事实，把「你被停用了」告诉一个
			// 未认证的调用方反而是多说了。
			return ErrStaffTokenInvalid
		}
		if err := tx.ConsumeStaffToken(ctx, hit.TokenID); err != nil {
			if errors.Is(err, repository.ErrStaffTokenNotFound) {
				return ErrStaffTokenInvalid
			}
			return err
		}
		out, err = s.issueSession(ctx, tx, hit.Staff)
		return err
	})
	if err != nil {
		return StaffSessionResult{}, err
	}
	return out, nil
}

// LoadStaffSession 实现 auth.StaffSessionLoader：中间件每个请求调一次。
//
// 作用域由**签名里的声明**决定，不由请求的 Host 决定。这一点是跨店防护的
// 另一半：中间件已经比过「令牌里的租户 == 本请求的租户」，所以这里按声明
// 开作用域与按 Host 开作用域是同一个值 —— 而万一哪天那条比对被人删掉，
// 这里仍然会在令牌自己声称的那家店里查，查不到就 401，
// 不会变成「拿 A 店的令牌在 B 店的上下文里查出一个撞了 id 的管理员」。
func (s *StaffService) LoadStaffSession(ctx context.Context, claims auth.StaffClaims,
	rawToken string) (auth.StaffIdentity, error) {

	var out auth.StaffIdentity
	err := s.inScopeFor(ctx, claims, func(tx repository.StaffTx) error {
		sess, err := tx.TouchLiveStaffSession(ctx, auth.HashStaffToken(rawToken))
		if errors.Is(err, repository.ErrStaffTokenNotFound) {
			return ErrStaffTokenInvalid
		}
		if err != nil {
			return err
		}
		if sess.StaffID != claims.StaffID {
			// 签名里的人和库里那一行的人对不上。两者是同一次签发写下的，
			// 真对不上说明签名密钥泄露或者会话表被改过 —— 那不是「登录失效」
			// 这种小事，所以它要有自己的日志（同 AuthService.Refresh）。
			s.log.ErrorContext(ctx, "后台会话的 staff_id 与令牌声明不一致",
				"token_staff_id", claims.StaffID, "session_staff_id", sess.StaffID)
			return ErrStaffTokenInvalid
		}
		out = auth.StaffIdentity{
			StaffID:    sess.StaffID,
			SessionID:  sess.SessionID,
			MerchantID: sess.MerchantID,
			Role:       sess.Role,
			Status:     sess.Status,
		}
		return nil
	})
	if err != nil {
		return auth.StaffIdentity{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// /admin/me 与 /admin/staff*
// ---------------------------------------------------------------------------

// Me 实现 GET /admin/me。
func (s *StaffService) Me(ctx context.Context) (repository.Staff, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return repository.Staff{}, err
	}
	var out repository.Staff
	err = s.inScope(ctx, id.Platform(), func(tx repository.StaffTx) error {
		st, err := tx.FindStaff(ctx, id.StaffID)
		if errors.Is(err, repository.ErrStaffNotFound) {
			return ErrStaffNotFound
		}
		out = st
		return err
	})
	if err != nil {
		return repository.Staff{}, err
	}
	return out, nil
}

// StaffPage 是员工列表那一页。
type StaffPage struct {
	Items    []repository.Staff
	Total    int64
	Page     int
	PageSize int
}

// ListStaff 实现 GET /admin/staff。
//
// 「平台级看见平台操作员，商家级只看见自己店的」——这句话在这个函数里
// **一个 if 都没有**，它由作用域与 RLS 给出。写成 if 的话，那条 SQL 就得带上
// merchant_id，而那正是 scripts/check_query_tenancy.py 挡的东西：
// 应用层再过滤一遍之后，「RLS 到底有没有生效」就再也测不出来了。
func (s *StaffService) ListStaff(ctx context.Context, page, pageSize int) (StaffPage, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return StaffPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)

	out := StaffPage{Page: page, PageSize: pageSize}
	err = s.inScope(ctx, id.Platform(), func(tx repository.StaffTx) error {
		total, err := tx.CountStaff(ctx)
		if err != nil {
			return err
		}
		items, err := tx.ListStaff(ctx, int64(pageSize), offsetOf(page, pageSize))
		if err != nil {
			return err
		}
		out.Total, out.Items = total, items
		return nil
	})
	if err != nil {
		return StaffPage{}, err
	}
	return out, nil
}

// StaffCreated 是新建一个员工的产物。
type StaffCreated struct {
	Staff repository.Staff
	// LoginToken 是给他的一次性登录链接 token 的明文。
	//
	// 它**不进响应体**（契约里 201 的 schema 是 Staff，没有这个字段，
	// 而且把别人的登录凭据回给创建者等于让管理员能冒充任何一个员工）。
	// 本轮没有邮件服务，所以它进进程日志 —— 与引导 token 同一个办法，
	// 同一个理由（§14 那段「打印到 stdout 而不是发邮件」）。
	LoginToken string
}

// CreateStaff 实现 POST /admin/staff。
//
// **新员工的租户归属从调用者的会话继承，不接受请求体传入**（契约与 §14
// 认证流程 ④ 都写着这一条）。这里的落地方式比「不读请求体里那个字段」更硬：
// 那一列的值由数据库的 DEFAULT staff_scope_merchant() 给出（00017），
// 而作用域由调用者的身份决定 —— 整条链路上没有一个地方有 merchant_id 这个
// 参数可以传错，包括生成的 Go 函数签名。
func (s *StaffService) CreateStaff(ctx context.Context, email, name string, role int16) (StaffCreated, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return StaffCreated{}, err
	}
	if !id.IsAdmin() {
		return StaffCreated{}, ErrStaffForbidden
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return StaffCreated{}, fmt.Errorf("%w: 缺 email", ErrStaffBadRequest)
	}
	if role != auth.StaffRoleAdmin && role != auth.StaffRoleOperator {
		return StaffCreated{}, fmt.Errorf("%w: role 只能是 1 或 2", ErrStaffBadRequest)
	}

	var out StaffCreated
	err = s.inScope(ctx, id.Platform(), func(tx repository.StaffTx) error {
		creator := id.StaffID
		st, err := tx.CreateStaff(ctx, email, strings.TrimSpace(name), role, &creator)
		if err != nil {
			if errors.Is(err, repository.ErrStaffEmailTaken) {
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
		out = StaffCreated{Staff: st, LoginToken: token}
		return nil
	})
	if err != nil {
		return StaffCreated{}, err
	}
	return out, nil
}

// UpdateStaff 实现 PATCH /admin/staff/{staff_id}。
func (s *StaffService) UpdateStaff(ctx context.Context, staffID int64, role, status *int16) (repository.Staff, error) {
	id, err := auth.StaffFromContext(ctx)
	if err != nil {
		return repository.Staff{}, err
	}
	if !id.IsAdmin() {
		return repository.Staff{}, ErrStaffForbidden
	}
	if role == nil && status == nil {
		// 契约里请求体是 minProperties: 1。一个什么都不改的 PATCH 回 200
		// 会让客户端以为它改成功了。
		return repository.Staff{}, fmt.Errorf("%w: role 与 status 至少给一个", ErrStaffBadRequest)
	}
	if role != nil && *role != auth.StaffRoleAdmin && *role != auth.StaffRoleOperator {
		return repository.Staff{}, fmt.Errorf("%w: role 只能是 1 或 2", ErrStaffBadRequest)
	}
	if status != nil && *status != auth.StaffStatusActive && *status != auth.StaffStatusDisabled {
		return repository.Staff{}, fmt.Errorf("%w: status 只能是 1 或 2", ErrStaffBadRequest)
	}

	var out repository.Staff
	err = s.inScope(ctx, id.Platform(), func(tx repository.StaffTx) error {
		// 先读一次：要知道这个人现在是不是在岗管理员，才能判断这一改会不会
		// 把最后一个管理员拿掉。**「只能改同租户内的人」不在这个 if 里** ——
		// 别的租户那一行在本作用域里根本查不出来，于是这里直接是 404。
		before, err := tx.FindStaff(ctx, staffID)
		if errors.Is(err, repository.ErrStaffNotFound) {
			return ErrStaffNotFound
		}
		if err != nil {
			return err
		}

		// 数据模型 §14 那条进不了数据库的约束：每个租户至少要有一个在岗管理员
		// （平台级那一层同理）。降级与停用都会踩到它，所以判据按「改完之后
		// 他还算不算一个在岗管理员」算，而不是分别判 role 和 status。
		wasAdmin := before.Role == auth.StaffRoleAdmin && before.Status == auth.StaffStatusActive
		stillAdmin := wasAdmin
		if role != nil {
			stillAdmin = stillAdmin && *role == auth.StaffRoleAdmin
		}
		if status != nil {
			stillAdmin = stillAdmin && *status == auth.StaffStatusActive
		}
		if wasAdmin && !stillAdmin {
			others, err := tx.CountOtherLiveAdmins(ctx, staffID)
			if err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}

		st, err := tx.UpdateStaffRoleStatus(ctx, staffID, role, status)
		if errors.Is(err, repository.ErrStaffNotFound) {
			return ErrStaffNotFound
		}
		out = st
		return err
	})
	if err != nil {
		return repository.Staff{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 内部
// ---------------------------------------------------------------------------

// inScope 在调用者的作用域里跑一个事务。
//
// 一个函数而不是两处 if：作用域选错的后果在两个方向上都很重 ——
// 平台级请求走租户作用域会查不到自己（症状像「登录失效」），
// 商家级请求走平台作用域会看见别人的平台操作员。合成一处之后，
// 「按调用者的身份选作用域」这条规则只有一份实现。
func (s *StaffService) inScope(ctx context.Context, platform bool, fn func(repository.StaffTx) error) error {
	if platform {
		return s.repo.WithPlatform(ctx, fn)
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error { return fn(tx) })
}

// inScopeFor 按令牌里的声明选作用域，给中间件那一次查库用。
func (s *StaffService) inScopeFor(ctx context.Context, claims auth.StaffClaims,
	fn func(repository.StaffTx) error) error {
	return s.inScope(ctx, claims.Platform, fn)
}

// issueSession 签一串会话 token 并把它的 sha256 写进 staff_tokens。
//
// 顺序与买家那边 issue 的 refresh_token 一致：**先签、再拿它的 hash 建行**。
// 会话的身份就是这串 token 的哈希，所以顺序不能反。
func (s *StaffService) issueSession(ctx context.Context, tx repository.StaffTx,
	st repository.Staff) (StaffSessionResult, error) {

	token, err := s.signer.IssueStaff(st.MerchantID, st.ID, auth.StaffSessionTTL)
	if err != nil {
		return StaffSessionResult{}, err
	}
	expireAt := s.now().UTC().Add(auth.StaffSessionTTL)
	if _, err := tx.CreateStaffToken(ctx, st.ID, auth.HashStaffToken(token),
		repository.StaffTokenSession, expireAt); err != nil {
		return StaffSessionResult{}, err
	}
	if err := tx.TouchStaffLogin(ctx, st.ID); err != nil {
		return StaffSessionResult{}, err
	}
	// 返回的 staff 用刚才读到的那一份，但 last_login_at 已经被上面那句改掉了。
	// 不为此再查一次库：契约里 last_login_at 是可选字段，而「这一次登录」
	// 的时间对刚登录的人没有信息量。
	return StaffSessionResult{Token: token, ExpireAt: expireAt, Staff: st}, nil
}
