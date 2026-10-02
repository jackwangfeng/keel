package service

// 渠道订单的收单（第三期，spec §5.2、§7.1）：回调 → 回读权威状态 → 版本守卫 → 落 channel_orders → 按状态决定动作。
//
//	orderChanged（EventOrderChanged 的处理器）
//	  └─ Caps.OutOfOrderInbound：FetchOrder 回读（回调只是提示）；否则第四期从事件载荷规整（今天也回读）
//	applyChannelOrder（一个事务，channel_orders 行 FOR UPDATE）
//	  ├─ version ≤ 已存：只更新 last_payload（Review Focus 2：旧状态不覆盖新状态、不触发动作）
//	  ├─ 待付款：只记状态（Review Focus 8：AUTHORIZED / PENDING 不接单，之后 orders/paid 带着新版本来）
//	  ├─ 新单 / 已接单，且没有活着的 keel 订单：
//	  │    AcceptRequired 且没配 auto_accept → 写 accept_deadline 等人（第六期的接单接口走 accept）
//	  │    否则同一个事务：映射校验（每行链到 keel SKU、门店有映射，否则标异常不建单，Review Focus 7）→
//	  │    建 keel 草稿（status 0、source 1、user_id 空、金额全用平台快照）→ 写 channel_orders.order_no；
//	  │    提交之后提交 SAGA（channel_order_saga.go，gid 由订单号定，重复提交被协调器去重）
//	  └─ 已取消 / 已发货 / 已完成：applyPlatformFacts（第五期；本期只把状态记进渠道单）
//
// # 为什么并发的三条回调只建一张 keel 订单（Review Focus 1）
//
// orders/create、orders/paid、orders/updated 几乎同时到、被不同 worker 并发处理，三个都回读到同一个版本。
// channel_orders 的（binding, 外部单号）唯一：第一条 INSERT 成功并持有那一行，另两条的 INSERT 撞唯一键、
// 等第一条提交后什么都不做，转去 SELECT … FOR UPDATE 拿到第一条提交的行 —— 版本已经不比它新，只留档。
// 建草稿与写 order_no 在第一条的同一个事务里，所以「有 order_no」与「有草稿」同生同灭；
// 之后再来的事件看到 order_no 就不再建。gid 由订单号定（dtm.OrderGID），同一张草稿重复提交是同一笔事务。
//
// # 渠道单的 keel 订单金额（不变量 5：不重新算价）
//
// goods = 平台的 Goods（Σ 行价 × 数量）、freight = 买家付的运费、discount = promotion_discount = 平台补贴 + 商家补贴
// （没有券）、payable = BuyerPaid（不含税）；接单成功时 paid = payable。税只记在 channel_orders.amounts.tax。
// 平台快照自己对不上（payable ≠ goods + freight − discount）就标异常不建单：建出来也过不了 chk_amount。
// 优惠按行金额比例摊到订单行（discount_cents = promotion_discount_cents，第五期按行退款的上限要它），
// 摊不进行金额的部分记成运费优惠（freight_discount_cents，不超过运费）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// QueueChannelOrderAction 是 keel 对渠道订单的动作（接单 / 拒单 / 发货回传）。worker 在 channel_order_action.go；
// 接单 SAGA 往里放接单 / 拒单，后台发货（EnqueueShipTx）放发货回传。
const QueueChannelOrderAction = "channel.order.action"

// channelActionJob 是 channel.order.action 的载荷。job_key = act:<channel_order_id>:<kind>。
type channelActionJob struct {
	ChannelOrderID int64          `json:"channel_order_id"`
	Action         channel.Action `json:"action"`
}

// channelOrderDraftTTL 是渠道单 keel 草稿的 expire_at：SAGA 卡住（库存服务不在、协调器在重试）时给足时间，
// 孤儿清扫（sweep.go）只在这之后才会关它。自营单是支付时限，渠道单早已付过款，不该 30 分钟就被关掉。
const channelOrderDraftTTL = 24 * time.Hour

// channelSagaWaitMS 是收单 worker 提交 SAGA 后等它到终态的上限。等是为了让同一张单的下一条事件看到
// 一个已经落定的状态（以及让测试确定）；超时不算失败：协调器会推完，渠道单的状态由分支写。
const channelSagaWaitMS = 15_000

var (
	// ErrChannelOrderNotRetryable：重试一张没有异常、或已经有活着的 keel 订单的渠道单（409）。
	ErrChannelOrderNotRetryable = errors.New("这张渠道单没有异常，或已经有 keel 订单，不用重试")
)

// channelOrderAmounts / channelOrderLine / channelOrderReceiver 是 channel_orders 三个 JSONB 列的形状（00320 的注释）。
type channelOrderAmounts struct {
	Goods              int64 `json:"goods"`
	Freight            int64 `json:"freight"`
	PlatformSubsidy    int64 `json:"platform_subsidy"`
	MerchantSubsidy    int64 `json:"merchant_subsidy"`
	Commission         int64 `json:"commission"`
	Tax                int64 `json:"tax"`
	MerchantReceivable int64 `json:"merchant_receivable"`
	BuyerPaid          int64 `json:"buyer_paid"`
	Refunded           int64 `json:"refunded"`
}

