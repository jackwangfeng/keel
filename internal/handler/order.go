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

// 下单主链路的两条接口：POST /orders/preview（试算）与 POST /orders（建单）。
//
// 它们在契约里**共用同一个请求 schema**（OrderCreateRequest），这里也共用同一个
// 绑定类型与同一次转换。共用是这条链路上最要紧的一件事：试算和真下单读出不同的
// 请求、算出不同的钱，是这条链路最严重的一类 bug（见 service/pricing.go 的文件头）。

// idempotencyKeyHeader 是契约里那个必填请求头。
const idempotencyKeyHeader = "Idempotency-Key"

// idempotencyReplayedHeader 告诉客户端「这是上次那单，本次调用没有真正执行」。
//
// 契约明写它影响埋点与提示文案 —— 没有它，客户端分不清「我真的下了单」与
// 「这是一次重放」，于是连点两下会被记成两次转化。
const idempotencyReplayedHeader = "Idempotency-Replayed"

// retryAfterSeconds 是 409 幂等键处理中时建议的退避秒数。
//
// 2 秒的依据是 sagaWaitTimeout：并发的第二个请求撞上「处理中」时，第一个请求
// 最多还要等 15 秒。退避 2 秒让客户端在正常情况下（SAGA 通常几十毫秒）几乎
// 立刻就能拿到重放结果，而不是空等一个固定的长间隔。
const retryAfterSeconds = 2

type OrderHandler struct{ svc *service.OrderService }

func NewOrderHandler(s *service.OrderService) *OrderHandler { return &OrderHandler{svc: s} }

// Preview 实现 POST /api/v1/orders/preview。**无副作用。**
func (h *OrderHandler) Preview(c *gin.Context) {
	req, ok := bindOrderRequest(c)
	if !ok {
		return
	}
	q, err := h.svc.Preview(c.Request.Context(), req)
	if err != nil {
		writeOrderError(c, err)
		return
	}

	// Items 的元素类型是契约生成出来的**匿名结构体**，所以这里要把它原样再写
	// 一遍才能 make 出来。看着是重复，实际是本仓库要的那种重复：契约里
	// OrderPreview.items 的字段名、类型或 json tag 改了一个字，这段就编译不过
	// （匿名结构体的类型同一性要求字段、顺序、tag 全等），而不是等到线上客户端
	// 解析失败。
	items := make([]struct {
		// AmountCents 金额，单位「分」。禁止使用浮点。
		AmountCents *api.Money `json:"amount_cents,omitempty"`

		// DiscountCents 该行分摊到的优惠。余数归金额最大行，保证求和恒等。
		DiscountCents *api.Money `json:"discount_cents,omitempty"`
		Quantity      *int       `json:"quantity,omitempty"`
		SkuId         *int64     `json:"sku_id,omitempty"`
	}, 0, len(q.Lines))

	for _, ln := range q.Lines {
		amount := api.Money(ln.AmountCents)
		discount := api.Money(ln.DiscountCents)
		qty := int(ln.Quantity)
		sku := ln.SKUID
		items = append(items, struct {
			AmountCents   *api.Money `json:"amount_cents,omitempty"`
			DiscountCents *api.Money `json:"discount_cents,omitempty"`
			Quantity      *int       `json:"quantity,omitempty"`
			SkuId         *int64     `json:"sku_id,omitempty"`
		}{
			AmountCents:   &amount,
			DiscountCents: &discount,
			Quantity:      &qty,
			SkuId:         &sku,
		})
	}

	discount := api.Money(q.DiscountCents)
	c.JSON(http.StatusOK, api.OrderPreview{
		GoodsAmountCents: api.Money(q.GoodsAmountCents),
		PayableCents:     api.Money(q.PayableCents),
		DiscountCents:    &discount,
		Items:            items,

		// FreightCents 刻意留 nil：**本期不计运费**，不是「算出来是 0」。
		// 理由与两者的差别写在 service/pricing.go 的 freightNotBilledThisRelease。
		// 这笔账挂在 contract_test.go 的 NotYetImplementedResponse 里。
		FreightCents: freightNotBilledThisRelease(q),

		// ApplicableCoupons 同样留 nil：券的三张表本轮没建，所以这里不是
		// 「一张可用券都没有」，而是**没查过**。回一个空数组会让前端显示
		// 「暂无可用优惠券」—— 一句在券上线之前都不会被纠正的假话。
	})
}

// freightNotBilledThisRelease 把 Quote 里那个 nil 原样递出去。
//
// 单独一个函数而不是直接写 nil：它让「响应里这个字段为什么不见了」有一处可以
// 跳转过去的落点，也让将来真的实现运费时，编译器会把每一个调用点都指出来。
func freightNotBilledThisRelease(q service.Quote) *api.Money {
	if q.FreightCents == nil {
		return nil
	}
	m := api.Money(*q.FreightCents)
	return &m
}

