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

// 买家侧售后（数据模型 §11）：
//
//	POST /orders/{order_no}/refunds    申请（支持部分退）
//	GET  /orders/{order_no}/refunds    这一单的退款单
//	GET  /refunds/{refund_no}          退款单详情
//	POST /refunds/{refund_no}/cancel   撤回
//
// 这四条都没有 query 参数；带 page / page_size / status 的 GET /refunds 单独在
// refund_list.go（contract_test.go 的 query 参数对账按文件解析）。
//
// 金额一律由服务端算（service/refund_calc.go），请求体里没有任何金额字段可以传。

// RefundHandler 是买家侧售后那几条接口。
type RefundHandler struct{ svc *service.RefundService }

func NewRefundHandler(s *service.RefundService) *RefundHandler { return &RefundHandler{svc: s} }

// Create 实现 POST /api/v1/orders/:order_no/refunds。
func (h *RefundHandler) Create(c *gin.Context) {
	var raw api.RefundCreateRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return
	}
	req := service.RefundCreateRequest{
		RefundType: int16(raw.RefundType),
		ReasonCode: int16(raw.ReasonCode),
		ReasonText: raw.ReasonText,
	}
	if raw.EvidenceUrls != nil {
		req.EvidenceURLs = *raw.EvidenceUrls
	}
	for _, it := range raw.Items {
		// 契约里 quantity 是 int（平台相关宽度），库里是 int4。越界的值不在这里
		// 静默截断成一个合法的数量，而是原样报 422。
		if it.Quantity < 1 || it.Quantity > int(^uint32(0)>>1) {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "退款件数超出范围")
			return
		}
		req.Items = append(req.Items, service.RefundLineInput{OrderItemID: it.OrderItemId, Quantity: int32(it.Quantity)})
	}
	r, replayed, err := h.svc.Create(c.Request.Context(), c.Param("order_no"), req,
		c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusCreated, apiRefund(r))
}

// ListForOrder 实现 GET /api/v1/orders/:order_no/refunds。
func (h *RefundHandler) ListForOrder(c *gin.Context) {
	rows, err := h.svc.ListForOrder(c.Request.Context(), c.Param("order_no"))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiRefunds(rows))
}

// Detail 实现 GET /api/v1/refunds/:refund_no。
func (h *RefundHandler) Detail(c *gin.Context) {
	r, err := h.svc.Detail(c.Request.Context(), c.Param("refund_no"))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiRefund(r))
}

// Cancel 实现 POST /api/v1/refunds/:refund_no/cancel。
func (h *RefundHandler) Cancel(c *gin.Context) {
	r, replayed, err := h.svc.Cancel(c.Request.Context(), c.Param("refund_no"),
		c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	if replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusOK, apiRefund(r))
}

// apiRefunds 把一批退款单装成契约的 Refund 数组。空的时候是 []，不是 null ——
// 「这一单没有售后」与「没查过」是两件事。
func apiRefunds(rows []repository.Refund) []api.Refund {
	out := make([]api.Refund, 0, len(rows))
	for _, r := range rows {
		out = append(out, apiRefund(r))
	}
	return out
}

// apiRefund 把领域退款单装成契约的 Refund。
func apiRefund(r repository.Refund) api.Refund {
	goods := api.Money(r.GoodsAmountCents)
	freight := api.Money(r.FreightCents)
	reason := api.RefundReasonCode(r.ReasonCode)
	paymentNo := r.PaymentNo
	evidence := r.EvidenceURLs
	if evidence == nil {
		evidence = []string{}
	}
	updated := r.UpdatedAt
	items := make([]api.RefundItem, 0, len(r.Items))
	for _, it := range r.Items {
		title := it.Title
		items = append(items, api.RefundItem{
			OrderItemId: it.OrderItemID,
			Title:       &title,
			ImageUrl:    it.ImageURL,
			Quantity:    int(it.Quantity),
			AmountCents: api.Money(it.AmountCents),
		})
	}
	out := api.Refund{
		RefundNo:         r.RefundNo,
		OrderNo:          r.OrderNo,
		PaymentNo:        &paymentNo,
		RefundType:       api.RefundType(r.RefundType),
		Status:           api.RefundStatus(r.Status),
		Items:            items,
		GoodsAmountCents: &goods,
		FreightCents:     &freight,
		AmountCents:      api.Money(r.AmountCents),
		ChannelRefundId:  r.ChannelRefundID,
		ReasonCode:       &reason,
		ReasonText:       r.ReasonText,
		EvidenceUrls:     &evidence,
		RejectReason:     r.RejectReason,
		AuditedAt:        r.AuditedAt,
		RefundedAt:       r.RefundedAt,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        &updated,
	}
	if name, ok := channelNames[r.Channel]; ok {
		ch := api.RefundChannel(name)
		out.Channel = &ch
	}
	return out
}

// writeRefundError 把售后那几条接口的错误翻成契约里的响应。
//
// 后台两条（审核、确认收到退货）也走这里：判权（403）排在最前，其余分支两边共用。
func writeRefundError(c *gin.Context, err error) {
	if writePermissionError(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrRefundOrderNotFound):
		// 契约在申请退款的 404 上点名了 order-not-found。
		problem.Write(c, http.StatusNotFound, problem.TypeOrderNotFound, "订单不存在")

	case errors.Is(err, service.ErrOrderNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "订单不存在")

	case errors.Is(err, service.ErrRefundNotFound):
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "退款单不存在")

	case errors.Is(err, service.ErrOrderNotRefundable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeOrderStatusNotRefundable,
			"这笔订单当前不能申请退款", err)

	case errors.Is(err, service.ErrRefundAlreadyInProgress):
		writeProblemDetail(c, http.StatusConflict, problem.TypeRefundAlreadyInProgress,
			"这件商品已经有一张进行中的退款单", err)

	case errors.Is(err, service.ErrRefundQuantityExceeded):
		writeProblemDetail(c, http.StatusConflict, problem.TypeRefundQuantityExceeded,
			"退款件数超过了可退数量", err)

	case errors.Is(err, service.ErrOrderItemMismatch):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeOrderItemMismatch,
			"退款明细里有不属于这笔订单的商品", err)

	case errors.Is(err, service.ErrRefundNotCancelable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeRefundStatusNotCancelable,
			"这张退款单当前不能撤回（退款中的钱已在渠道路上）", err)

	case errors.Is(err, service.ErrRefundNotAuditable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeRefundStatusNotAuditable,
			"这张退款单当前不能审核（只有待审核的可以）", err)

	case errors.Is(err, service.ErrRefundNotReceivable):
		writeProblemDetail(c, http.StatusConflict, problem.TypeRefundStatusNotReceivable,
			"这张退款单当前不能确认收到退货（只有待买家退货的可以）", err)

	case errors.Is(err, service.ErrRefundFreightExceeded):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeRefundFreightExceeded,
			"退运费超过了订单实收运费", err)

	case errors.Is(err, service.ErrRefundBadRequest):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"退款请求参数不合法", err)

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