type channelOrderLine struct {
	ExternalLineID string `json:"external_line_id"`
	ExternalSKUID  string `json:"external_sku_id"`
	SKUID          *int64 `json:"sku_id"`
	Title          string `json:"title"`
	Qty            int32  `json:"qty"`
	PriceCents     int64  `json:"price_cents"`
	RefundedQty    int32  `json:"refunded_qty"`
}

type channelOrderAddress struct {
	Province string `json:"province"`
	City     string `json:"city"`
	District string `json:"district"`
	Address  string `json:"address"`
	Zip      string `json:"zip"`
	Country  string `json:"country"`
}

type channelOrderReceiver struct {
	Name      string              `json:"name"`
	Phone     string              `json:"phone"`
	PhoneKind int                 `json:"phone_kind"` // 0 真实号码 1 平台隐私号
	Address   channelOrderAddress `json:"address"`
}

// channelOrderSnapshot 把适配器规整好的订单写成 channel_orders 的一行。last_payload 存规整后的整张单
// （含平台原文 Raw）：留档、排障，接单收尾分支从这里读平台下单时间。
func channelOrderSnapshot(bindingID int64, storeID *int64, o channel.ChannelOrder) (repository.ChannelOrderSnapshot, error) {
	a := o.Amounts
	amounts, _ := json.Marshal(channelOrderAmounts{Goods: a.GoodsCents, Freight: a.FreightCents,
		PlatformSubsidy: a.PlatformSubsidyCents, MerchantSubsidy: a.MerchantSubsidyCents, Commission: a.CommissionCents,
		Tax: a.TaxCents, MerchantReceivable: a.MerchantReceivableCents, BuyerPaid: a.BuyerPaidCents, Refunded: a.RefundedCents})
	lines := make([]channelOrderLine, 0, len(o.Lines))
	for _, l := range o.Lines {
		lines = append(lines, channelOrderLine{ExternalLineID: l.ExternalLineID, ExternalSKUID: l.ExternalSKUID,
			Title: l.Title, Qty: l.Qty, PriceCents: l.PriceCents, RefundedQty: l.RefundedQty})
	}
	ls, _ := json.Marshal(lines)
	r := o.Receiver
	kind := 0
	if r.PhoneVirtual {
		kind = 1
	}
	recv, _ := json.Marshal(channelOrderReceiver{Name: r.Name, Phone: r.Phone, PhoneKind: kind,
		Address: channelOrderAddress{Province: r.Province, City: r.City, District: r.District, Address: r.Address,
			Zip: r.Zip, Country: r.Country}})
	payload, err := json.Marshal(o)
	if err != nil {
		return repository.ChannelOrderSnapshot{}, fmt.Errorf("渠道单 %s 编不成 JSON：%w", o.ExternalOrderID, err)
	}
	return repository.ChannelOrderSnapshot{BindingID: bindingID, ExternalOrderID: o.ExternalOrderID,
		ExternalOrderName: o.ExternalOrderName, StoreID: storeID, PlatformStatus: o.PlatformStatus,
		Status: int16(o.Status), AcceptDeadline: o.AcceptDeadline, DeliveryMode: int16(o.Delivery),
		Amounts: amounts, Lines: ls, Receiver: recv, Version: o.Version, LastPayload: payload, Test: o.Test}, nil
}

