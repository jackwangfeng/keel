package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 订单详情：GET /api/v1/orders/{order_no}。
//
// 单独一个文件，理由同 product_detail.go 的文件头（contract_test.go 按文件对账
// query 参数，这条接口一个都没有）。

// channelNames 是 payments.channel 到契约那个字符串枚举的反向映射。
//
// 正向那张表在 service/payment.go（webhookChannels），这里是它的逆。两张表而不是
// 一张双向的：正向那张是**白名单**（哪些渠道可以从 webhook 进来，balance 刻意
// 不在里面），逆向这张是**展示**（库里存着 3 就得显示 balance，不然余额支付的
// 历史记录会变成一片空白）。合成一张的话，往白名单里加 balance 就成了「顺手」的
// 一步，而那一步会开出一条用伪造的余额回调把订单推成已支付的路。
var channelNames = map[int16]string{
	repository.PaymentChannelWechat:  "wechat",
	repository.PaymentChannelAlipay:  "alipay",
	repository.PaymentChannelBalance: "balance",
}

// Detail 实现 GET /api/v1/orders/{order_no}。
func (h *OrderHandler) Detail(c *gin.Context) {
	d, err := h.svc.Detail(c.Request.Context(), c.Param("order_no"))
	if err != nil {
		// service.ErrOrderNotFound → 404（writeOrderError 里那一支）。
		// 它同时覆盖「这一单是别人的」，所以这条路径就是买家隔离在 HTTP 层的出口。
		writeOrderError(c, err)
		return
	}

	base := apiOrder(d.Order)
	items := apiOrderItems(d.Items)
	payments := apiPayments(d.Payments)
	refunds := apiRefunds(d.Refunds)
	receiver := api.ReceiverSnapshot{
		ReceiverName: d.Receiver.ReceiverName,
		Phone:        d.Receiver.Phone,
		Province:     d.Receiver.Province,
		City:         d.Receiver.City,
		District:     d.Receiver.District,
		Street:       &d.Receiver.Street,
		Detail:       d.Receiver.Detail,
		RegionCode:   d.Receiver.RegionCode,
		PostalCode:   d.Receiver.PostalCode,
	}

	// OrderDetail 在契约里是 `allOf: [Order, {...}]`，而生成器把它摊成了一个
	// 独立的扁平结构体（不是内嵌 api.Order）。所以这里要逐字段搬一遍。
	//
	// 搬而不是给 api.Order 加几个字段：那个类型由契约生成，手改会被
	// check-all.sh 的产物闸门当场抓住。字段漏搬一个会怎样：契约里它是可选的，
	// JSON 里就整个不出现 —— 所以下面这段必须与 apiOrder 逐行对齐，
	// 而 order_query_test.go 里那条「详情与列表对同一单给出同样的 Order 部分」
	// 的断言就是钉这件事的。
	regionID := d.Order.RegionID
	store := api.OrderStoreSnapshot{
		StoreName:    d.Store.StoreName,
		RegionName:   optStr(d.Store.RegionName),
		StoreAddress: optStr(d.Store.Address),
		StorePhone:   optStr(d.Store.Phone),
	}

	c.JSON(http.StatusOK, api.OrderDetail{
		OrderNo:          base.OrderNo,
		Status:           base.Status,
		RefundStatus:     base.RefundStatus,
		PayableCents:     base.PayableCents,
		GoodsAmountCents: base.GoodsAmountCents,
		DiscountCents:    base.DiscountCents,
		// 这一单用的券（00026）。优惠券那一棒给 apiOrder 加了它、这里漏搬了 ——
		// 正是上面那段注释预言的症状：契约里可选，漏了 JSON 里就整个不出现。
		UserCouponId:  base.UserCouponId,
		PaidCents:     base.PaidCents,
		RefundedCents: base.RefundedCents,
		FreightCents:  base.FreightCents,
		ExpireAt:      base.ExpireAt,
		CreatedAt:     base.CreatedAt,
		PaidAt:        base.PaidAt,
		ShippedAt:     base.ShippedAt,
		FinishedAt:    base.FinishedAt,

		// 履约门店。**store_id 在契约里是必返**（库里 NOT NULL），
		// region_id 与 store 都是可选。三个一起给：store_id 让客户端能拿它
		// 去打 /admin 或再下一单，store 是**下单当时**的展示快照 ——
		// 门店改名、搬家、换大区之后，这一单的详情页仍然显示当时那个名字。
		StoreId:  d.Order.StoreID,
		RegionId: &regionID,
		Store:    &store,

		Receiver: &receiver,
		Items:    &items,
		Payments: &payments,

		// 退款域（00034）落地之后这是**查过的**：空数组的意思就是「这一单没有售后」。
		Refunds: &refunds,
	})
}

