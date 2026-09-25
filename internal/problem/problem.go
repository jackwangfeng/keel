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
	TypeNotFound = "https://keel.dev/problems/not-found"
	TypeInternal = "https://keel.dev/problems/internal"
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