// orderChanged 是 EventOrderChanged 的处理器。
func (s *ChannelService) orderChanged(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error {
	if b.Roles&repository.ChannelRoleOutlet == 0 {
		return nil
	}
	if ev.ExternalOrderID == "" {
		s.log.WarnContext(ctx, "订单回调没带订单号，丢弃", "binding_id", b.ID, "topic", ev.Topic)
		return nil
	}
	o, err := s.fetchChannelOrder(ctx, b, ev.ExternalOrderID)
	if err != nil || o == nil {
		return err
	}
	return s.applyChannelOrder(ctx, b, *o)
}

// fetchChannelOrder 回读一张订单的权威状态。适配器不是销售渠道时返回 nil（记一笔、不重试）。
//
// Caps.OutOfOrderInbound 的渠道（Shopify）回调只是提示，必须回读；其余渠道第四期改为从事件载荷规整，
// 今天一律回读（权威状态总是对的，只是多一次调用）。
func (s *ChannelService) fetchChannelOrder(ctx context.Context, b repository.ChannelBinding, externalID string) (*channel.ChannelOrder, error) {
	_, ab, err := s.loadBinding(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	a, _ := s.reg.Lookup(b.Channel)
	out, ok := a.(channel.Outlet)
	if !ok {
		s.log.WarnContext(ctx, "订单回调来了，但这个适配器不是销售渠道", "binding_id", b.ID, "channel", b.Channel)
		return nil, nil
	}
	o, err := out.FetchOrder(ctx, ab, externalID)
	if errors.Is(err, channel.ErrCredentials) {
		s.markCredentialsBroken(ctx, b.ID, channel.RedactError(err, ab.Secrets))
	}
	if err != nil {
		return nil, channel.RedactError(err, ab.Secrets)
	}
	return &o, nil
}

// applyOpts：force（后台「重试」）不受版本守卫、无视已有异常、也当作已接单；accept（第六期的人工接单）只跳过「等人接单」。
type applyOpts struct{ force, accept bool }

// applyChannelOrder 是版本守卫 + 状态转动作（文件头）。
func (s *ChannelService) applyChannelOrder(ctx context.Context, b repository.ChannelBinding, o channel.ChannelOrder) error {
	return s.applyChannelOrderOpts(ctx, b, o, applyOpts{})
}

func (s *ChannelService) applyChannelOrderOpts(ctx context.Context, b repository.ChannelBinding, o channel.ChannelOrder, opt applyOpts) error {
	if o.ExternalOrderID == "" {
		return fmt.Errorf("适配器回读的订单没有外部单号")
	}
	a, ok := s.reg.Lookup(b.Channel)
	if !ok {
		return fmt.Errorf("%w：%q", ErrChannelUnknownKind, b.Channel)
	}
	caps := a.Caps()
	autoAccept := parseBindingConfig(b.Config).AutoAccept
	var saga *channelSaga
	var kicks []string
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		saga, kicks = nil, nil
		storeID, err := s.mappedStore(ctx, tx, b.ID, o.ExternalStoreID)
		if err != nil {
			return err
		}
		snap, err := channelOrderSnapshot(b.ID, storeID, o)
		if err != nil {
			return err
		}
		co, inserted, err := tx.InsertOrLockChannelOrder(ctx, snap)
		if err != nil {
			return err
		}
		if !inserted {
			if !opt.force && o.Version <= co.Version {
				return tx.TouchChannelOrderPayload(ctx, co.ID, snap.LastPayload)
			}
			// keel 这一侧走过的「已接单」不被平台的「新单」盖回去（Shopify 没有接单这一步，平台上永远是新单）。
			if snap.Status == repository.ChannelOrderNew && co.Status == repository.ChannelOrderAccepted {
				snap.Status = repository.ChannelOrderAccepted
			}
			if err := tx.UpdateChannelOrderSnapshot(ctx, co.ID, snap); err != nil {
				return err
			}
		}
		co.Status, co.StoreID, co.AcceptDeadline = snap.Status, snap.StoreID, snap.AcceptDeadline
		live, err := liveChannelKeelOrder(ctx, tx, co)
		if err != nil {
			return err
		}
		switch co.Status {
		case repository.ChannelOrderPendingPayment:
			return nil
		case repository.ChannelOrderNew, repository.ChannelOrderAccepted:
			if live != nil {
				if live.Status == orderStatusDraft {
					// 草稿还在 0：上次提交 SAGA 失败或进程死在提交之前。再提交一次（同一个 gid，协调器去重）。
					saga, err = resumeChannelSaga(ctx, tx, *live)
					return err
				}
				// 已经成单：平台上的部分退款（Shopify 部分退款后仍是新单）照样要记。
				kicks, err = s.applyPlatformFacts(ctx, tx, b, co, o, live)
				return err
			}
			if co.Exception != nil && !opt.force {
				return nil // 有异常等人处理（后台「重试」走 force）：不在每条回调上反复建单、反复缺货
			}
			if caps.AcceptRequired && !autoAccept && !opt.accept && !opt.force {
				return nil // 等人接单；接单截止已随快照写进 accept_deadline
			}
			saga, err = s.openChannelOrderTx(ctx, tx, b, co, o)
			return err
		default:
			kicks, err = s.applyPlatformFacts(ctx, tx, b, co, o, live)
			return err
		}
	})
	if err != nil {
		return err
	}
	// 退款回补：提交之后就地跑一次 outbox 任务，库存服务不在时留给 worker（同 RefundService 的 kickRestock）。
	for _, k := range kicks {
		s.ob.kick(ctx, k)
	}
	if saga == nil {
		return nil
	}
	return s.submitChannelSaga(ctx, *saga)
}

// applyPlatformFacts 把平台上的取消 / 退款 / 发货转成 keel 订单上的动作（applyChannelOrderOpts 的事务里，渠道单行已锁）。
// 返回提交之后要就地跑的库存回补任务键（restock:<退款单号>）。
//
//	没有活着的 keel 订单：只记渠道单状态（已经写了快照）。
//	keel 订单还在 0 / 10（接单 SAGA 在途）：同上 —— 收尾分支锁渠道单看到已取消就判失败，协调器补偿（库存按流水放回、关到 90）。
//	已取消：keel 20 → 整单退款（20 → 50 → 60，退款单直接成功、不经支付渠道，回补库存）；keel 30 / 40 → 不动，渠道单标异常。
//	其余：平台上新出现的退款（按平台退款 ID 幂等）记成功退款单与行退款数，未发货且平台放回了库存的回补；
//	      平台上发了货（整单）而 keel 还在 20 → keel 发货 20 → 30，承运商与单号用平台的，不入队回传（不回声）。
//
// 幂等：退款单的 channel_refund_id = channel_refund:<binding>:<平台退款 ID>（整单取消 …:<外部单号>:cancel），
// uk_refunds_channel_txn 唯一；重放同一事件（或平台上的新版本带着同样的退款）先查它，记过就跳过（Review Focus 5）。
func (s *ChannelService) applyPlatformFacts(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, o channel.ChannelOrder, live *repository.Order) ([]string, error) {
	if live == nil || live.Status == orderStatusDraft || live.Status == orderStatusPending {
		return nil, nil
	}
	order, err := tx.LockOrderByID(ctx, live.ID)
	if err != nil {
		return nil, err
	}
	switch co.Status {
	case repository.ChannelOrderCancelled:
		return s.platformCancelled(ctx, tx, b, co, order, o)
	case repository.ChannelOrderRejected, repository.ChannelOrderPendingPayment:
		return nil, nil
	}
	var kicks []string
	for _, rf := range o.Refunds {
		k, err := s.platformRefund(ctx, tx, b, &order, rf)
		if err != nil {
			return nil, err
		}
		if k != "" {
			kicks = append(kicks, k)
		}
	}
	if (co.Status == repository.ChannelOrderShipped || co.Status == repository.ChannelOrderCompleted) &&
		order.Status == orderStatusPaid && len(o.Shipments) > 0 {
		if err := s.platformShipped(ctx, tx, b, order, o); err != nil {
			return nil, err
		}
	}
	return kicks, nil
}

