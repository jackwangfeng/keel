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

// 多收款退回（00150）：GET /api/v1/admin/payment-returns，以及订单详情上的 payment_returns。
// 单独一个文件：query 参数名要以字面量出现在 c.Query 里（contract_test.go 的参数对账按文件解析）。

// PaymentReturnHandler 是后台「多收款退回」列表。
type PaymentReturnHandler struct{ svc *service.PaymentReturnService }

// NewPaymentReturnHandler 建 handler。
func NewPaymentReturnHandler(s *service.PaymentReturnService) *PaymentReturnHandler {
	return &PaymentReturnHandler{svc: s}
}

type paymentReturnListResponse struct {
	api.PageMeta
	Items []api.PaymentReturn `json:"items"`
}

// List 实现 GET /api/v1/admin/payment-returns。
func (h *PaymentReturnHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	status := parseSmallint(c.Query("status"))
	out, err := h.svc.List(c.Request.Context(), status, page, pageSize)
	if err != nil {
		if writePermissionError(c, err) {
			return
		}
		if errors.Is(err, service.ErrBadRequest) {
			writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "筛选参数不合法", err)
			return
		}
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
		return
	}
	items := make([]api.PaymentReturn, 0, len(out.Items))
	for _, r := range out.Items {
		items = append(items, apiPaymentReturn(r, true))
	}
	c.JSON(http.StatusOK, paymentReturnListResponse{
		PageMeta: api.PageMeta{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total)},
		Items:    items,
	})
}

// apiPaymentReturn 装契约的 PaymentReturn。admin 为假（买家订单详情）时只给核心字段：
// 流水号、提交次数、失败原因是给后台排查的，买家看不懂也不需要。
func apiPaymentReturn(r repository.PaymentReturn, admin bool) api.PaymentReturn {
	out := api.PaymentReturn{ReturnNo: r.ReturnNo, AmountCents: api.Money(r.AmountCents),
		Reason: api.PaymentReturnReason(r.Reason), Status: api.PaymentReturnStatus(r.Status),
		CreatedAt: r.CreatedAt, ReturnedAt: r.ReturnedAt}
	if name, ok := channelNames[r.Channel]; ok && name != "balance" {
		ch := api.PaymentReturnChannel(name)
		out.Channel = &ch
	}
	if admin {
		no, attempts := r.OrderNo, int(r.Attempts)
		out.OrderNo, out.PaymentTxnId, out.Attempts, out.LastError = &no, r.PaymentTxnID, &attempts, r.LastError
	}
	return out
}

// apiPaymentReturns 是订单详情上的 payment_returns：没有时 nil（字段缺席）。
func apiPaymentReturns(rs []repository.PaymentReturn) *[]api.PaymentReturn {
	if len(rs) == 0 {
		return nil
	}
	out := make([]api.PaymentReturn, 0, len(rs))
	for _, r := range rs {
		out = append(out, apiPaymentReturn(r, false))
	}
	return &out
}
