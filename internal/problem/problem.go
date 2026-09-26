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

	// 发起支付那条接口上的业务冲突。契约里 POST /orders/{order_no}/payments 的
	// 409 描述逐字写着这个 type：「订单当前状态不允许支付（非 10 待支付，
	// 或已超时关闭）」。
	//
	// 它与 insufficient-stock / price-changed 同为 409，而客户端的处置完全不同：
	// 这一个要刷新订单（这一单可能已经付过了，再付一次是重复付款），
	// 那两个要改购物车。只看状态码分不开。
	TypeOrderStatusNotPayable = "https://keel.dev/problems/order-status-not-payable"

	// 后台身份那四个（数据模型 §14 / 契约 AdminAuth 与 Admin 两个 tag）。
	//
	// 它们同样分得细，理由与上面那几组一字不差 —— 契约里这四种全都是
	// 403 或 409，而只看状态码分不开，但客户端与运维对它们的处置完全不同：
	//
	//   bootstrap-closed   → 这个部署已经引导过了；别再拿引导 token 试，
	//                        走邮箱链接。这是个**正常**状态，不是故障。
	//   staff-forbidden    → 你是操作员不是管理员；换个人来做这件事。
	//   staff-email-taken  → 换个邮箱（或者那个人已经在了）。
	//   last-admin         → 先加一个管理员，再来降级 / 停用这一个。
	//                        数据模型 §14 那条进不了数据库的约束。
	//
	// last-admin 与 staff-email-taken 都是 409 且都出现在同一条 PATCH /
	// POST 上，压成一个之后前端只能把两句完全不同的话写成一句。
	TypeBootstrapClosed = "https://keel.dev/problems/bootstrap-closed"
	TypeStaffForbidden  = "https://keel.dev/problems/staff-forbidden"
	TypeStaffEmailTaken = "https://keel.dev/problems/staff-email-taken"
	TypeLastAdmin       = "https://keel.dev/problems/last-admin"

	// 商家写路径那一组（M4，契约 Admin + Catalog 两个 tag 的 16 条）。
	//
	// 它们**全部是 404 或 409 或 422**，而只看状态码分不开 —— 契约在每一条
	// 端点上都写着「按 Problem type 区分」。分得细的理由与上面那几组同构：
	// 客户端对它们的处置完全不同，而这里有两处混掉会直接造出无限重试：
	//
	//   inventory-precondition-failed → 刷新那一格（响应体里带 current）再试一次，
	//                                   **会成功**
	//   （对照）404 SKU 不在本租户    → 重试**永远**不会成功
	//
	//   product-deleted               → 这件商品已经软删，改它没有意义
	//   product-still-published       → 先下架再删（两步都是调用方做得到的）
	//   product-has-no-sku            → 先加一个 SKU 再上架
	//   sku-code-duplicated           → 换一个货号
	//   sku-last-of-published-product → 先下架商品，或者先加一个兄弟规格
	//   category-has-children /
	//   category-has-products         → 先把子分类 / 商品挪走，再从叶子往上删
	//   category-cycle                → 目标父节点是自己的后代，换一个
	//   upload-not-found /
	//   upload-wrong-purpose /
	//   product-image-duplicated      → 这三条都是 422，客户端要改的东西各不相同
	//
	// 逐字对着契约里那几段 description 里写出来的 URI，不要顺手改措辞。
	TypeProductDeleted         = "https://keel.dev/problems/product-deleted"
	TypeProductStillPublished  = "https://keel.dev/problems/product-still-published"
	TypeProductHasNoSKU        = "https://keel.dev/problems/product-has-no-sku"
	TypeSKUCodeDuplicated      = "https://keel.dev/problems/sku-code-duplicated"
	TypeSKULastOfPublished     = "https://keel.dev/problems/sku-last-of-published-product"
	TypeInventoryPrecondition  = "https://keel.dev/problems/inventory-precondition-failed"
	TypeUploadNotFound         = "https://keel.dev/problems/upload-not-found"
	TypeUploadWrongPurpose     = "https://keel.dev/problems/upload-wrong-purpose"
	TypeProductImageDuplicated = "https://keel.dev/problems/product-image-duplicated"
	TypeCategoryHasChildren    = "https://keel.dev/problems/category-has-children"
	TypeCategoryHasProducts    = "https://keel.dev/problems/category-has-products"
	TypeCategoryCycle          = "https://keel.dev/problems/category-cycle"
	TypeUploadTooLarge         = "https://keel.dev/problems/upload-too-large"
	TypeUploadUnsupportedMedia = "https://keel.dev/problems/upload-unsupported-media-type"

	// 合规检查那两条（M4 阶段 2，商品理解服务设计 §2）。
	//
	// 它们分成两个 type，而且状态码也不同（422 / 503），因为商家要做的事
	// 完全相反：
	//
	//   compliance-rejected    → 你的文案里有违禁词，**改了再来**。
	//                            errors[] 里逐条写着哪个字段第几个字
	//                            （契约的 FieldError）。重试原请求永远失败。
	//   compliance-unavailable → 我们没查出来，**过一会儿原样再试一次**。
	//                            你的文案可能一个字都不用改。
	//
	// 压成一个的话，客户端只能把「改文案」和「退避重试」写成同一段逻辑，
	// 而它们一个是死路一个是活路。
	//
	// **unavailable 是 503 而不是 500**：这一条是全系统唯一一处「宁可误拒」
	// （§7 的降级表 / docs/ai-capabilities.md 的三条纪律第三条）——
	// 它不是服务端出 bug，是我们主动选择在答不出来时拒绝。503 + Retry-After
	// 是这件事在 HTTP 上的准确说法，而 500 会让客户端以为有人写错了代码。
	TypeComplianceRejected    = "https://keel.dev/problems/compliance-rejected"
	TypeComplianceUnavailable = "https://keel.dev/problems/compliance-unavailable"

	// 契约声明了、本轮刻意没有实现的路径。用一个**专门的** type 而不是复用
	// internal：客户端能据此分辨「这个功能还没有」与「服务器炸了」，
	// 而这两件事的重试策略完全相反。
	TypeNotImplemented = "https://keel.dev/problems/not-implemented"

	// 被限流挡住。契约里已经有这个 type（POST /auth/sms-code 的 429 描述
	// 逐字写着它），所以这里复用，不新造一个 —— 同一件事两个 type，
	// 客户端的退避逻辑就要写两遍。
	//
	// 429 配 Retry-After：契约在那条接口上把这个头写进了响应定义
	// （「建议退避秒数」）。没有它的话，客户端能做的只有立刻重试，
	// 而那正好是限流要挡的行为。
	TypeRateLimited = "https://keel.dev/problems/rate-limited"
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

// WriteValue 写一个**带扩展成员**的 problem 响应。
//
// RFC 9457 允许 problem 对象带扩展成员，而契约里真的用了一处：
// PUT /admin/skus/{sku_id}/inventory 的 409 回的是 InventoryConflict ——
// Problem 加一个必填的 current。Write 那个函数只收 type / title / status，
// 表达不了它。
//
// 收 any 而不是给 Write 加一个 extras map：扩展的形状由契约决定（InventoryConflict
// 是一个有具体字段的 schema），而 map 会让调用方自己拼键名，
// 那正是「响应体用生成类型，不手写结构体」要挡的东西 —— 调用方传进来的
// 应当是 api.InventoryConflict 本身。
//
// Content-Type 与 Write 走同一句：两处各写一遍的话，分叉时的症状是某一条
// 错误响应的 Content-Type 退回 application/json，而按契约生成的客户端
// 会在它最需要读懂的那类响应上走错分支。
func WriteValue(c *gin.Context, status int, body any) {
	c.Header("Content-Type", "application/problem+json")
	c.AbortWithStatusJSON(status, body)
}
