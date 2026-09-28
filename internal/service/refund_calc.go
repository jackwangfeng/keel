package service

import (
	"errors"
	"fmt"

	"github.com/keel/keel/internal/repository"
)

// 退款金额怎么算（数据模型 §11「§7 那句话的兑现」）。**纯函数**，不碰数据库，
// 所以它的每一条规则都能在没有库的单元测试里逐分核对（refund_calc_test.go）。
//
// 设某行实付净额 net = amount_cents - discount_cents（discount_cents 是下单时
// 券按行分摊下来的那一份，§7），购买 quantity 件，已退 refunded_qty 件 /
// refunded_cents 分，本次退 k 件：
//
//	refunded_qty + k < quantity ：本次 = floor(net × k / quantity)
//	refunded_qty + k = quantity ：本次 = net - refunded_cents（退干净剩下的）
//
// 于是一行退完时 SUM(refund_items.amount_cents) = net 恒等 —— 用户「退完了，
// 钱退全了」。这是 README 那句「优惠按行分摊，所以部分退款能算对金额」的全部内容：
// 没有 discount_cents，买三件用了满减券、退一件时只能按原价退（多退）或者
// 按整单比例估（对不平）。
//
// **为什么这个公式在多次部分退款下仍然成立**：同一行任何时刻至多一张在途退款单
// （planRefund 拒绝在途行再申请，契约的 refund-already-in-progress），所以
// 申请时读到的 refunded_qty / refunded_cents 到入账那一刻不会变 —— 入账前唯一
// 能改它们的是这一行别的退款单的入账，而那样的单此刻不存在。

// ErrRefundBadRequest：请求体不合法（件数 < 1、同一行出现两次、字段越界……）。
// 契约：422 invalid-request。
var ErrRefundBadRequest = errors.New("退款申请参数不合法")

// RefundLineInput 是申请退款的一行（契约 RefundItemInput）。
type RefundLineInput struct {
	OrderItemID int64 `json:"order_item_id"`
	Quantity    int32 `json:"quantity"`
}

// refundPlan 是 planRefund 的结果。
type refundPlan struct {
	Lines []repository.NewRefundItem
	// Goods 是本次退的货款之和。
	Goods int64
	// CoversEverything：这一单没有任何在途退款，而且本次把每一行剩下的件数都退了 ——
	// 即「整单退」。只有它配上「未发货」才进 50 退款中、才全退运费（§5 / §11）。
	CoversEverything bool
	// ClosesWithInflight：本次加上在途的退款单，恰好把每一行剩下的件数都退完（且确有在途的）。
	// 未发货的单遇到它要拒：两张各自都不是「整单退」，于是运费谁也不退、订单停在 20 还能发货
	// （2026-09-28 破坏性测试：按行分别申请、前一张未审时申请后一张）。
	ClosesWithInflight bool
}

// refundLineAmount 是一行退 k 件的金额，见文件头的两条式子。
func refundLineAmount(it repository.RefundableItem, k int32) int64 {
	net := it.AmountCents - it.DiscountCents
	if it.RefundedQty+k == it.Quantity {
		return net - it.RefundedCents
	}
	// net ≤ 单行金额（上限约 999 件 × 单价），k ≤ 999：乘积远在 int64 之内。
	return net * int64(k) / int64(it.Quantity)
}

// planRefund 校验一次申请并算出每一行的金额。
//
// 错误的顺序就是契约里 409 / 422 各条的优先级：不属于这一单（422
// order-item-mismatch）先于「这一行已在途」（409 refund-already-in-progress），
// 后者又先于「件数超了」（409 refund-quantity-exceeded）——在途的那一行，
// 「还可退几件」本身就没有定论。
func planRefund(items []repository.RefundableItem, inflight map[int64]int32,
	req []RefundLineInput) (refundPlan, error) {

	if len(req) == 0 {
		return refundPlan{}, fmt.Errorf("%w: items 至少一行", ErrRefundBadRequest)
	}
	byID := make(map[int64]repository.RefundableItem, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	asked := make(map[int64]int32, len(req))
	var plan refundPlan
	for _, ln := range req {
		if ln.Quantity < 1 {
			return refundPlan{}, fmt.Errorf("%w: order_item_id %d 的件数 %d 必须 ≥ 1",
				ErrRefundBadRequest, ln.OrderItemID, ln.Quantity)
		}
		if _, dup := asked[ln.OrderItemID]; dup {
			return refundPlan{}, fmt.Errorf("%w: order_item_id %d 出现了两次", ErrRefundBadRequest, ln.OrderItemID)
		}
		it, ok := byID[ln.OrderItemID]
		if !ok {
			return refundPlan{}, fmt.Errorf("%w: order_item_id %d", ErrOrderItemMismatch, ln.OrderItemID)
		}
		if inflight[it.ID] > 0 {
			return refundPlan{}, fmt.Errorf("%w: order_item_id %d 有 %d 件在一张进行中的退款单里",
				ErrRefundAlreadyInProgress, it.ID, inflight[it.ID])
		}
		if left := it.Quantity - it.RefundedQty; ln.Quantity > left {
			return refundPlan{}, fmt.Errorf("%w: order_item_id %d 还可退 %d 件，申请了 %d 件",
				ErrRefundQuantityExceeded, it.ID, left, ln.Quantity)
		}
		asked[it.ID] = ln.Quantity
		amount := refundLineAmount(it, ln.Quantity)
		plan.Lines = append(plan.Lines, repository.NewRefundItem{
			OrderItemID: it.ID, Quantity: ln.Quantity, AmountCents: amount,
		})
		plan.Goods += amount
	}

	plan.CoversEverything = true
	anyInflight, closes := false, true
	for _, it := range items {
		if inflight[it.ID] > 0 {
			anyInflight = true
		}
		if inflight[it.ID] > 0 || it.RefundedQty+asked[it.ID] != it.Quantity {
			plan.CoversEverything = false
		}
		if it.RefundedQty+asked[it.ID]+inflight[it.ID] != it.Quantity {
			closes = false
		}
	}
	plan.ClosesWithInflight = anyInflight && closes
	return plan, nil
}
