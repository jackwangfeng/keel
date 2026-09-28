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

	// Items 的元素类型本轮（00058）从契约里的内联 schema 提成了具名的 OrderPreviewItem。
	// 此前这里要把匿名结构体原样再写一遍，而匿名类型多一个字段就不再可赋值 ——
	// 给每一行加活动价与活动分摊时正是撞上了这个（数据模型 §15 记过的那笔债）。
	// 具名之后字段照样由契约生成：契约改一个字，这段照样编译不过。
	items := make([]api.OrderPreviewItem, 0, len(q.Lines))
	for _, ln := range q.Lines {
		items = append(items, api.OrderPreviewItem{
			SkuId:                  ln.SKUID,
			Quantity:               int(ln.Quantity),
			PriceCents:             api.Money(ln.PriceCents),
			ListPriceCents:         api.Money(ln.ListPriceCents),
			PricePromotionId:       ln.PricePromotionID,
			AmountCents:            api.Money(ln.AmountCents),
			DiscountCents:          api.Money(ln.DiscountCents),
			PromotionDiscountCents: api.Money(ln.PromotionDiscountCents),
			AvailableQty:           availableOf(q.Available, ln.SKUID),
		})
	}

	discount := api.Money(q.DiscountCents)
	applicable := apiApplicableCoupons(q.ApplicableCoupons)
	regionID := q.Store.RegionID
	c.JSON(http.StatusOK, api.OrderPreview{
		// 回显门店与大区：契约把 store_id 定成必返，客户端靠它核对试算与下单是同一家店。
		StoreId:          q.Store.StoreID,
		RegionId:         &regionID,
		GoodsAmountCents: api.Money(q.GoodsAmountCents),
		PayableCents:     api.Money(q.PayableCents),
		DiscountCents:    &discount,
		Items:            items,

		// 运费（00056）：必返。商家没配任何模板时是算出来的 0（明细里 free_reason =
		// no_template），不是「没算」—— 试算一定带着收货地址，所以一定算过。
		FreightCents:         api.Money(q.FreightCents),
		FreightDiscountCents: api.Money(q.FreightDiscountCents),
		Freight:              apiFreightBreakdownOf(q.Freight),

		// 这个买家手里本单可用的券（与 POST /coupons/applicable 同一份实现、同一段渲染）。
		// 查过了才给：空数组的意思就是「真的一张都没有」。
		ApplicableCoupons: &applicable,
		UserCouponId:      q.UserCouponID,

		// 营销活动（00058）：DiscountCents = 活动 + 券，两个来源各给一份；
		// promotions 是命中了哪些、各减多少、还差多少凑满（与购物车同一段渲染）。
		PromotionDiscountCents: api.Money(q.PromotionDiscountCents),
		CouponDiscountCents:    api.Money(q.CouponDiscountCents),
		Promotions:             apiPromotionHits(q.Promotions),
	})
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
	freight := api.Money(o.FreightCents)
	freightDiscount := api.Money(o.FreightDiscountCents)
	discount := api.Money(o.DiscountCents)
	paid := api.Money(o.PaidCents)
	refunded := api.Money(o.RefundedCents)
	expire := o.ExpireAt
	regionID := o.RegionID
	promoDiscount := api.Money(o.PromotionDiscountCents)
	promotions := apiOrderPromotions(o.Promotions)
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

		// 这一单用的券；没用券时整个不出现。
		UserCouponId: o.UserCouponID,
		// 下单时的券名快照（00029），与 user_coupon_id 同进同出。
		CouponName: o.CouponName,

		// 运费（00056）：下单时算好写进订单。00056 之前的订单是 0 —— 那时确实没收运费，
		// 0 是账上的真值。实收运费 = freight_cents - freight_discount_cents。
		FreightCents:           &freight,
		FreightDiscountCents:   &freightDiscount,
		PromotionDiscountCents: &promoDiscount,
		Promotions:             &promotions,
	}
}

