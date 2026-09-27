package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 后台的两条详情：GET /admin/orders/{order_no} 与 GET /admin/refunds/{refund_no}，
// 以及后台订单 / 退款单在契约形状上的装配（两个列表文件也用这里的函数）。
//
// 两条详情都没有 query 参数，放在同一个文件里；列表各有一串 query 参数，
// 各自一个文件（contract_test.go 的参数对账按文件解析）。

// OrderDetail 实现 GET /api/v1/admin/orders/:order_no。
func (h *AdminOrderHandler) OrderDetail(c *gin.Context) {
	d, err := h.svc.OrderDetail(c.Request.Context(), c.Param("order_no"))
	if err != nil {
		writeAdminOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminOrderDetail(d))
}

// RefundDetail 实现 GET /api/v1/admin/refunds/:refund_no。
func (h *AdminOrderHandler) RefundDetail(c *gin.Context) {
	d, err := h.svc.RefundDetail(c.Request.Context(), c.Param("refund_no"))
	if err != nil {
		writeRefundError(c, err)
		return
	}
	c.JSON(http.StatusOK, apiAdminRefundDetail(d))
}

// apiAdminOrderSummary 把后台订单摘要装成契约的 AdminOrderSummary。
//
// 契约里它是 `allOf: [Order, {...}]`，生成器把它摊成了一个扁平结构体，
// 所以 Order 那一半要从 apiOrder 逐字段搬 —— 与 order_detail.go 同一个处境。
// 漏搬一个字段的症状是 JSON 里整个不出现（契约里可选），钉住它的是
// admin_order_test.go 里「后台摘要的 Order 部分与买家侧同一单逐键一致」那条断言。
//
// 运费（00056 起买家侧也照实给出）：后台审核退货退款时裁定退运费，服务端的上限是
// 实收运费 freight_cents - freight_discount_cents（refund-freight-exceeded 的判据），
// 审核人得看得到这两个数。
func apiAdminOrderSummary(s service.AdminOrderSummary) api.AdminOrderSummary {
	base := apiOrder(s.Order.Order)
	return api.AdminOrderSummary{
		OrderNo:          base.OrderNo,
		StoreId:          base.StoreId,
		RegionId:         base.RegionId,
		Status:           base.Status,
		RefundStatus:     base.RefundStatus,
		PayableCents:     base.PayableCents,
		GoodsAmountCents: base.GoodsAmountCents,
		DiscountCents:    base.DiscountCents,
		FreightCents:     base.FreightCents,
		// 包邮券抵掉的运费（00056）。
		FreightDiscountCents: base.FreightDiscountCents,
		PaidCents:            base.PaidCents,
		RefundedCents:        base.RefundedCents,
		ExpireAt:             base.ExpireAt,
		CreatedAt:            base.CreatedAt,
		PaidAt:               base.PaidAt,
		ShippedAt:            base.ShippedAt,
		FinishedAt:           base.FinishedAt,
		UserCouponId:         base.UserCouponId,
		CouponName:           base.CouponName,

		PromotionDiscountCents: base.PromotionDiscountCents,
		Promotions:             base.Promotions,

		Receiver:      apiReceiver(s.Receiver),
		Store:         apiStoreSnapshot(s.Store),
		HasOpenRefund: s.Order.HasOpenRefund,
	}
}

func apiAdminOrderDetail(d service.AdminOrderDetail) api.AdminOrderDetail {
	s := apiAdminOrderSummary(d.AdminOrderSummary)
	shipments := make([]api.Shipment, 0, len(d.Shipments))
	for _, sh := range d.Shipments {
		shipments = append(shipments, apiShipment(sh))
	}
	refunds := make([]api.AdminRefund, 0, len(d.Refunds))
	for _, r := range d.Refunds {
		refunds = append(refunds, apiAdminRefund(r))
	}
	return api.AdminOrderDetail{
		OrderNo:              s.OrderNo,
		StoreId:              s.StoreId,
		RegionId:             s.RegionId,
		Status:               s.Status,
		RefundStatus:         s.RefundStatus,
		PayableCents:         s.PayableCents,
		GoodsAmountCents:     s.GoodsAmountCents,
		DiscountCents:        s.DiscountCents,
		FreightCents:         s.FreightCents,
		FreightDiscountCents: s.FreightDiscountCents,
		// 下单那一刻的运费明细快照；00056 之前的订单没有，整个不出现。
		Freight:                apiFreightBreakdownPtr(d.Freight),
		PaidCents:              s.PaidCents,
		RefundedCents:          s.RefundedCents,
		ExpireAt:               s.ExpireAt,
		CreatedAt:              s.CreatedAt,
		PaidAt:                 s.PaidAt,
		ShippedAt:              s.ShippedAt,
		FinishedAt:             s.FinishedAt,
		UserCouponId:           s.UserCouponId,
		CouponName:             s.CouponName,
		Receiver:               s.Receiver,
		Store:                  s.Store,
		HasOpenRefund:          s.HasOpenRefund,
		PromotionDiscountCents: s.PromotionDiscountCents,
		Promotions:             s.Promotions,

		Items:     apiOrderItems(d.Items),
		Payments:  apiPayments(d.Payments),
		Shipments: shipments,
		Refunds:   refunds,
	}
}

