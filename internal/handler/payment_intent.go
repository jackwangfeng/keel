package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 发起支付：POST /api/v1/orders/{order_no}/payments。
//
// 这条接口回的是**沙箱支付参数**。为什么是沙箱、沙箱为什么不走捷径、
// 它的代价是什么，完整论证在 service/payment_intent.go 的文件头。
// 这里只说 HTTP 这一侧的三件事：
//
//  1. 它与支付回调**共用同一个 PaymentService**。共用是刻意的：沙箱造出来的
//     报文与回调认得的报文必须是同一个形状、同一把密钥、同一个签名算法，
//     而让两个服务各持一份的话，它们分叉时的症状是「沙箱支付 401」——
//     一个看上去像密钥配错了的装配错误。
//  2. 它要 bearer（契约里这条接口没有 security: []），而回调那条不要。
//     同一个服务上挂着一条认证接口和一条未认证接口，这一点值得在装配处
//     （app.Router）再看一眼。
//  3. 幂等由 Idempotency-Key 请求头驱动，与 POST /orders 同一套机制、
//     不同的 scope（service 里那个常量）。
//
// 单独一个文件：contract_test.go 按文件对账 query 参数，这条接口一个都没有。

// PaymentHandler 实现发起支付。
//
// 与 PaymentWebhookHandler 分开两个类型而不是给它加个方法：两者的**信任模型
// 相反**——这一个的调用方是已登录的买家（身份可信、意图不可信），
// 那一个的调用方是未认证的渠道（身份靠验签、报文可信）。
// 放进同一个类型之后，「这个方法要不要鉴权」就成了一件要靠记性的事。
type PaymentHandler struct{ svc *service.PaymentService }

func NewPaymentHandler(s *service.PaymentService) *PaymentHandler {
	return &PaymentHandler{svc: s}
}

// intentRequest 是契约里那个内联请求体（只有一个必填的 channel）。
//
// 契约里它是内联 schema，没有 $ref，所以生成器没有为它出类型 —— 这是本仓库
// 「响应体用生成类型」那条规矩够不着的一处。手写的代价说清楚：契约给这个请求体
// 加一个字段时，这里不会有任何编译错误。所以本轮在报告里把「给这个请求体
// 起个名字（$ref 到 components.schemas）」列为 defer。
type intentRequest struct {
	Channel string `json:"channel"`
}

// Create 实现 POST /api/v1/orders/{order_no}/payments。
func (h *PaymentHandler) Create(c *gin.Context) {
	var req intentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.WriteBindError(c, err)
		return
	}

	intent, err := h.svc.CreateIntent(c.Request.Context(),
		c.Param("order_no"), req.Channel, c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		// 与下单共用 writeOrderError：这条接口的错误集合（404 / 409 / 422 /
		// 幂等三态）与 POST /orders 高度重合，而两份映射意味着同一个 sentinel
		// 可以在两条接口上回出不同的状态码。
		//
		// Retry-After 那一支尤其不能重写：契约在这条接口上也明写了它
		// （「同一 Idempotency-Key 正在处理中 —— 409 + Retry-After」）。
		writeOrderError(c, err)
		return
	}
	if intent.Replayed {
		// 契约里这条接口的 201 也带这个响应头，与 POST /orders 同一个 header
		// 定义（components.headers.IdempotencyReplayed），所以用的是 order.go
		// 里那个常量，不另起一个字符串 —— 写岔了的症状是客户端把一次重放
		// 记成一次新的支付调起，埋点上凭空多出一倍的支付发起量。
		c.Header(idempotencyReplayedHeader, "true")
	}

	payload := intent.Payload
	c.JSON(http.StatusCreated, api.PaymentIntent{
		PaymentNo:   intent.PaymentNo,
		Channel:     api.PaymentIntentChannel(intent.Channel),
		AmountCents: api.Money(intent.AmountCents),
		Payload:     &payload,
	})
}