const channelCancelAfterShip = "平台在 keel 发货后取消了订单"

// platformCancelled：平台取消了订单。
func (s *ChannelService) platformCancelled(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, order repository.Order, o channel.ChannelOrder) ([]string, error) {
	switch order.Status {
	case orderStatusPaid:
		k, err := s.refundWholeChannelOrder(ctx, tx, b, co, order, o)
		if err != nil || k == "" {
			return nil, err
		}
		return []string{k}, nil
	case orderStatusShipped, orderStatusFinished:
		if co.Exception != nil && *co.Exception == channelCancelAfterShip {
			return nil, nil // 重放
		}
		reason := channelCancelAfterShip
		s.log.WarnContext(ctx, "渠道单在 keel 发货后被平台取消，标异常", "channel_order_id", co.ID, "order_no", order.OrderNo)
		if err := tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: co.Status, OrderNo: co.OrderNo,
			Exception: &reason, AcceptDeadline: co.AcceptDeadline}); err != nil {
			return nil, err
		}
		return nil, notifyChannelOrderAttention(ctx, tx, order, "cancel", reason,
			"keel 订单 "+order.OrderNo+" 的货已经发出，请联系顾客处理退货。")
	default:
		return nil, nil // 50 / 60：已经整单退过（重放）
	}
}

// refundWholeChannelOrder：keel 订单 20 → 50 → 60，记一张成功的退款单（金额 = 还没退的实收，每行退掉剩下的件数），
// 回补库存（返回任务键）。不调支付渠道：钱是平台退的。
func (s *ChannelService) refundWholeChannelOrder(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, order repository.Order, o channel.ChannelOrder) (string, error) {
	key := fmt.Sprintf("channel_refund:%d:%s:cancel", b.ID, co.ExternalOrderID)
	if done, err := tx.ChannelRefundExists(ctx, key); err != nil || done {
		return "", err
	}
	// 平台取消时自己放回了库存（取消带的退款行）：推送基线跟着加，理由同下面 platformRefund。
	// 单笔记过的部分退款（它们的基线当时已经调过）不再算。
	for _, rf := range o.Refunds {
		if !rf.Restock || rf.ExternalID == "" {
			continue
		}
		if done, err := tx.ChannelRefundExists(ctx, fmt.Sprintf("channel_refund:%d:%s", b.ID, rf.ExternalID)); err != nil {
			return "", err
		} else if !done {
			if err := s.raiseBaselineForPlatformRestock(ctx, tx, b.ID, order.StoreID, rf.Lines); err != nil {
				return "", err
			}
		}
	}
	ok, err := tx.StartWholeOrderRefund(ctx, order.ID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("渠道单 %d 的 keel 订单 %s 在行锁之下从 20 推 50 失败", co.ID, order.OrderNo)
	}
	items, err := tx.ListRefundableItems(ctx, order.ID)
	if err != nil {
		return "", err
	}
	var lines []repository.NewRefundItem
	var goods int64
	for _, it := range items {
		qty := it.Quantity - it.RefundedQty
		if qty <= 0 {
			continue
		}
		amt := it.AmountCents - it.DiscountCents - it.RefundedCents
		lines = append(lines, repository.NewRefundItem{OrderItemID: it.ID, Quantity: qty, AmountCents: amt})
		goods += amt
	}
	total := order.PaidCents - order.RefundedCents
	goods = min(goods, total)
	refundNo := ""
	if total > 0 {
		if refundNo, err = newRefundNo(time.Now()); err != nil {
			return "", err
		}
		if _, err := tx.InsertChannelRefund(ctx, repository.NewChannelRefund{RefundNo: refundNo, OrderID: order.ID,
			ReasonText: "平台取消了订单（" + b.Channel + " " + co.ExternalOrderName + "）", GoodsAmountCents: goods,
			FreightCents: total - goods, ChannelRefundID: key, RefundedAt: time.Now(), Items: lines}); err != nil {
			return "", err
		}
		for _, l := range lines {
			if err := tx.WriteBackOrderItemRefund(ctx, l.OrderItemID, l.Quantity, l.AmountCents); err != nil {
				return "", err
			}
		}
		if err := tx.AddChannelRefundedCents(ctx, order.ID, total); err != nil {
			return "", err
		}
	}
	if ok, err := tx.FinishWholeOrderRefund(ctx, order.ID); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("渠道单 %d 的 keel 订单 %s 在行锁之下从 50 推 60 失败", co.ID, order.OrderNo)
	}
	if err := tx.RecomputeOrderRefundStatus(ctx, order.ID); err != nil {
		return "", err
	}
	hint := fmt.Sprintf("keel 订单 %s 已整单退款（%s，钱由平台退给顾客），不用发货。", order.OrderNo, yuanText(total))
	kick := ""
	if refundNo != "" && len(lines) > 0 {
		// 没发过货（20）：货还在门店，按退款单回补（与自营退款同一个 outbox 任务）。
		if err := enqueueRefundRestock(ctx, tx, refundNo); err != nil {
			return "", err
		}
		kick = restockJobKey(refundNo)
		hint = fmt.Sprintf("keel 订单 %s 已整单退款（%s，钱由平台退给顾客）并回补库存，不用发货。", order.OrderNo, yuanText(total))
	}
	return kick, notifyChannelOrderAttention(ctx, tx, order, "cancel", "平台取消了渠道订单 "+co.ExternalOrderName, hint)
}

