package handler

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 退款渠道异步回调：POST /api/v1/webhooks/refunds/{channel}。
//
// 与支付回调（webhook.go）同构到底：同一道报文大小闸门、同一个签名头、同一张
// 状态码表 —— 验签失败 401（不泄露任何内部状态）；首次入账与重复推送 200；
// 「渠道重推也没用」的业务结论 200 + Error 日志；基础设施故障 5xx 请渠道重推。

// RefundWebhookHandler 是退款回调那一条接口。
type RefundWebhookHandler struct{ svc *service.RefundService }

func NewRefundWebhookHandler(s *service.RefundService) *RefundWebhookHandler {
	return &RefundWebhookHandler{svc: s}
}

// Notify 实现 POST /api/v1/webhooks/refunds/:channel。
func (h *RefundWebhookHandler) Notify(c *gin.Context) {
	// 验签要读完整个 body，所以大小闸门必须在验签之前（与支付回调同一个理由）。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxNotifyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		_ = c.Error(err)
		c.Status(http.StatusUnauthorized)
		return
	}

	err = h.svc.Notify(c.Request.Context(), c.Param("channel"), raw, c.GetHeader(service.SignatureHeader))
	switch {
	case err == nil:
		c.Status(http.StatusOK)

	case errors.Is(err, service.ErrWebhookSignature):
		_ = c.Error(err)
		c.Status(http.StatusUnauthorized)

	case errors.Is(err, service.ErrWebhookDuplicate):
		// 重复推送是常态（退款到账往往延迟数小时，渠道会反复推）。
		c.Status(http.StatusOK)

	case isRefundSettledButNotAccepted(err):
		_ = c.Error(err)
		c.Status(http.StatusOK)

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal,
			"回调未能入账，请稍后重推")
	}
}

// isRefundSettledButNotAccepted：验签过了、渠道重推也不会让结果变的那几种结论。
func isRefundSettledButNotAccepted(err error) bool {
	for _, sentinel := range []error{
		service.ErrRefundWebhookUnknown,
		service.ErrRefundWebhookAmountMismatch,
		service.ErrRefundWebhookNotRefunding,
		service.ErrRefundWebhookChannelMismatch,
		service.ErrWebhookBadPayload,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}
