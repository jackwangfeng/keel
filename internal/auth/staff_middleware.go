package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/tenant"
)

// staffCtxKey 与 ctxKey 是两个**不同**的未导出类型，所以买家身份与后台身份
// 在 context 里互不可见、也互不覆盖。
//
// 这一点不是洁癖：契约约定 6 说后台接口一律挂在 /admin/ 前缀下、与前台分开
// 鉴权，而共用一个 key 的话，一条同时挂了两道中间件的路由会让后写的那个
// 把先写的顶掉，症状是「某条 /admin/ 接口把 staff_id 当成 user_id 用」。
type staffCtxKey struct{}

// ErrNoStaff 表示上下文里没有当前后台身份。
//
// 与 ErrNoUser 一样，它总是一个 bug（StaffBearer 没挂），不是可恢复的业务
// 错误。调用方该做的是让请求失败，而不是回落到某个默认操作员 ——
// 那意味着一个未认证的请求能以某个管理员的身份改价、发退款。
var ErrNoStaff = errors.New("请求上下文中没有当前后台身份；staff 鉴权中间件是否未挂载？")

// StaffIdentity 是「这个后台请求是谁发的」。
//
// MerchantID 是 *int64：nil = 平台级操作员（数据模型 §14）。不压成 0，
// 理由与 repository.Staff 那一处一字不差。
//
// Role 与 Status 在这里，是因为它们**每个请求都从库里重读**
// （TouchLiveStaffSession 连带取出）：一个被降级或停用的操作员必须在下一个
// 请求就失去权限，而会话是 7 天的。把它们放进令牌会让「已经把他降成操作员了」
// 这句话在一周之内都是假的。
type StaffIdentity struct {
	StaffID    int64
	SessionID  int64
	MerchantID *int64
	Role       int16
	Status     int16
}

// Platform 回答这个操作员是不是平台级的。
func (s StaffIdentity) Platform() bool { return s.MerchantID == nil }

// IsAdmin 回答这个操作员是不是管理员（role = 1，§14）。
//
// 「在各自层级内生效」（平台级的管理员能加平台操作员，商家级的管理员只能加
// 自己店的员工）不在这个判据里 —— 那一层由作用域与 RLS 保证，而不是由
// 一个布尔值保证。
func (s StaffIdentity) IsAdmin() bool { return s.Role == StaffRoleAdmin }

// 角色与状态取值，与数据模型 §14 的注释逐值一致。
const (
	StaffRoleAdmin    int16 = 1
	StaffRoleOperator int16 = 2

	StaffStatusActive   int16 = 1
	StaffStatusDisabled int16 = 2
)

// NewStaffContext 由 StaffBearer 调用。
func NewStaffContext(ctx context.Context, id StaffIdentity) context.Context {
	return context.WithValue(ctx, staffCtxKey{}, id)
}

// StaffFromContext 取当前后台身份。取不到时返回 ErrNoStaff，绝不返回零值：
// 零值的 Role 是 0，既不是管理员也不是操作员，而零值的 MerchantID 是 nil ——
// 也就是**平台级**。一个悄悄回落的零值等于把匿名请求当成平台管理员。
func StaffFromContext(ctx context.Context) (StaffIdentity, error) {
	v, ok := ctx.Value(staffCtxKey{}).(StaffIdentity)
	if !ok || v.StaffID <= 0 {
		return StaffIdentity{}, ErrNoStaff
	}
	return v, nil
}

// StaffSessionLoader 是中间件需要的那一次查库：拿签名里的声明去换一条
// **还活着**的会话。由 service 实现（它知道作用域怎么开）。
//
// 定义在这里而不是收一个 *service.StaffService：internal/service 已经 import
// 了本包，反过来收具体类型就成环了。更要紧的是，这个接口上只有一个方法 ——
// 中间件拿不到别的任何能力。
type StaffSessionLoader interface {
	// LoadStaffSession 校验这条会话并返回当前身份。
	//
	// 它必须在**声明所指的作用域**里查（平台级走平台作用域，商家级走那家店
	// 的租户作用域），而不是在请求 Host 的租户里查 —— 那正是跨店令牌被
	// RLS 拦住的地方，也是平台级令牌能查到自己的唯一路径。
	LoadStaffSession(ctx context.Context, claims StaffClaims, rawToken string) (StaffIdentity, error)
}