// platformRefund：平台上的一笔退款（全部列出，按 ExternalID 幂等）。order 随之更新已退金额。
// 金额：有退款行时按 keel 订单行的实付（行金额 − 行优惠）× 件数 / 下单件数，平台金额更少时（扣了手续费）按平台的、
// 更多时（含税，税不进 keel 订单）按行算的；没有退款行（只退运费之类）记成运费退款。都不超过还没退的实收。
func (s *ChannelService) platformRefund(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	order *repository.Order, rf channel.Refund) (string, error) {
	if rf.ExternalID == "" || order.Status == orderStatusRefunding || order.Status == orderStatusRefunded {
		return "", nil
	}
	key := fmt.Sprintf("channel_refund:%d:%s", b.ID, rf.ExternalID)
	if done, err := tx.ChannelRefundExists(ctx, key); err != nil || done {
		return "", err
	}
	items, err := tx.ListRefundableItems(ctx, order.ID)
	if err != nil {
		return "", err
	}
	bySKU := map[int64]*repository.RefundableItem{}
	for i := range items {
		bySKU[items[i].SKUID] = &items[i]
	}
	var lines []repository.NewRefundItem
	var goods int64
	for _, l := range rf.Lines {
		if l.Qty <= 0 {
			continue
		}
		link, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemSKU, l.ExternalSKUID)
		if errors.Is(err, repository.ErrChannelNotFound) {
			s.log.WarnContext(ctx, "平台退款里有一行对不上 keel 的 SKU，这一行不记件数", "order_no", order.OrderNo,
				"external_sku_id", l.ExternalSKUID)
			continue
		}
		if err != nil {
			return "", err
		}
		it := bySKU[link.KeelID]
		if it == nil {
			continue
		}
		qty := min(l.Qty, it.Quantity-it.RefundedQty)
		if qty <= 0 {
			continue
		}
		net := it.AmountCents - it.DiscountCents
		amt := min(net*int64(qty)/int64(it.Quantity), net-it.RefundedCents)
		if qty == it.Quantity-it.RefundedQty {
			amt = net - it.RefundedCents // 最后几件把零头带走
		}
		it.RefundedQty += qty
		it.RefundedCents += amt
		lines = append(lines, repository.NewRefundItem{OrderItemID: it.ID, Quantity: qty, AmountCents: amt})
		goods += amt
	}
	remain := order.PaidCents - order.RefundedCents
	total := min(rf.AmountCents, remain)
	if len(lines) > 0 {
		total = min(total, goods)
		for i := len(lines) - 1; i >= 0 && goods > total; i-- {
			d := min(goods-total, lines[i].AmountCents)
			lines[i].AmountCents -= d
			goods -= d
		}
	} else {
		goods = 0
	}
	if total <= 0 {
		return "", nil
	}
	refundNo, err := newRefundNo(time.Now())
	if err != nil {
		return "", err
	}
	at := rf.At
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := tx.InsertChannelRefund(ctx, repository.NewChannelRefund{RefundNo: refundNo, OrderID: order.ID,
		ReasonText: "平台上的退款（" + b.Channel + " " + rf.ExternalID + "）", GoodsAmountCents: goods,
		FreightCents: total - goods, ChannelRefundID: key, RefundedAt: at, Items: lines}); err != nil {
		return "", err
	}
	for _, l := range lines {
		if err := tx.WriteBackOrderItemRefund(ctx, l.OrderItemID, l.Quantity, l.AmountCents); err != nil {
			return "", err
		}
	}
	if err := tx.AddChannelRefundedCents(ctx, order.ID, total); err != nil {
		return "", err
	}
	order.RefundedCents += total
	if err := tx.RecomputeOrderRefundStatus(ctx, order.ID); err != nil {
		return "", err
	}
	if rf.Restock {
		if err := s.raiseBaselineForPlatformRestock(ctx, tx, b.ID, order.StoreID, rf.Lines); err != nil {
			return "", err
		}
	}
	kick := ""
	hint := fmt.Sprintf("keel 订单 %s 已记一笔退款 %s（钱由平台退给顾客）。", order.OrderNo, yuanText(total))
	if rf.Restock && order.ShippedAt == nil && len(lines) > 0 {
		if err := enqueueRefundRestock(ctx, tx, refundNo); err != nil {
			return "", err
		}
		kick = restockJobKey(refundNo)
		hint = fmt.Sprintf("keel 订单 %s 已记一笔退款 %s（钱由平台退给顾客），退掉的件数已回补库存，发货时少发这几件。",
			order.OrderNo, yuanText(total))
	}
	return kick, notifyChannelOrderAttention(ctx, tx, *order, "refund:"+rf.ExternalID, "渠道订单在平台上退了款", hint)
}

