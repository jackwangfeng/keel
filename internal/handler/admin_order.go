package handler

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 后台的订单与售后写操作：发货（POST /admin/orders/{order_no}/shipments）、
// 退款审核（POST /admin/refunds/{refund_no}/audit）、确认收到退货
// （POST /admin/refunds/{refund_no}/receipt）。
//
// 三条都没有 query 参数，放在同一个文件里（contract_test.go 的对账按文件解析）。
// handler 只做参数绑定、调 service、渲染 —— 判权（门店范围）与状态机都在
// service/order_fulfillment.go 与 service/refund.go 里。

// AdminOrderHandler 是后台订单与售后那几条接口。
type AdminOrderHandler struct{ svc *service.AdminOrderService }

func NewAdminOrderHandler(s *service.AdminOrderService) *AdminOrderHandler {
	return &AdminOrderHandler{svc: s}
}

// Ship 实现 POST /api/v1/admin/orders/:order_no/shipments。
func (h *AdminOrderHandler) Ship(c *gin.Context) {
	var req api.ShipmentCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}
	sh, replayed, err := h.svc.Ship(c.Request.Context(), c.Param("order_no"),
		service.ShipRequest{CarrierCode: req.CarrierCode, TrackingNo: req.TrackingNo},
		c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeAdminOrderError(c, err)
		return
	}
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusCreated, apiShipment(sh))
}

func apiShipment(s repository.Shipment) api.Shipment {
	return api.Shipment{
		Id:          s.ID,
		CarrierCode: s.CarrierCode,
		TrackingNo:  s.TrackingNo,
		Status:      api.ShipmentStatus(s.Status),
		ShippedAt:   s.ShippedAt,
		DeliveredAt: s.DeliveredAt,
	}
}

// writeAdminOrderError 把后台订单 / 售后那几条接口的错误翻成契约里的响应。
//
// 判权（403 role-forbidden / out-of-scope）排在最前面，与别的后台接口同一个入口
// （writePermissionError）。其余逐条对应契约里明写的状态码，没对上的一律 500。
func writeAdminOrderError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrOrderNotFound):
		// 契约 /admin/ 段头的约定：查不到当前租户名下的那一个一律 404，不是 403。
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "订单不存在或不属于当前租户")

	case errors.Is(err, service.ErrOrderHasPendingFullRefund):
		writeProblemDetail(c, http.StatusConflict, problem.TypeOrderHasPendingFullRefund,
			"这笔订单有未完结的整单退款申请，请先处理退款单", err)

	case errors.Is(err, service.ErrOrderNotShippable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeOrderStatusNotShippable,
			"这笔订单当前不能发货（非已支付，或已发过货）", err)

	case errors.Is(err, service.ErrTrackingNoDuplicated):
		problem.Write(c, http.StatusConflict, problem.TypeTrackingNoDuplicated,
			"这个运单号已经登记过了")

	case errors.Is(err, service.ErrShipmentBadRequest):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"承运商与运单号都必须填写", err)

	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")

	case errors.Is(err, service.ErrIdempotencyInFlight):
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem.Write(c, http.StatusConflict, problem.TypeIdempotencyKeyInFlight,
			"这个 Idempotency-Key 正在处理中，请稍后重试")

	case errors.Is(err, service.ErrIdempotencyKeyReused):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeIdempotencyKeyReused,
			"同一个 Idempotency-Key 配了不同的请求体")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
	}
}
