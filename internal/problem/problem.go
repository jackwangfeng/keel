// Package problem 按 RFC 9457 写错误响应。
//
// 单独成包，是因为写 Problem 的地方跨了两层：handler 要写（业务错误），
// 租户解析中间件也要写（解析不到商家的 404）。中间件在 internal/tenant 里，
// 而 tenant 被 repository 依赖、repository 被 handler 依赖 ——
// 让 tenant 去 import handler 会成环。
//
// 为什么中间件那条也必须写 body：契约里 /products 的响应集合只有 `200` 和
// `default: $ref Problem`。一个 Content-Length: 0 的 404 不在这个集合里，
// 按契约生成的客户端会拿到一个解析不出来的响应 —— 而 404 恰恰是它最需要读懂
// 的那一个（「这家店不存在」和「服务挂了」得区分得开）。
package problem

import (
	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
)

// 目前用到的 problem type。它们是 URI 形式的稳定标识，客户端按它分支，
// 所以改一个等于改契约的一部分 —— 不要顺手改措辞。
const (
	TypeNotFound         = "https://keel.dev/problems/not-found"
	TypeMethodNotAllowed = "https://keel.dev/problems/method-not-allowed"
	TypeInternal         = "https://keel.dev/problems/internal"

	// 鉴权相关的四个。它们分得这么细，是因为客户端对它们的处置**各不相同**：
	//
	//   unauthorized         → 重新登录
	//   token-expired        → 去 /auth/refresh，不必打扰用户
	//   token-tenant-mismatch→ 这串令牌不属于本店；客户端该丢掉它，
	//                          而运维该知道有人在跨店用令牌
	//   account-disabled     → 封禁 / 注销，重新登录也没用
	//
	// 全都压成一个 unauthorized 的话，客户端只能靠猜，而猜错的那一半会
	// 把用户踢回登录页；跨店那一条更是会淹没在一片普通的鉴权失败里。
	TypeUnauthorized        = "https://keel.dev/problems/unauthorized"
	TypeTokenExpired        = "https://keel.dev/problems/token-expired"
	TypeTokenTenantMismatch = "https://keel.dev/problems/token-tenant-mismatch"
	TypeAccountDisabled     = "https://keel.dev/problems/account-disabled"

	// 请求体本身不合法（缺字段、两种凭据都给了或都没给）。
	TypeInvalidRequest = "https://keel.dev/problems/invalid-request"

	// 下单链路的四个。它们同样分得细，因为客户端对它们的处置各不相同：
	//
	//   insufficient-stock        → 让用户改数量或换商品
	//   price-changed             → 重新试算再提交（**不要**直接重试原请求）
	//   idempotency-key-in-flight → 按 Retry-After 退避重试，**不是**业务失败，
	//                               不该弹窗，更不该让用户再点一次「提交订单」
	//   idempotency-key-reused    → 客户端自己的 bug：同一把钥匙配了两个请求体
	//
	// 前两个都是 409，后两个一个 409 一个 422 —— 只看状态码分不开，而
	// 「退避重试」与「让用户改购物车」是完全相反的动作。
	//
	// 契约里这几个写的是 https://errors.example.com/... 那个示例域名；
	// 本仓库统一用 keel.dev（见本常量块开头那句话：改一个等于改契约的一部分，
	// 所以也不顺手去改那几个）。这处不一致已在报告里列为 defer。
	TypeInsufficientStock      = "https://keel.dev/problems/insufficient-stock"
	TypePriceChanged           = "https://keel.dev/problems/price-changed"
	TypeIdempotencyKeyInFlight = "https://keel.dev/problems/idempotency-key-in-flight"
	TypeIdempotencyKeyReused   = "https://keel.dev/problems/idempotency-key-reused"

	// 契约声明了、本轮刻意没有实现的路径。用一个**专门的** type 而不是复用
	// internal：客户端能据此分辨「这个功能还没有」与「服务器炸了」，
	// 而这两件事的重试策略完全相反。
	TypeNotImplemented = "https://keel.dev/problems/not-implemented"
)

// Write 写一个 RFC 9457 响应并中止后续 handler。
//
// 刻意只收 type 和 title，不收 detail：detail 是给人看的自由文本，而这里的
// 调用点全都在匿名可访问的路径上，err 的内容里常常带着表名、列名和参数值。
// 真要加 detail，加的是一句人写的话，不是 err.Error()。
func Write(c *gin.Context, status int, kind, title string) {
	// 先设 Content-Type：gin 的 JSON 渲染只在它还没被设过时才写自己那个
	// application/json，所以顺序反了的话 problem+json 会被吃掉。
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, api.Problem{
		Type:   kind,
		Title:  title,
		Status: status,
	})
}