// raiseBaselineForPlatformRestock：平台退款时自己把货放回了平台上的库存（Shopify restockType CANCEL / RETURN），
// 平台上的数就比上次推出去的多了这几件。推送基线跟着加（Review Focus 3 的反方向，DecrementChannelListingBaseline
// 传负数），keel 回补之后的那次推送才是一次正常的 CAS，不会每笔退款撞一次冲突、记一条差异。
func (s *ChannelService) raiseBaselineForPlatformRestock(ctx context.Context, tx repository.Tx, bindingID, storeID int64,
	lines []channel.ActionLine) error {
	for _, l := range lines {
		if l.Qty <= 0 {
			continue
		}
		link, err := tx.ChannelItemLinkByExternal(ctx, bindingID, repository.ChannelItemSKU, l.ExternalSKUID)
		if errors.Is(err, repository.ErrChannelNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := tx.DecrementChannelListingBaseline(ctx, bindingID, storeID, link.KeelID, -l.Qty); err != nil {
			return err
		}
	}
	return nil
}

// platformShipped：平台上整单发了货、keel 还在 20 → keel 发货 20 → 30（承运商 / 单号用平台的，created_by 空），
// 不入队回传（那就是回声）。买家通知不发：渠道单无 keel 买家，平台自己通知顾客。
func (s *ChannelService) platformShipped(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	order repository.Order, o channel.ChannelOrder) error {
	sh := o.Shipments[0]
	for _, x := range o.Shipments {
		if x.TrackingNo != "" {
			sh = x
			break
		}
	}
	carrier, tracking := strings.TrimSpace(sh.Company), strings.TrimSpace(sh.TrackingNo)
	if carrier == "" {
		carrier = b.Channel
	}
	if tracking == "" {
		tracking = o.ExternalOrderID // 平台上发货没填单号：用平台单号占位（shipments 不收空串）
	}
	ok, err := tx.ShipOrder(ctx, order.ID)
	if errors.Is(err, repository.ErrIllegalOrderTransition) {
		ok, err = false, nil
	}
	if err != nil || !ok {
		return err
	}
	if _, err := tx.InsertShipment(ctx, repository.NewShipment{OrderID: order.ID, CarrierCode: truncateRunes(carrier, 64),
		TrackingNo: truncateRunes(tracking, 64)}); err != nil {
		return fmt.Errorf("渠道单 %s 在平台上发了货，keel 记发货失败: %w", o.ExternalOrderID, err)
	}
	return nil
}

// yuanText 把分写成「¥12.30」（通知正文里的金额，与模板的 yuan 同一种写法）。
func yuanText(cents int64) string {
	return fmt.Sprintf("¥%d.%02d", cents/100, cents%100)
}

// RetryChannelOrder 是后台「重试」：只对有异常、且没有活着（非 90）的 keel 订单的渠道单。重新回读平台、强制重走接单
// （不受版本守卫；成单时清掉异常，又失败时换成新的原因）。
func (s *ChannelService) RetryChannelOrder(ctx context.Context, id int64) error {
	var b repository.ChannelBinding
	var externalID string
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err := tx.GetChannelOrder(ctx, id)
		if err != nil {
			return err
		}
		live, err := liveChannelKeelOrder(ctx, tx, co)
		if err != nil {
			return err
		}
		if co.Exception == nil || live != nil {
			return ErrChannelOrderNotRetryable
		}
		externalID = co.ExternalOrderID
		b, err = tx.GetChannelBinding(ctx, co.BindingID)
		return err
	})
	if err != nil {
		return err
	}
	o, err := s.fetchChannelOrder(ctx, b, externalID)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("binding %d 的适配器不是销售渠道，回读不了订单", b.ID)
	}
	return s.applyChannelOrderOpts(ctx, b, *o, applyOpts{force: true, accept: true})
}

// liveChannelKeelOrder 是渠道单当前关联的、还活着（不是 90）的 keel 订单；没有返回 nil。
func liveChannelKeelOrder(ctx context.Context, tx repository.Tx, co repository.ChannelOrder) (*repository.Order, error) {
	if co.OrderNo == nil {
		return nil, nil
	}
	o, err := tx.FindOrderByNo(ctx, *co.OrderNo)
	if err != nil {
		return nil, err
	}
	if o.Status == orderStatusClosed {
		return nil, nil
	}
	return &o, nil
}

