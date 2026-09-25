package handler

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 支付渠道异步回调：POST /api/v1/webhooks/payments/{channel}。
//
// # 契约只给了两个状态码，这决定了这个 handler 的全部形状
//
//	'200': 已受理（无论首次还是重复）
//	'401': 签名验证失败
//
// 没有 default: Problem，没有 404、没有 422、没有 5xx。于是「我没能把这笔钱
// 入账，请重推」这句话**在契约里说不出来**。
//
// 本轮的处置：除验签失败外一律 200，每一种不正常都留一条 Error 级日志。
// 这是唯一诚实的做法 —— 编一个契约里没有的状态码，
// 按契约生成的渠道侧客户端解析不了；而把失败伪装成成功却不告警，
// 会让一笔真实到账无声无息地消失。
//
// 这处契约缺口已经补上了（ff6d0e1 给两条 webhook 加了
// `default: { $ref: Problem }`），所以下面的 500 分支回的是 Problem 体，
// 不是一个 Content-Length: 0 的裸状态码。
//
// # 它为什么挂在 v1 组里（带租户中间件、不带 bearer）
//
// 契约写的是 `security: []` —— 不需要令牌，因为调用方是支付渠道。
// 但**不需要令牌不等于不需要租户**：租户由 v1 组上那道 res.Middleware() 从 Host
// 定出来，和别的每一条接口一模一样，没有破例。
// 完整论证（Host 回答「哪家店」、签名回答「是不是真的」）写在 service/payment.go
// 的文件头。

// PaymentWebhookHandler 实现支付渠道异步回调。
type PaymentWebhookHandler struct{ svc *service.PaymentService }

func NewPaymentWebhookHandler(s *service.PaymentService) *PaymentWebhookHandler {
	return &PaymentWebhookHandler{svc: s}
}

// Notify 实现 POST /api/v1/webhooks/payments/:channel。
func (h *PaymentWebhookHandler) Notify(c *gin.Context) {
	// 大小闸门必须在读 body 之前。验签算的是**全部字节**，所以它没法先验签再
	// 限长；这道闸门保护的正是验签本身（见 service.MaxNotifyBytes）。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxNotifyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// 读不完（超长，或者连接断了）。归到 401：这份报文我们没法验签，
		// 而契约里能表达「拒绝」的只有这一个码。
		_ = c.Error(err)
		c.Status(http.StatusUnauthorized)
		return
	}

	// 原始字节交给 service，不是重新序列化的 JSON：HMAC 算的是字节，
	// `{"a":1}` 与 `{"a": 1}` 是同一个对象、不同的字节。
	err = h.svc.Notify(c.Request.Context(), c.Param("channel"),
		raw, c.GetHeader(service.SignatureHeader))

	switch {
	case err == nil:
		c.Status(http.StatusOK)

	case errors.Is(err, service.ErrWebhookSignature):
		// 契约：「签名验证失败返回 401，**且不得泄露任何内部状态**」。
		// 所以这里是一个**空的** 401：没有 body、没有 Problem、没有 detail。
		// 连 problem.Write 都不用 —— 那会写出一个带 type/title 的 JSON，
		// 而「这家店没配密钥」与「签名对不上」在那份 JSON 里迟早会被分开写，
		// 那正是一个「这家店接没接支付」的探测器。
		//
		// 日志留在服务端（service 那边已经打了 Warn），客户端一个字都不给。
		_ = c.Error(err)
		c.Status(http.StatusUnauthorized)

	case errors.Is(err, service.ErrWebhookDuplicate):
		// **正常路径。** 渠道没收到上一次的 200 就会再推，而「没收到」与
		// 「我们没处理」在它那边是同一件事。契约明写：「重复回调时写入冲突，
		// 直接返回 200」。不记 c.Error —— 它不是错误，记了会让错误日志里
		// 最常见的一条是一件正常的事。
		c.Status(http.StatusOK)

	case isSettledButNotAccepted(err):
		// 订单查不到、金额对不上、订单已不在待支付、报文解不开。
		//
		// 四种的共同点是**重推一次不会有任何不同**：订单号不会变出来，
		// 金额不会对上，已关的单不会变回待支付，解不开的报文下次还是解不开。
		// 所以 200 加一条 Error 日志 —— 让渠道停下来，让人去看日志。
		// 每一种在 service 那边都有自己的 sentinel 与自己的文案，
		// 这里不再分支：分支只会让人以为客户端能据此做点什么，而它不能。
		_ = c.Error(err)
		c.Status(http.StatusOK)

	default:
		// 剩下的是**基础设施故障**（连不上库、事务提交失败）。
		//
		// 这一支回 500。契约现在**有**这条（`default: { $ref: Problem }`，
		// 见 ff6d0e1）—— 当初写这段注释时它还没有，那是契约缺一句话，不是实现
		// 越界。理由是两害相权：
		//   · 回 200 的话渠道不会再推，而我们确实没入账 —— 一笔真实到账
		//     就这么没了，只在日志里留个影；
		//   · 回 500 是每个渠道都懂的「稍后重推」，而重推正是这里该发生的事
		//     （下一分钟数据库可能就好了，幂等由 uk_payments_channel_txn 兜着，
		//     重推不会重复入账）。
		//
		// 换句话说：这件事真的会发生，所以契约必须说得出来。
		//
		// 响应体走 Problem，不是光秃秃的 c.Status —— 契约声明的是
		// `application/problem+json`，回一个 Content-Length: 0 的裸 500，
		// 按契约生成的客户端会在它最需要读懂的那类响应上解析失败。
		// 这正是 NoRoute/NoMethod 当初不肯用 gin 默认 text/plain 404 的同一条理由。
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal,
			"回调未能入账，请稍后重推")
	}
}

// isSettledButNotAccepted 判断这是不是一种「重推也没用」的业务结论。
//
// 单列一个函数而不是在 switch 里铺四个 case：这四种共享的是**处置**
// （200 + Error 日志），而不是语义。摊在 switch 里的话，将来加第五种 sentinel
// 的人会以为自己在加一个新的响应码分支，而他其实是在决定「渠道要不要重推」。
func isSettledButNotAccepted(err error) bool {
	for _, sentinel := range []error{
		service.ErrWebhookOrderUnknown,
		service.ErrWebhookAmountMismatch,
		service.ErrWebhookOrderNotPayable,
		service.ErrWebhookBadPayload,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
