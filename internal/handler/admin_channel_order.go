package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 后台的渠道订单（第三期 Task 7，service/admin_channel_order.go）：
//
//	GET  /api/v1/admin/channel-orders                         （admin_channel_order_list.go）
//	GET  /api/v1/admin/channel-orders/{channel_order_id}      （含平台申请）
//	POST /api/v1/admin/channel-orders/{channel_order_id}/retry
//	POST /api/v1/admin/channel-orders/{channel_order_id}/accept
//	POST /api/v1/admin/channel-orders/{channel_order_id}/reject
//	POST /api/v1/admin/channel-order-requests/{request_id}/decision
//
// 与渠道管理一样，KEEL_CHANNELS 关闭时不注册。三个动作与申请决定都回处理之后的渠道单详情。

func writeChannelOrderError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrChannelOrderNotRetryable), errors.Is(err, service.ErrChannelOrderNotAcceptable),
		errors.Is(err, service.ErrChannelRequestDecided):
		writeProblemDetail(c, http.StatusConflict, problem.TypeChannelOrderState, "渠道单此刻不能这样处理", err)
	case errors.Is(err, service.ErrChannelOrderAcceptFailed):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeChannelOrderAcceptFailed, "接单没成功", err)
	default:
		writeChannelError(c, err)
	}
}

// apiChannelOrder 把一行渠道单写成契约 ChannelOrder。金额 / 行 / 收货人三列的 JSON 形状与契约逐字段一致
// （service/channel_order.go 的 channelOrderAmounts 等），直接解进契约类型。
func apiChannelOrder(co repository.ChannelOrder) api.ChannelOrder {
	out := api.ChannelOrder{Id: co.ID, BindingId: co.BindingID, ExternalOrderId: co.ExternalOrderID,
		ExternalOrderName: co.ExternalOrderName, StoreId: co.StoreID, OrderNo: co.OrderNo, PlatformStatus: co.PlatformStatus,
		Status: api.ChannelOrderStatus(co.Status), Exception: co.Exception, AcceptDeadline: co.AcceptDeadline,
		DeliveryMode: int32(co.DeliveryMode), Lines: []api.ChannelOrderLine{}, Test: co.Test,
		CreatedAt: co.CreatedAt, UpdatedAt: co.UpdatedAt}
	_ = json.Unmarshal(co.Amounts, &out.Amounts)
	_ = json.Unmarshal(co.Lines, &out.Lines)
	_ = json.Unmarshal(co.Receiver, &out.Receiver)
	if out.Lines == nil {
		out.Lines = []api.ChannelOrderLine{}
	}
	return out
}

func apiChannelOrderDetail(v service.ChannelOrderView) api.ChannelOrderDetail {
	o := apiChannelOrder(v.Order)
	reqs := make([]api.ChannelOrderRequest, 0, len(v.Requests))
	for _, r := range v.Requests {
		ar := api.ChannelOrderRequest{Id: r.ID, ExternalRequestId: r.ExternalRequestID, Kind: api.ChannelOrderRequestKind(r.Kind),
			AmountCents: r.AmountCents, Reason: r.Reason, Status: api.ChannelOrderRequestStatus(r.Status), Deadline: r.Deadline,
			DecidedBy: r.DecidedBy, DecidedAt: r.DecidedAt, CreatedAt: r.CreatedAt}
		_ = json.Unmarshal(r.Lines, &ar.Lines)
		if ar.Lines == nil {
			ar.Lines = []struct {
				ExternalSkuId string `json:"external_sku_id"`
				Qty           int32  `json:"qty"`
			}{}
		}
		reqs = append(reqs, ar)
	}
	return api.ChannelOrderDetail{Id: o.Id, BindingId: o.BindingId, ExternalOrderId: o.ExternalOrderId,
		ExternalOrderName: o.ExternalOrderName, StoreId: o.StoreId, OrderNo: o.OrderNo, PlatformStatus: o.PlatformStatus,
		Status: o.Status, Exception: o.Exception, AcceptDeadline: o.AcceptDeadline, DeliveryMode: o.DeliveryMode,
		Amounts: o.Amounts, Lines: o.Lines, Receiver: o.Receiver, Test: o.Test, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		Requests: reqs}
}

func (h *AdminChannelHandler) writeChannelOrder(c *gin.Context, v service.ChannelOrderView, err error) {
	if err != nil {
		writeChannelOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiChannelOrderDetail(v))
}

// GetChannelOrder 实现 GET /api/v1/admin/channel-orders/{channel_order_id}。
func (h *AdminChannelHandler) GetChannelOrder(c *gin.Context) {
	id, ok := pathID(c, "channel_order_id")
	if !ok {
		return
	}
	v, err := h.svc.GetChannelOrder(c.Request.Context(), id)
	h.writeChannelOrder(c, v, err)
}

// RetryChannelOrder 实现 POST /api/v1/admin/channel-orders/{channel_order_id}/retry。
func (h *AdminChannelHandler) RetryChannelOrder(c *gin.Context) {
	id, ok := pathID(c, "channel_order_id")
	if !ok {
		return
	}
	v, err := h.svc.RetryChannelOrder(c.Request.Context(), id)
	h.writeChannelOrder(c, v, err)
}

// AcceptChannelOrder 实现 POST /api/v1/admin/channel-orders/{channel_order_id}/accept。
func (h *AdminChannelHandler) AcceptChannelOrder(c *gin.Context) {
	id, ok := pathID(c, "channel_order_id")
	if !ok {
		return
	}
	v, err := h.svc.AcceptChannelOrder(c.Request.Context(), id)
	h.writeChannelOrder(c, v, err)
}

// RejectChannelOrder 实现 POST /api/v1/admin/channel-orders/{channel_order_id}/reject（请求体可省）。
func (h *AdminChannelHandler) RejectChannelOrder(c *gin.Context) {
	id, ok := pathID(c, "channel_order_id")
	if !ok {
		return
	}
	var req api.PostAdminChannelOrdersChannelOrderIdRejectJSONBody
	if c.Request.ContentLength != 0 && !bindJSON(c, &req) {
		return
	}
	reason := ""
	if req.Reason != nil {
		reason = *req.Reason
	}
	v, err := h.svc.RejectChannelOrder(c.Request.Context(), id, reason)
	h.writeChannelOrder(c, v, err)
}

// DecideChannelOrderRequest 实现 POST /api/v1/admin/channel-order-requests/{request_id}/decision。
func (h *AdminChannelHandler) DecideChannelOrderRequest(c *gin.Context) {
	id, ok := pathID(c, "request_id")
	if !ok {
		return
	}
	// agree 必填：用指针收，缺了不当成「拒绝」。
	var req struct {
		Agree *bool `json:"agree"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if req.Agree == nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "缺少 agree（true 同意 / false 拒绝）")
		return
	}
	v, err := h.svc.DecideChannelOrderRequest(c.Request.Context(), id, *req.Agree)
	h.writeChannelOrder(c, v, err)
}
