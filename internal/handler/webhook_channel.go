package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 渠道回调：POST /api/v1/webhooks/channels/{binding_id}（渠道适配层，service/channel_inbound.go）。
//
// 与支付回调同一个形状：security: []、Host 定租户、签名定真假；对外只有 200（带平台要的 ack）、
// 空的 401（binding 不存在 / 验签失败 / 没配密钥，三者不可区分）、基础设施故障时的 default Problem
// （平台会重推，入库按外部事件 ID 去重）。KEEL_CHANNELS 关闭时这条路由不注册。

// ChannelWebhookHandler 实现渠道回调。
type ChannelWebhookHandler struct{ svc *service.ChannelService }

func NewChannelWebhookHandler(s *service.ChannelService) *ChannelWebhookHandler {
	return &ChannelWebhookHandler{svc: s}
}

// Notify 实现 POST /api/v1/webhooks/channels/:binding_id。
func (h *ChannelWebhookHandler) Notify(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxChannelWebhookBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		_ = c.Error(err)
		c.Status(http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(c.Param("binding_id"), 10, 64)
	if err != nil || id <= 0 {
		c.Status(http.StatusUnauthorized)
		return
	}
	ack, err := h.svc.Inbound(c.Request.Context(), id, c.Request, raw)
	switch {
	case err == nil:
		if len(ack) == 0 {
			c.Status(http.StatusOK)
			return
		}
		ct := "text/plain; charset=utf-8"
		if json.Valid(ack) {
			ct = "application/json"
		}
		c.Data(http.StatusOK, ct, ack)
	case errors.Is(err, service.ErrChannelWebhookRejected):
		// 空的 401：不说是哪一种（见文件头）。
		c.Status(http.StatusUnauthorized)
	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "回调未能处理，请稍后重推")
	}
}