// writeOrderError 把 service 的业务错误翻成契约里那几种响应。
//
// 每一条都对应契约里明写的一个状态码。没对上的一律 500 —— 兜底分支不该猜一个
// 4xx，那会把服务端的 bug 报成客户端的错，而客户端会照着这个错重试。
func writeOrderError(c *gin.Context, err error) {
	// 拆分部署下试算 / 下单撞上活动报价要问库存服务的活动配额（微服务拆分阶段 1b），
	// 它不在时 503 inventory-unavailable（契约 info.description），单体不会走到这里。
	if writeInventoryUnavailable(c, err) {
		return
	}
	switch {
	case errors.Is(err, service.ErrCouponNotApplicable):
		// 契约：409 coupon-not-applicable，试算与下单同一个 type。detail 带原因
		// （门槛差多少、范围不含这家店……），客户端换一张券或不用券。
		// **绝不忽略这张券按原价继续**：那是用户以为用了券、实际按原价成交。
		writeProblemDetail(c, http.StatusConflict, problem.TypeCouponNotApplicable, "这张优惠券本单不可用", err)

	case errors.Is(err, service.ErrPromotionLimitExceeded):
		// 契约：409 promotion-limit-exceeded。detail 说明限购几件、已买几件，客户端减数量。
		// **不按门店价卖超出的那几件**：买家看到的是活动价，按另一个价成交是钱的问题。
		writeProblemDetail(c, http.StatusConflict, problem.TypePromotionLimitExceeded, "超出活动每人限购", err)

	case errors.Is(err, service.ErrPromotionSoldOut):
		// 契约：409 promotion-sold-out。秒杀配额在试算之后被抢光了；重新试算会按门店价报价。
		writeProblemDetail(c, http.StatusConflict, problem.TypePromotionSoldOut, "活动商品已抢光，请重新试算", err)

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

	case errors.Is(err, service.ErrRegionNotDeliverable):
		// 有商品送不到这个收货地址（00056）。**422 而不是 409**：重试不会成功，
		// 客户端该去掉这几行或换地址。undeliverable_items 逐行给出 SKU 与原因。
		writeUndeliverable(c, err)

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

	case errors.Is(err, service.ErrBelowMinimumOrder):
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeBelowMinimumOrder,
			"没到这家门店的起送价", err)

	case errors.Is(err, service.ErrAddressOutOfRange):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeAddressOutOfRange,
			"收货地址不在这家门店的配送范围内，请换一个地址或按地址重新选择门店")

	case errors.Is(err, service.ErrStoreClosed):
		problem.Write(c, http.StatusConflict, problem.TypeStoreUnavailable,
			"这家门店已停业或所在大区已停用，请重新定位或换一家门店")

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

	case errors.Is(err, service.ErrOrderNotCancelable):
		// 契约：409 order-status-not-cancelable。多半是刚付完款或刚被超时关掉，
		// 客户端刷新订单即可看到原因。
		problem.Write(c, http.StatusConflict, problem.TypeOrderStatusNotCancelable,
			"这笔订单当前不能取消（只有待支付的订单可以取消）")

	case errors.Is(err, service.ErrOrderNotConfirmable):
		problem.Write(c, http.StatusConflict, problem.TypeOrderStatusNotConfirmable,
			"这笔订单当前不能确认收货（只有已发货的订单可以确认收货）")

	case errors.Is(err, service.ErrIdempotencyKeyMissing):
		// 取消 / 确认收货把 Idempotency-Key 定成 required。422 而不是 400：
		// 这几条接口的错误集合里有 422 没有 400。
		problem.Write(c, http.StatusUnprocessableEntity,
			problem.TypeInvalidRequest, "缺少必填的 Idempotency-Key 请求头")

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

// writeUndeliverable 写 422 region-not-deliverable，带 undeliverable_items（契约 Problem）。
func writeUndeliverable(c *gin.Context, err error) {
	var ue *service.UndeliverableError
	items := []api.FreightUndeliverableLine{}
	if errors.As(err, &ue) {
		for _, l := range ue.Lines {
			items = append(items, apiUndeliverable(l))
		}
	}
	detail := err.Error()
	problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{
		Type:               problem.TypeRegionNotDeliverable,
		Title:              "有商品送不到这个收货地址",
		Status:             http.StatusUnprocessableEntity,
		Detail:             &detail,
		UndeliverableItems: &items,
	})
}

// availableOf 是试算行上的 available_qty：问不到库存时缺席。
func availableOf(m map[int64]int32, sku int64) *int {
	if m == nil {
		return nil
	}
	v := int(m[sku])
	return &v
}