// apiAdminRefund 把后台退款单装成契约的 AdminRefund（`allOf: [Refund, {...}]`，
// 同样被摊平了，Refund 那一半从 apiRefund 搬）。
func apiAdminRefund(v service.AdminRefundView) api.AdminRefund {
	b := apiRefund(v.Refund.Refund)
	out := api.AdminRefund{
		RefundNo:         b.RefundNo,
		OrderNo:          b.OrderNo,
		PaymentNo:        b.PaymentNo,
		RefundType:       b.RefundType,
		Status:           b.Status,
		Items:            b.Items,
		GoodsAmountCents: b.GoodsAmountCents,
		FreightCents:     b.FreightCents,
		AmountCents:      b.AmountCents,
		ChannelRefundId:  b.ChannelRefundId,
		ReasonCode:       b.ReasonCode,
		ReasonText:       b.ReasonText,
		EvidenceUrls:     b.EvidenceUrls,
		ReturnShipment:   b.ReturnShipment,
		RejectReason:     b.RejectReason,
		AuditedAt:        b.AuditedAt,
		ReturnDeadlineAt: b.ReturnDeadlineAt,
		RefundedAt:       b.RefundedAt,
		CreatedAt:        b.CreatedAt,
		UpdatedAt:        b.UpdatedAt,

		StoreId:     v.Refund.StoreID,
		Store:       apiStoreSnapshot(v.Store),
		OrderStatus: api.OrderStatus(v.Refund.OrderStatus),
		AuditedBy:   apiStaffRef(v.Refund.AuditedBy),
		ReceivedAt:  v.Refund.ReceivedAt,
		ReceivedBy:  apiStaffRef(v.Refund.ReceivedBy),
	}
	if b.Channel != nil {
		ch := api.AdminRefundChannel(*b.Channel)
		out.Channel = &ch
	}
	return out
}

func apiAdminRefundDetail(d service.AdminRefundDetail) api.AdminRefundDetail {
	r := apiAdminRefund(d.AdminRefundView)
	out := api.AdminRefundDetail{
		RefundNo:         r.RefundNo,
		OrderNo:          r.OrderNo,
		PaymentNo:        r.PaymentNo,
		RefundType:       r.RefundType,
		Status:           r.Status,
		Items:            r.Items,
		GoodsAmountCents: r.GoodsAmountCents,
		FreightCents:     r.FreightCents,
		AmountCents:      r.AmountCents,
		ChannelRefundId:  r.ChannelRefundId,
		ReasonCode:       r.ReasonCode,
		ReasonText:       r.ReasonText,
		EvidenceUrls:     r.EvidenceUrls,
		ReturnShipment:   r.ReturnShipment,
		RejectReason:     r.RejectReason,
		AuditedAt:        r.AuditedAt,
		ReturnDeadlineAt: r.ReturnDeadlineAt,
		RefundedAt:       r.RefundedAt,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		StoreId:          r.StoreId,
		Store:            r.Store,
		OrderStatus:      r.OrderStatus,
		AuditedBy:        r.AuditedBy,
		ReceivedAt:       r.ReceivedAt,
		ReceivedBy:       r.ReceivedBy,

		Order: apiAdminOrderSummary(d.Order),
	}
	if r.Channel != nil {
		ch := api.AdminRefundDetailChannel(*r.Channel)
		out.Channel = &ch
	}
	return out
}

// apiStaffRef 把审核记录里的「谁」装成契约的 StaffRef。名字读不到（平台级员工）
// 或是空串时整个不出现，只剩 id。
func apiStaffRef(s *repository.StaffRef) *api.StaffRef {
	if s == nil {
		return nil
	}
	out := &api.StaffRef{Id: s.ID}
	if s.Name != nil && *s.Name != "" {
		name := *s.Name
		out.Name = &name
	}
	return out
}

func apiReceiver(r service.ReceiverSnapshot) api.ReceiverSnapshot {
	return api.ReceiverSnapshot{
		ReceiverName: r.ReceiverName,
		Phone:        r.Phone,
		Province:     r.Province,
		City:         r.City,
		District:     r.District,
		Street:       optStr(r.Street),
		Detail:       r.Detail,
		RegionCode:   r.RegionCode,
		PostalCode:   r.PostalCode,
	}
}

func apiStoreSnapshot(s service.StoreSnapshot) api.OrderStoreSnapshot {
	return api.OrderStoreSnapshot{
		StoreName:    s.StoreName,
		RegionName:   optStr(s.RegionName),
		StoreAddress: optStr(s.Address),
		StorePhone:   optStr(s.Phone),
	}
}

// writeAdminListError 是两条后台列表的错误出口：判权（理论上列表不会被拒 ——
// 范围只收窄、不拒绝 —— 但未知的判权错误照样按契约翻）、筛选参数不合法 422，其余 500。
func writeAdminListError(c *gin.Context, err error) {
	if writePermissionError(c, err) || writeInventoryUnavailable(c, err) {
		return
	}
	if errors.Is(err, service.ErrAdminListBadRequest) {
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"筛选参数不合法", err)
		return
	}
	_ = c.Error(err)
	problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "服务内部错误")
}

// adminListTime 解析列表上的一个可选时间参数（RFC3339）。空串即没传。
//
// 解析失败**不当成没传**（与 page / status 那几个参数的宽容处理刻意相反）：
// 契约给这两条列表声明了 422，因为一个写错的时间范围回一页不带筛选的全量结果，
// 会让人以为那段时间就这么多单。
func adminListTime(raw, name string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s 不是合法的 RFC3339 时间：%q", service.ErrAdminListBadRequest, name, raw)
	}
	return &t, nil
}

// adminListInt64 解析一个可选的正整数 id 参数。读不动当没传，与 GET /admin/stores 的
// region_id 一致（筛选 id 写错时拿到的是更宽的列表，不是一个错误的窄列表）。
func adminListInt64(raw string) *int64 {
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return nil
	}
	return &v
}

// adminListText 把一个可选的文本参数变成指针；空串即没传。
func adminListText(raw string) *string {
	if raw == "" {
		return nil
	}
	return &raw
}