// apiOrderItems 把订单行装成契约的 OrderItem。
func apiOrderItems(rows []repository.OrderItem) []api.OrderItem {
	out := make([]api.OrderItem, 0, len(rows))
	for _, it := range rows {
		amount := api.Money(it.AmountCents)
		discount := api.Money(it.DiscountCents)
		product := it.ProductID
		refunded := int(it.RefundedQty)
		refunding := int(it.RefundingQty)

		// 规格快照解不开时给一个空 map，**不让整条请求失败**。
		//
		// 这里与 service 里 decodeSpec 的处置刻意相反，因为两处的「解不开」
		// 意味着不同的东西：商品详情读的是 skus 的当前值（解不开 = 数据坏了，
		// 该炸出来），而这里读的是一份**历史快照**，它可能是几个月前由一版
		// 更老的代码写下的。为一行读不懂的旧快照让用户打不开自己的订单，
		// 换来的不是正确性，是一个再也修不了的历史订单。
		spec := map[string]string{}
		if m, err := service.DecodeSpecValues(it.SpecSnapshot); err == nil {
			spec = m
		}

		out = append(out, api.OrderItem{
			Id:            it.ID,
			SkuId:         it.SKUID,
			ProductId:     &product,
			Title:         it.TitleSnapshot,
			SpecValues:    &spec,
			ImageUrl:      it.ImageSnapshot,
			PriceCents:    api.Money(it.PriceCents),
			Quantity:      int(it.Quantity),
			AmountCents:   &amount,
			DiscountCents: &discount,

			// RefundedQty 是 order_items 上一列真实存在的数（DDL 里
			// NOT NULL DEFAULT 0），所以它填得出来，今天恒为 0 也照填。
			RefundedQty: &refunded,

			// RefundingQty 是 `refund_items ⋈ refunds WHERE status IN (10,20,30)`
			// 的聚合（§11），由 service 在读订单的同一个事务里补上。
			// 客户端据此算「还可退 = quantity - refunded_qty - refunding_qty」。
			RefundingQty: &refunding,
		})
	}
	return out
}

// apiPayments 把支付记录装成契约的 PaymentRecord。
func apiPayments(rows []repository.Payment) []api.PaymentRecord {
	out := make([]api.PaymentRecord, 0, len(rows))
	for _, p := range rows {
		no := p.PaymentNo
		amount := api.Money(p.AmountCents)
		status := int(p.Status)
		rec := api.PaymentRecord{
			PaymentNo:   &no,
			AmountCents: &amount,
			Status:      &status,
			PaidAt:      p.PaidAt,
		}
		// 认不出来的渠道号让 channel 缺席，而不是回一个 ""。
		// 空串不在契约的枚举里，按契约生成的客户端会在它上面解析失败；
		// 缺席则是一个它本来就要处理的情况（这是可选字段）。
		if name, ok := channelNames[p.Channel]; ok {
			ch := api.PaymentRecordChannel(name)
			rec.Channel = &ch
		}
		out = append(out, rec)
	}
	return out
}