// mappedStore：渠道门店 → keel 门店；没有映射（或订单没落到单一门店）返回 nil。
func (s *ChannelService) mappedStore(ctx context.Context, tx repository.Tx, bindingID int64, externalStoreID string) (*int64, error) {
	if externalStoreID == "" {
		return nil, nil
	}
	links, err := tx.ListChannelStoreLinks(ctx, bindingID)
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if l.ExternalStoreID == externalStoreID {
			id := l.StoreID
			return &id, nil
		}
	}
	return nil, nil
}

// channelSaga 是提交之后要跑的接单 SAGA。
type channelSaga struct {
	orderNo string
	storeID int64
	lines   []inventory.OrderLine
}

// markChannelOrderException 标异常、不建单（原因写清是哪一行 / 哪个门店）。
func (s *ChannelService) markChannelOrderException(ctx context.Context, tx repository.Tx, co repository.ChannelOrder, reason string) error {
	s.log.WarnContext(ctx, "渠道单没能建 keel 订单，标异常", "channel_order_id", co.ID,
		"external_order_id", co.ExternalOrderID, "reason", reason)
	return tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: co.Status, OrderNo: nil,
		Exception: &reason, AcceptDeadline: co.AcceptDeadline})
}

// openChannelOrderTx 在 applyChannelOrderOpts 的事务里：映射校验 → 建 keel 草稿 + 订单行 → 写 order_no、清异常。
// 映射不全返回 (nil, nil) 并标异常。
func (s *ChannelService) openChannelOrderTx(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, o channel.ChannelOrder) (*channelSaga, error) {
	fail := func(format string, args ...any) (*channelSaga, error) {
		return nil, s.markChannelOrderException(ctx, tx, co, fmt.Sprintf(format, args...))
	}
	switch {
	case o.StoreError != "":
		return fail("门店：%s", o.StoreError)
	case o.ExternalStoreID == "":
		return fail("门店：平台没给出这张单的发货门店")
	case co.StoreID == nil:
		return fail("门店：平台门店 %s 没有映射到 keel 门店", o.ExternalStoreID)
	}
	type keelLine struct {
		sku   int64
		title string
		qty   int32
		price int64
	}
	var lines []keelLine
	for i, l := range o.Lines {
		if l.Qty <= 0 {
			continue
		}
		link, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemSKU, l.ExternalSKUID)
		if errors.Is(err, repository.ErrChannelNotFound) || (err == nil && l.ExternalSKUID == "") {
			return fail("第 %d 行「%s」（渠道 SKU %s）没有链到 keel 的 SKU", i+1, l.Title, l.ExternalSKUID)
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, keelLine{sku: link.KeelID, title: l.Title, qty: l.Qty, price: l.PriceCents})
	}
	if len(lines) == 0 {
		return fail("订单里没有商品行")
	}
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.sku)
	}
	skus, err := tx.ChannelOrderSKUs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, l := range lines {
		if _, ok := skus[l.sku]; !ok {
			return fail("第 %d 行「%s」链到的 keel SKU %d 已经删除", i+1, l.title, l.sku)
		}
	}

	am := o.Amounts
	discount := am.PlatformSubsidyCents + am.MerchantSubsidyCents
	var goods int64
	for _, l := range lines {
		goods += l.price * int64(l.qty)
	}
	switch {
	case goods != am.GoodsCents:
		return fail("金额：商品行合计 %d 分，平台给的商品金额 %d 分", goods, am.GoodsCents)
	case am.BuyerPaidCents != am.GoodsCents+am.FreightCents-discount || discount < 0 || am.FreightCents < 0:
		return fail("金额：平台快照对不上（商品 %d + 运费 %d − 优惠 %d ≠ 实付 %d，单位分）",
			am.GoodsCents, am.FreightCents, discount, am.BuyerPaidCents)
	}
	// 优惠按行金额比例摊（最大余数），摊不进的记运费优惠。
	shares := make([]int64, len(lines))
	lineDiscount := min(discount, goods)
	if goods > 0 {
		var given int64
		for i, l := range lines {
			shares[i] = lineDiscount * l.price * int64(l.qty) / goods
			given += shares[i]
		}
		for i := 0; given < lineDiscount; i = (i + 1) % len(lines) {
			if shares[i] < lines[i].price*int64(lines[i].qty) {
				shares[i]++
				given++
			}
		}
	}
	freightDiscount := discount - lineDiscount
	if freightDiscount > am.FreightCents {
		return fail("金额：优惠 %d 分超过了商品与运费的合计", discount)
	}

	now := time.Now()
	orderNo, err := newOrderNo(now)
	if err != nil {
		return nil, err
	}
	r := o.Receiver
	recv := receiverSnapshot{ReceiverName: r.Name, Phone: r.Phone, Province: r.Province, City: r.City,
		District: r.District, Street: r.Address, Detail: r.Country}
	if r.Zip != "" {
		zip := r.Zip
		recv.PostalCode = &zip
	}
	recvJSON, _ := json.Marshal(recv)
	remark := fmt.Sprintf("%s %s", b.Channel, o.ExternalOrderName)
	draft, err := tx.CreateChannelOrderDraft(ctx, repository.NewChannelOrderDraft{OrderNo: orderNo, ChannelOrderID: co.ID,
		StoreID: *co.StoreID, GoodsAmountCents: am.GoodsCents, FreightCents: am.FreightCents,
		FreightDiscountCents: freightDiscount, DiscountCents: discount, PayableCents: am.BuyerPaidCents,
		ReceiverSnapshot: recvJSON, Remark: &remark, ExpireAt: now.Add(channelOrderDraftTTL)})
	if errors.Is(err, repository.ErrCatalogBadReference) {
		return fail("门店：keel 门店 %d 不存在或已删除", *co.StoreID)
	}
	if err != nil {
		return nil, err
	}
	deduct := map[int64]int32{}
	for i, l := range lines {
		sku := skus[l.sku]
		amount := l.price * int64(l.qty)
		if err := tx.CreateOrderItem(ctx, repository.NewOrderItem{OrderID: draft.ID, SKUID: l.sku, ProductID: sku.ProductID,
			TitleSnapshot: sku.Title, SpecSnapshot: specOrEmpty(sku.SpecValues), ImageSnapshot: sku.ImageURL,
			PriceCents: l.price, Quantity: l.qty, AmountCents: amount, DiscountCents: shares[i],
			ListPriceCents: l.price, PromotionDiscountCents: shares[i]}); err != nil {
			return nil, err
		}
		deduct[l.sku] += l.qty
	}
	if err := tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: co.Status, OrderNo: &orderNo,
		Exception: nil, AcceptDeadline: co.AcceptDeadline}); err != nil {
		return nil, err
	}
	saga := &channelSaga{orderNo: orderNo, storeID: *co.StoreID}
	for sku, qty := range deduct {
		saga.lines = append(saga.lines, inventory.OrderLine{SKUID: sku, Qty: qty})
	}
	sort.Slice(saga.lines, func(i, j int) bool { return saga.lines[i].SKUID < saga.lines[j].SKUID })
	return saga, nil
}

func specOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}

// resumeChannelSaga 从草稿重建 SAGA（扣减行从 order_items 读）。
func resumeChannelSaga(ctx context.Context, tx repository.Tx, order repository.Order) (*channelSaga, error) {
	lines, err := tx.ListOrderLines(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	saga := &channelSaga{orderNo: order.OrderNo, storeID: order.StoreID}
	for _, l := range lines {
		saga.lines = append(saga.lines, inventory.OrderLine{SKUID: l.SKUID, Qty: l.Quantity})
	}
	return saga, nil
}

// submitChannelSaga 提交接单 SAGA 并等它落定（有界）。提交失败返回错误：回调任务退避重试，
// 重试时看到草稿还在 0 会再提交（resumeChannelSaga）。
func (s *ChannelService) submitChannelSaga(ctx context.Context, saga channelSaga) error {
	tc := s.coord()
	if tc == nil {
		return errors.New("协调器还没接上，接单 SAGA 提交不了")
	}
	gid, err := orderGID(ctx, saga.orderNo)
	if err != nil {
		return err
	}
	steps, err := s.channelSagaSteps(saga.orderNo, saga.storeID, saga.lines)
	if err != nil {
		return err
	}
	if err := tc.SubmitSaga(gid, steps); err != nil {
		return fmt.Errorf("提交接单 SAGA 失败（gid=%s）: %w", gid, err)
	}
	st, err := tc.WaitFinal(gid, channelSagaWaitMS)
	if err != nil || (st != "succeed" && st != "failed") {
		s.log.WarnContext(ctx, "接单 SAGA 还没落定，协调器会接着推", "gid", gid, "status", st, "err", err)
	}
	return nil
}

// enqueueChannelAction 入队一个对渠道订单的动作（同一张单同一种动作只排一次）。
func enqueueChannelAction(ctx context.Context, tx repository.Tx, channelOrderID int64, a channel.Action) error {
	if a.IdemKey == "" {
		a.IdemKey = "act:" + strconv.FormatInt(channelOrderID, 10) + ":" + string(a.Kind)
	}
	payload, _ := json.Marshal(channelActionJob{ChannelOrderID: channelOrderID, Action: a})
	_, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelOrderAction,
		JobKey: fmt.Sprintf("act:%d:%s", channelOrderID, a.Kind), Payload: payload, MaxAttempts: channelPushMaxAttempts})
	return err
}

// stockoutReason 把库存服务的拒绝流水写成给人看的原因：「缺货：连衣裙 要 3 件（可售 1）」。
func stockoutReason(trail []inventory.TrailEntry, want map[int64]int32, titles map[int64]string) string {
	var parts []string
	for _, e := range trail {
		if e.BizType != inventory.BizOrderRejected {
			continue
		}
		switch e.Reason {
		case inventory.RejectInsufficient:
			title := titles[e.SKUID]
			if title == "" {
				title = "SKU " + strconv.FormatInt(e.SKUID, 10)
			}
			parts = append(parts, fmt.Sprintf("%s 要 %d 件（可售 %d）", title, want[e.SKUID], e.Before))
		default:
			parts = append(parts, fmt.Sprintf("SKU %d 扣不了库存（%s）", e.SKUID, e.Reason))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "缺货：" + strings.Join(parts, "；")
}