// Create 实现 POST /api/v1/orders。
func (h *OrderHandler) Create(c *gin.Context) {
	req, ok := bindOrderRequest(c)
	if !ok {
		return
	}
	res, err := h.svc.Create(c.Request.Context(), req, c.GetHeader(idempotencyKeyHeader))
	if err != nil {
		writeOrderError(c, err)
		return
	}
	if res.Replayed {
		c.Header(idempotencyReplayedHeader, "true")
	}
	c.JSON(http.StatusCreated, apiOrder(res.Order))
}

// bindOrderRequest 把请求体绑成 service 的入参。两条接口共用。
//
// 绑定的是 api.OrderCreateRequest（契约生成的类型），不是手写结构体：
// 契约里改个字段名，这里当场编译失败，而不是等到线上客户端发现自己传的东西
// 被服务端忽略了。
func bindOrderRequest(c *gin.Context) (service.CreateRequest, bool) {
	var raw api.OrderCreateRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求体不是合法的 JSON")
		return service.CreateRequest{}, false
	}
	items := make([]service.LineInput, 0, len(raw.Items))
	for _, it := range raw.Items {
		// 契约里 quantity 是 int（平台相关宽度），而库里是 int4。
		// 在这里收口成 int32，越界的值由 service 的 [1, 999] 校验拦下来 ——
		// 不在这里静默截断：截断会把一个荒谬的数量变成一个合法的数量。
		q := it.Quantity
		if q < 0 || q > int(^uint32(0)>>1) {
			problem.Write(c, http.StatusUnprocessableEntity,
				problem.TypeInvalidRequest, "商品数量超出范围")
			return service.CreateRequest{}, false
		}
		items = append(items, service.LineInput{SKUID: it.SkuId, Quantity: int32(q)})
	}
	return service.CreateRequest{
		Items:     items,
		AddressID: raw.AddressId,
		// store_id 在契约里是 **required**，所以这里原样透传，**不填默认值**。
		//
		// 少了这一行（或者在这里补一个「没传就用默认店」）的后果不是 400，
		// 是一笔按错误门店成交的订单：买家在 A 店看到的价与库存，
		// 服务端按 B 店扣减、按 B 店的价记账，而两边都是合法数据，
		// 没有任何东西会响。所以取值校验放在 service.orderScope 那一处，
		// 它连「回落」那条路径都没有写。
		StoreID:              raw.StoreId,
		Remark:               raw.Remark,
		ExpectedPayableCents: raw.ExpectedPayableCents,
		UserCouponID:         raw.UserCouponId,
	}, true
}

// apiOrder 把领域订单装成契约的 Order。
func apiOrder(o repository.Order) api.Order {
	goods := api.Money(o.GoodsAmountCents)
	discount := api.Money(o.DiscountCents)
	paid := api.Money(o.PaidCents)
	refunded := api.Money(o.RefundedCents)
	expire := o.ExpireAt
	regionID := o.RegionID
	return api.Order{
		OrderNo: o.OrderNo,
		// store_id 必返（库里 NOT NULL），region_id 可选但一起给：
		// 这一单的价格是按**下单当时**那个大区算的，而门店可以被调到别的
		// 大区去。要能事后回答「这个价是怎么来的」，就得把当时那一个留下来。
		StoreId:          o.StoreID,
		RegionId:         &regionID,
		Status:           api.OrderStatus(o.Status),
		RefundStatus:     api.OrderRefundStatus(o.RefundStatus),
		PayableCents:     api.Money(o.PayableCents),
		GoodsAmountCents: &goods,
		DiscountCents:    &discount,
		PaidCents:        &paid,
		RefundedCents:    &refunded,
		ExpireAt:         &expire,
		CreatedAt:        o.CreatedAt,

		// 三个可空时间戳原样递出去：库里是 NULL 就让它们在 JSON 里整个不出现。
		// 它们的缺席是有意义的信息 —— paid_at 没有就是「还没付」，
		// 而一个 0001-01-01 会让客户端显示一个荒唐的日期，或者更糟：
		// 让「已支付但没有付款时间」这种数据损坏看上去很正常。
		PaidAt:     o.PaidAt,
		ShippedAt:  o.ShippedAt,
		FinishedAt: o.FinishedAt,

		// FreightCents 同 preview：本期不计运费，字段整个不出现。
		// 库里那一列是 0（chk_amount 的恒等式要它），但那是账，不是「算过了」。
	}
}