// StaffBearer 校验 Authorization 头里的后台会话 token，并把当前身份放进 ctx。
//
// **它必须挂在租户中间件之后**，理由与 Bearer 一字不差。
//
// 校验的六步，顺序是硬的：
//
//  1. 取租户（没有 → 500，这是装配 bug，不是客户端的错）
//  2. 取令牌（没有 / 不是 Bearer → 401）
//  3. 验签、核 typ、核载荷形状、核有效期（不过 → 401）
//  4. **令牌里的租户必须对得上本请求的租户**（不对 → 401，且留一条日志）
//  5. 查库：这条会话还活着吗、这个人还在吗（不在 → 401）
//  6. 这个人被停用了吗（停用 → 403）
//
// 第 3 步与第 4 步在第 5 步**之前**，这是这个文件最要紧的一句话：
// 一个拿着别家店令牌、或者拿着伪造令牌的调用方，打不出一次查库。
// 反过来（先查库再比租户）有两种结局，都比拒绝糟，展开写在 token.go 的
// 文件头 —— 而在后台这条路上它们更贵：staff 与 users 的 id 来自同一种自增
// 序列，撞上之后读到的是**另一家店的管理员身份**。
//
// 第 6 步单独一步而不是并进第 5 步：停用（status=2）要回 403 而不是 401。
// 401 会让被停用的人一遍遍重新登录，而他登得进来 —— 邮箱链接照样发得出去，
// 只是每次都在同一个地方被挡。403 说的是「你的账号被停了」，那是真话。
func StaffBearer(s *Signer, loader StaffSessionLoader, log *slog.Logger) gin.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		merchantID, err := tenant.FromContext(ctx)
		if err != nil {
			// 中间件挂反了。回 401 会把一个装配错误伪装成「你没登录」。
			_ = c.Error(err)
			problem.Write(c, http.StatusInternalServerError,
				problem.TypeInternal, "服务内部错误")
			return
		}

		raw, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeUnauthorized, "需要登录")
			return
		}

		claims, err := s.ParseStaff(raw)
		switch {
		case err == nil:
		case errors.Is(err, ErrTokenExpired):
			// 与「令牌不对」分开：客户端见到这个该去重新走邮箱链接，
			// 见到那个该把手里这串丢掉。后台没有 refresh，所以两者的动作
			// 其实一样，但运维看到的事件不一样 —— 一片过期是会话到期，
			// 一片无效是有人在试。
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeTokenExpired, "后台会话已过期，请重新登录")
			return
		default:
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeUnauthorized, "令牌无效")
			return
		}

		if !claims.TenantMatches(merchantID) {
			// 一条真实的跨店尝试，或者一个客户端存串了令牌。两者都值得一条
			// 日志：401 本身在运维那里是不可观测的。
			//
			// 日志里不记令牌本身 —— 它是一份能直接登录后台的凭据，
			// 把它写进日志等于把它写进每一次日志采集。
			log.WarnContext(ctx, "后台令牌的租户与本请求的租户不一致，拒绝",
				"token_merchant_id", claims.MerchantID,
				"token_platform", claims.Platform,
				"request_merchant_id", merchantID,
				"path", c.Request.URL.Path)
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeTokenTenantMismatch,
				"这串后台令牌不属于本店")
			return
		}

		id, err := loader.LoadStaffSession(ctx, claims, raw)
		if err != nil {
			// 会话被吊销、过期、那一行被删了、那个人被软删了 —— 对客户端
			// 都是同一件事：重新登录。分辨它们只会造出一个探针。
			//
			// 真正的服务端错误（连不上库）也落到这里回 401，这是一处刻意的
			// 取舍：把它们分开需要在这一层判断 pg 错误码，而那会让鉴权中间件
			// 知道数据库的事。错误本身进了 c.Error，日志看得见。
			_ = c.Error(err)
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeUnauthorized, "登录已失效，请重新登录")
			return
		}

		if id.Status != StaffStatusActive {
			problem.Write(c, http.StatusForbidden,
				problem.TypeAccountDisabled, "该后台账号已停用")
			return
		}

		c.Request = c.Request.WithContext(NewStaffContext(ctx, id))
		c.Next()
	}
}
