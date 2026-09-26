package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/tenant"
)

// Bearer 校验 Authorization 头里的 access_token，并把当前用户放进 ctx。
//
// **它必须挂在租户中间件之后。** 顺序反了的话第一步就取不到租户，那时这里
// 返回 500 而不是放行——放行意味着一个没有租户的请求带着一个「已认证」的
// 上下文走下去，而下游每一处都会以为租户校验已经做过了。
//
// 校验的四步，顺序也是硬的：
//
//  1. 取租户（没有 → 500，这是装配 bug，不是客户端的错）
//  2. 取令牌（没有 / 不是 Bearer → 401）
//  3. 验签与有效期（不过 → 401）
//  4. **令牌里的租户必须等于本请求的租户**（不等 → 401）
//
// 第 4 步是这个文件存在的理由，展开写在 token.go 的文件头。这里只重复最要紧
// 的那一句：拒绝必须发生在**这里**，在任何一次查库之前，而且它是一条鉴权
// 语义的拒绝，不是「这个人在本店查不到」。
//
// 把第 4 步删掉、让 user_id 直接进 ctx 会怎样：请求带着 A 店的 user_id 进入
// B 店的租户上下文，随后每一条 SQL 都在 B 店的 RLS 之下按这个 id 查。
// 两家店的 users.id 来自同一个序列，撞上是日常——于是「拿别人的令牌读到
// 另一个人的订单」不需要任何额外条件。这条路径上没有任何一处会报错。
func Bearer(s *Signer, log *slog.Logger) gin.HandlerFunc { return bearer(s, log, false) }

// OptionalBearer 与 Bearer 只差一处：**请求里完全没有 Authorization 头时放行**，
// ctx 里没有用户（匿名）。只给「公开可读、但某一类内容只有本人能读」的接口用 ——
// 眼下只有 GET /uploads/{upload_id}（退款凭证只有上传者本人可读，商品图与头像公开）。
//
// **带了头就按 Bearer 的全部四步判**，无效、过期、别家店的令牌照样 401，
// 不会悄悄降级成匿名。降级的话，一个令牌过期的买家读自己的凭证会拿到 403
// 「无权读取」—— 他会以为那张图不是他的，而正确的动作是去刷新令牌。
func OptionalBearer(s *Signer, log *slog.Logger) gin.HandlerFunc { return bearer(s, log, true) }

func bearer(s *Signer, log *slog.Logger, optional bool) gin.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		merchantID, err := tenant.FromContext(ctx)
		if err != nil {
			// 中间件挂反了。回 401 会把一个装配错误伪装成「你没登录」，
			// 于是运维看到的是一片鉴权失败，而真因是路由的一行顺序。
			_ = c.Error(err)
			problem.Write(c, http.StatusInternalServerError,
				problem.TypeInternal, "服务内部错误")
			return
		}

		header := c.GetHeader("Authorization")
		if optional && strings.TrimSpace(header) == "" {
			c.Next()
			return
		}
		raw, ok := bearerToken(header)
		if !ok {
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeUnauthorized, "需要登录")
			return
		}

		claims, err := s.ParseKind(raw, KindAccess)
		switch {
		case err == nil:
		case errors.Is(err, ErrTokenExpired):
			// 与「令牌不对」分开：客户端见到这个应该去 /auth/refresh，
			// 见到那个应该去重新登录。两者都回 401 而只差 title 的话，
			// 客户端只能靠猜，而猜错的那一半会把用户踢回登录页。
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeTokenExpired, "登录已过期，请刷新令牌")
			return
		default:
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeUnauthorized, "令牌无效")
			return
		}

		if claims.MerchantID != merchantID {
			// 一条真实的跨店尝试，或者一个客户端存串了令牌。两者都值得一条
			// 日志：401 本身在运维那里是不可观测的，而「有人拿着另一家店的
			// 令牌在打这家店」和「某个用户的令牌过期了」是完全不同的事件。
			//
			// 日志里不记令牌本身，只记两个租户 id —— 令牌是一份能直接登录的
			// 凭据，把它写进日志等于把它写进每一次日志采集。
			log.WarnContext(ctx, "令牌的租户与本请求的租户不一致，拒绝",
				"token_merchant_id", claims.MerchantID,
				"request_merchant_id", merchantID,
				"path", c.Request.URL.Path)
			problem.Write(c, http.StatusUnauthorized,
				problem.TypeTokenTenantMismatch,
				"这串令牌不属于本店")
			return
		}

		c.Request = c.Request.WithContext(NewContext(ctx, Identity{
			UserID:    claims.UserID,
			SessionID: claims.SessionID,
		}))
		c.Next()
	}
}

// bearerToken 从 Authorization 头里取出令牌。
//
// scheme 用大小写不敏感的比较：RFC 7235 说 scheme 是大小写不敏感的，
// 而真实客户端里 "bearer"、"Bearer"、"BEARER" 三种都见得到。
// 按字面量比的话，症状是「某个客户端永远 401」，而它的请求看上去完全正常。
func bearerToken(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}