// writeOrderError 把 service 的业务错误翻成契约里那几种响应。
//
// 每一条都对应契约里明写的一个状态码。没对上的一律 500 —— 兜底分支不该猜一个
// 4xx，那会把服务端的 bug 报成客户端的错，而客户端会照着这个错重试。
func writeOrderError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrCouponNotImplemented):
		// 501 而不是静默忽略。**传了券却被忽略 = 用户以为用了券、实际按原价
		// 成交**，那是钱的问题，而且客户端没有任何办法发现。
		//
		// 契约允许这个状态码吗：POST /orders 的响应集合里有 default（收其余
		// 一切，体是 Problem），所以允许。**POST /orders/preview 的响应集合里
		// 只有 200，没有 default** —— 那是契约自己的一处缺口（它连 404 都没法
		// 表达），已在报告里列为 defer：给 /orders/preview 补 default: Problem。
		// 本轮选择返回 501 而不是为了「契约合规」去假装试算成功。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"优惠券尚未实现：券的三张表还没有建。请先不要传 user_coupon_id")

	case errors.Is(err, service.ErrPriceChanged):
		// 契约明写：「服务端试算不一致时返回 409，防止价格变动导致用户以旧价成交」。
		problem.Write(c, http.StatusConflict, problem.TypePriceChanged,
			"商品价格已变动，请重新试算后再提交")

	case errors.Is(err, service.ErrInsufficientStock):
		problem.Write(c, http.StatusConflict, problem.TypeInsufficientStock,
			"库存不足")

	case errors.Is(err, service.ErrIdempotencyInFlight):
		// 契约：409 + Retry-After，客户端应退避重试，**不要当成业务失败弹窗**。
		// Retry-After 在 problem.Write 之前设：那个函数会 Abort。
		c.Header("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem.Write(c, http.StatusConflict, problem.TypeIdempotencyKeyInFlight,
			"这个 Idempotency-Key 正在处理中，请稍后重试")

	case errors.Is(err, service.ErrIdempotencyKeyReused):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeIdempotencyKeyReused,
			"同一个 Idempotency-Key 配了不同的请求体")

	case errors.Is(err, service.ErrAddressNotFound):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "收货地址不存在")

	case errors.Is(err, service.ErrSKUUnavailable):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求里有不可售的商品")

	// 门店那两条（00020）。**两条都是 422，按 type 分**，契约在 POST /orders
	// 与 POST /orders/preview 的 422 描述里逐条写着。
	case errors.Is(err, repository.ErrSKUNotSoldInStore):
		// 这家店（或它所在大区）把这件商品下架了。
		//
		// **刻意不是 409。** 409 在这份契约里被客户端读成「重读一次再试」——
		// Retry-After 与 InventoryConflict.current 都在教它这么读，
		// 而这一种重试永远不会成功：客户端该做的是换一家店。
		// 也刻意不是 404：那条分界线是「路径里指名的资源不存在 → 404，
		// 请求体里指名的东西不存在或不可用 → 422」，而这条路径是 /orders，
		// 报 404 会被读成「下单接口不存在」。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeSKUNotSoldInStore, "这家门店不卖请求里的某些商品，请换一家门店")

	case errors.Is(err, service.ErrStoreNotFound):
		// store_id 服务端不认识。与「不是你的 SKU」合用 invalid-request 是
		// 契约定的：两者对客户端是同一件事 —— 请求体里指名了一个不存在的东西。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "store_id 指向的门店不存在或不属于当前店铺")

	case errors.Is(err, service.ErrBadRequest):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "请求参数不合法")

	case errors.Is(err, service.ErrOrderNotFound):
		// 契约里 GET /orders/{order_no} 与 POST /orders/{order_no}/payments
		// 都明写了 404。它同时覆盖「没有这一单」与「这一单是别人的」——
		// 分开报会把这条接口变成一个单号存在性判定器（service/order_query.go）。
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "订单不存在")

	case errors.Is(err, service.ErrOrderNotPayable):
		// 契约：409 `.../order-status-not-payable`，描述里明写「非 10 待支付，
		// 或已超时关闭」。刻意不是 422：请求本身没毛病，是这一单的状态变了，
		// 客户端该做的是刷新订单而不是改参数重发。
		problem.Write(c, http.StatusConflict, problem.TypeOrderStatusNotPayable,
			"这笔订单当前不能支付（已支付、已关闭或已超时）")

	case errors.Is(err, service.ErrPaymentChannelUnknown):
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "不认识的支付渠道")

	case errors.Is(err, service.ErrBalancePaymentNotImplemented):
		// 501 而不是静默当成微信支付。余额支付不走渠道回调，它要自己的账户表，
		// 而数据模型里没有那张表 —— 假装受理会让用户以为钱从余额里扣了。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"余额支付尚未实现：余额账户表还没有建")

	case errors.Is(err, service.ErrSandboxDisabled):
		// 沙箱关了而真渠道没接。这是那种配置下唯一诚实的回答。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"支付未开通：本部署关闭了沙箱支付，而真实支付渠道尚未对接")

	case errors.Is(err, service.ErrSandboxNoSecret):
		// 这家店没配回调密钥。是**部署没配好**，不是客户端的错，所以要留日志。
		_ = c.Error(err)
		problem.Write(c, http.StatusNotImplemented, problem.TypeNotImplemented,
			"支付未开通：本店没有配置该渠道的回调密钥")

	case errors.Is(err, service.ErrCrossTenantSKU):
		// **刻意不是 409「库存不足」。**
		//
		// 这是硬约束三的落点：把一次跨租户访问（或一个没有库存行的 SKU）
		// 翻译成一次正常的缺货，会让它在监控上只表现为库存波动，而客户端会
		// 提示用户「换一件商品」—— 真正发生的事情根本不在客户端能处理的范围里。
		// 500 + 一条 Error 日志才是对的。
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")

	default:
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError,
			problem.TypeInternal, "服务内部错误")
	}
}
