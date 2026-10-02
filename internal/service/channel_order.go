package service

// 渠道订单的收单（第三期，spec §5.2、§7.1）：回调 → 回读权威状态 → 版本守卫 → 落 channel_orders → 按状态决定动作。
//
//	orderChanged（EventOrderChanged 的处理器）
//	  └─ Caps.OutOfOrderInbound：FetchOrder 回读（回调只是提示）；否则第四期从事件载荷规整（今天也回读）
//	applyChannelOrder（一个事务，channel_orders 行 FOR UPDATE）
//	  ├─ version < 已存（或同版本而状态反倒更靠前）：只更新 last_payload（Review Focus 2：旧状态不覆盖新状态、不触发动作）；
//	  │    同版本照样往下走（Shopify updatedAt 只到秒；上次没做完的靠重放补上），往下的每一步都幂等
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
// 等第一条提交后什么都不做，转去 SELECT … FOR UPDATE 拿到第一条提交的行 —— 同一个版本照样往下走，但看到了
// order_no：草稿还在 0 就再提交一次 SAGA（gid 由订单号定，dtm.OrderGID；协调器对同一个 gid 的重复提交去重，
// 在途、已终结都一样，分支只跑一次），已经成单就只对平台事实（退款按平台退款 ID 幂等）。
// 建草稿与写 order_no 在第一条的同一个事务里，所以「有 order_no」与「有草稿」同生同灭；之后再来的事件看到 order_no 就不再建。
//
// # 同版本重放为什么不重复做事
//
// 同一个版本会被处理多次（并发的回调、SAGA 提交失败后的退避重试、Shopify 同一秒里的两次变化），每一步各自幂等：
// 建单看 order_no（有活着的 keel 订单就不建；有异常不建，等后台「重试」）；SAGA 按 gid 去重；收尾分支只在 10 时
// 推 20、发「新订单」通知；退款单按 channel_refund_id 唯一（整单取消 …:cancel）、先查后记，通知随退款单一起；
// 发货后取消的异常按原因去重；平台发货只在 keel 还是 20 时推 30；收单这条路径本身不入队对平台的动作（接单 / 拒单在 SAGA 分支里，分支只跑一次）。
//
// # 渠道单的 keel 订单金额（不变量 5：不重新算价）
//
// goods = 平台的 Goods（Σ 行价 × 数量）、freight = 买家付的运费、discount = promotion_discount = 平台补贴 + 商家补贴
// （没有券）、payable = BuyerPaid；接单成功时 paid = payable。价外税的店 BuyerPaid 不含税、税不进 keel 订单；
// 价内税的店（amounts.taxes_included）行价已经含税，BuyerPaid 就是顾客付的总价。税额都记在 channel_orders.amounts.tax。
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
	ErrChannelOrderNotRetryable = errors.New("这张渠道单没有异常、也没有卡住的 keel 订单，不用重试")
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
	// TaxesIncluded：价内税（行价已经含税，keel 实付 = 顾客付的总价）；假 = 价外税，税不进 keel 订单。见 channel.OrderAmounts。
	TaxesIncluded bool `json:"taxes_included"`
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
		Tax: a.TaxCents, MerchantReceivable: a.MerchantReceivableCents, BuyerPaid: a.BuyerPaidCents, Refunded: a.RefundedCents,
		TaxesIncluded: a.TaxesIncluded})
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
			// 版本守卫：只有严格更旧的版本只留档。同版本照样往下走 —— 上次在这个版本上没做完的（SAGA 提交失败、
			// 草稿还在 0）要靠重放补上；Shopify 的 updatedAt 只到秒，同一秒里的两次变化也是同一个版本。
			// 往下走的每一步都幂等（见 sameVersionStale 与文件头）。
			if !opt.force && (o.Version < co.Version || (o.Version == co.Version && sameVersionStale(co.Status, snap.Status))) {
				return tx.TouchChannelOrderPayload(ctx, co.ID, snap.LastPayload)
			}
			// keel 这一侧走过的「已接单」「已拒单」不被平台的「新单」盖回去（Shopify 没有接单这一步，平台上永远是新单；
			// 拒单回传之前平台上也还是新单）。
			if snap.Status == repository.ChannelOrderNew &&
				(co.Status == repository.ChannelOrderAccepted || co.Status == repository.ChannelOrderRejected) {
				snap.Status = co.Status
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

// channelStatusRank 是平台状态的先后：待付款 → 新单 / 已接单 → 已发货 → 已完成；取消 / 拒单是终态。
func channelStatusRank(st int16) int {
	switch st {
	case repository.ChannelOrderPendingPayment:
		return 1
	case repository.ChannelOrderNew, repository.ChannelOrderAccepted:
		return 2
	case repository.ChannelOrderShipped:
		return 3
	case repository.ChannelOrderCompleted:
		return 4
	default: // 已取消、已拒单
		return 5
	}
}

// sameVersionStale：同一个版本的两次回读，后到的那次状态反倒更靠前 —— 两个 worker 在同一秒的两次变化之间各回读了一次，
// 先回读的后提交。它不比已存的新，只留档（否则同一秒里先取消、后处理到「新单」的快照会建出 keel 订单，Review Focus 2）。
func sameVersionStale(stored, fetched int16) bool {
	return channelStatusRank(fetched) < channelStatusRank(stored)
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
	basis, err := keelBasisOf(ctx, tx, co.ID, order.OrderNo)
	if err != nil {
		return nil, err
	}
	switch co.Status {
	case repository.ChannelOrderCancelled:
		return s.platformCancelled(ctx, tx, b, co, order, o, basis)
	case repository.ChannelOrderRejected, repository.ChannelOrderPendingPayment:
		return nil, nil
	}
	var kicks []string
	for _, rf := range o.Refunds {
		if basis.absorbed(rf.ExternalID) {
			continue // 建 keel 订单时已经按剩余件数吸收了
		}
		k, err := s.platformRefund(ctx, tx, b, &order, rf, o.Amounts, basis)
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
	co repository.ChannelOrder, order repository.Order, o channel.ChannelOrder, basis *channelKeelBasis) ([]string, error) {
	switch order.Status {
	case orderStatusPaid:
		k, err := s.refundWholeChannelOrder(ctx, tx, b, co, order, o, basis)
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
// 回补库存（返回任务键）。不调支付渠道：钱是平台退的。还没退的实收比各行剩余净额少（之前有只退钱、记成货款的
// 平台退款）时，行退款从后往前收紧到实收（同 platformRefund），Σ 退款行 = 退款单货款，行的已退金额不虚高。
func (s *ChannelService) refundWholeChannelOrder(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, order repository.Order, o channel.ChannelOrder, basis *channelKeelBasis) (string, error) {
	key := fmt.Sprintf("channel_refund:%d:%s:cancel", b.ID, co.ExternalOrderID)
	if done, err := tx.ChannelRefundExists(ctx, key); err != nil || done {
		return "", err
	}
	// 平台取消时自己放回了库存（取消带的退款行）：推送基线跟着加，理由同下面 platformRefund。
	// 单笔记过的部分退款（它们的基线当时已经调过）不再算。
	for _, rf := range o.Refunds {
		if !rf.Restock || rf.ExternalID == "" || basis.absorbed(rf.ExternalID) {
			continue // 建单时吸收的退款：那几件没扣过 keel 库存，接单时基线也只减了剩余件数
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
	goods = trimRefundLines(lines, goods, max(total, 0))
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
//
// 金额：先把平台金额换成 keel 口径 —— 价外税的店平台退款含税、税不进 keel 订单，按 BuyerPaid / (BuyerPaid + Tax)
// 剥掉税那一份（价内税的店不剥）。有退款行时按 keel 订单行的实付（行金额 − 行优惠）× 件数 / 下单件数，平台金额更少时
// （扣了手续费）按平台的、更多时按行算的；没有退款行（只退钱）时先记运费（不超过还能退的运费 = 实付运费 − 已退运费），
// 其余记货款、不落行（refund_items 要件数 > 0，这笔钱对不上哪一件）。都不超过还没退的实收。
//
// 退款行落到 keel 订单行：按平台行 ID（建单依据 basis.Items，同一个变体占两行也分得开）；依据里没有的平台行
// （建单时已经剩 0 件、没进 keel 订单）不记件数。没有依据（或适配器不给平台行 ID）时退回按 SKU 找，同一个 SKU 的几行
// 按顺序分。
func (s *ChannelService) platformRefund(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	order *repository.Order, rf channel.Refund, am channel.OrderAmounts, basis *channelKeelBasis) (string, error) {
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
	byID := map[int64]*repository.RefundableItem{}
	bySKU := map[int64][]*repository.RefundableItem{}
	for i := range items {
		byID[items[i].ID] = &items[i]
		bySKU[items[i].SKUID] = append(bySKU[items[i].SKUID], &items[i])
	}
	var lines []repository.NewRefundItem
	var goods int64
	take := func(it *repository.RefundableItem, want int32) int32 {
		qty := min(want, it.Quantity-it.RefundedQty)
		if qty <= 0 {
			return 0
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
		return qty
	}
	for _, l := range rf.Lines {
		if l.Qty <= 0 {
			continue
		}
		if basis != nil && l.ExternalLineID != "" && len(basis.Items) > 0 {
			if it := byID[basis.Items[l.ExternalLineID]]; it != nil {
				take(it, l.Qty)
			}
			continue // 依据里没有：建单时这一行已经剩 0 件
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
		want := l.Qty
		for _, it := range bySKU[link.KeelID] {
			if want <= 0 {
				break
			}
			want -= take(it, want)
		}
	}
	// 合并同一个 keel 订单行的退款行（uk_refund_items：一张退款单每行只能出现一次）。
	lines = mergeRefundLines(lines)
	remain := order.PaidCents - order.RefundedCents
	total := min(keelRefundCents(rf.AmountCents, am), remain)
	if len(lines) > 0 {
		total = min(total, goods)
		goods = trimRefundLines(lines, goods, max(total, 0))
	} else {
		refundedFreight, err := tx.OtherRefundFreight(ctx, order.ID, 0)
		if err != nil {
			return "", err
		}
		freight := min(max(total, 0), max(order.FreightPaidCents()-refundedFreight, 0))
		goods = total - freight
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

// keelRefundCents 把平台退款金额换成 keel 口径：价外税的店剥掉税那一份（按 BuyerPaid / (BuyerPaid + Tax)，向下取整），
// 价内税的店（税在商品价里）原样。
func keelRefundCents(platform int64, am channel.OrderAmounts) int64 {
	if am.TaxesIncluded || am.TaxCents <= 0 || am.BuyerPaidCents <= 0 {
		return platform
	}
	return platform * am.BuyerPaidCents / (am.BuyerPaidCents + am.TaxCents)
}

// trimRefundLines 把退款行金额从后往前减，直到合计 = want（want < sum 时）；返回收紧后的合计。
// 行的件数不变（货确实退了），只是这几件实际退回的钱更少（平台扣了手续费、或之前只退钱的那笔已经占掉了一部分）。
func trimRefundLines(lines []repository.NewRefundItem, sum, want int64) int64 {
	for i := len(lines) - 1; i >= 0 && sum > want; i-- {
		d := min(sum-want, lines[i].AmountCents)
		lines[i].AmountCents -= d
		sum -= d
	}
	return sum
}

// mergeRefundLines 把落到同一个 keel 订单行上的退款行并成一行（按第一次出现的顺序）。
func mergeRefundLines(lines []repository.NewRefundItem) []repository.NewRefundItem {
	at := map[int64]int{}
	out := lines[:0:0]
	for _, l := range lines {
		if i, ok := at[l.OrderItemID]; ok {
			out[i].Quantity += l.Quantity
			out[i].AmountCents += l.AmountCents
			continue
		}
		at[l.OrderItemID] = len(out)
		out = append(out, l)
	}
	return out
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

// RetryChannelOrder 是后台「重试」：对有异常、且没有活着（非 90）的 keel 订单的渠道单，以及 keel 草稿卡在 0 /
// 被孤儿清扫关掉的渠道单（判据见函数里）。重新回读平台、强制重走接单
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
		var liveStatus *int16
		if live != nil {
			liveStatus = &live.Status
		}
		if !channelOrderRetryable(co, liveStatus) {
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

// channelOrderRetryable 是后台「重试」的判据（RetryChannelOrder 与后台渠道单的 retryable 同一个）。
// liveStatus 是渠道单指着的 keel 订单此刻的状态，没有活着的（没有单号、或已经关到 90）为 nil。能重试的三种：
// 有异常且没有活着的 keel 订单；keel 草稿卡在 0（SAGA 提交失败、回调任务也重试完了）—— 强制重走时再提交同一个 gid；
// 草稿被孤儿清扫关到了 90 而渠道单没有异常（渠道单还指着那张关掉的草稿）—— 重新建单。
func channelOrderRetryable(co repository.ChannelOrder, liveStatus *int16) bool {
	if liveStatus != nil {
		return *liveStatus == orderStatusDraft
	}
	if co.Exception != nil {
		return true
	}
	return co.OrderNo != nil && (co.Status == repository.ChannelOrderNew || co.Status == repository.ChannelOrderAccepted)
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

// channelKeelBasis 是 channel_orders.keel_basis（00326）：建 keel 订单时的依据。
type channelKeelBasis struct {
	OrderNo         string           `json:"order_no"`
	Items           map[string]int64 `json:"items"`            // 平台行 ID → order_items.id
	AbsorbedRefunds []string         `json:"absorbed_refunds"` // 建单时已有的带行退款（已按剩余件数吸收）
}

// keelBasisOf 读渠道单的建单依据；不是 orderNo 这张 keel 订单的（没写过、或属于之前关掉的草稿）返回 nil。
func keelBasisOf(ctx context.Context, tx repository.Tx, channelOrderID int64, orderNo string) (*channelKeelBasis, error) {
	raw, err := tx.GetChannelOrderKeelBasis(ctx, channelOrderID)
	if err != nil {
		return nil, err
	}
	var b channelKeelBasis
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("渠道单 %d 的 keel_basis 解不开：%w", channelOrderID, err)
		}
	}
	if b.OrderNo == "" || b.OrderNo != orderNo {
		return nil, nil
	}
	return &b, nil
}

// absorbed：这笔平台退款在建 keel 订单时已经吸收（不再记）。
func (b *channelKeelBasis) absorbed(externalRefundID string) bool {
	if b == nil {
		return false
	}
	for _, id := range b.AbsorbedRefunds {
		if id == externalRefundID {
			return true
		}
	}
	return false
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
	// 每行按当时的剩余件数（Qty − RefundedQty）进 keel 订单：建单前平台上已经退掉 / 移除的件不扣库存、不发货，
	// 剩 0 的行不进来（也就不要求它链到 keel SKU）。金额校验仍按下单件数（平台快照的 Goods）。
	type keelLine struct {
		extLine     string
		sku         int64
		title       string
		qty, placed int32 // 剩余件数、下单件数
		price       int64
	}
	var lines []keelLine
	var placedGoods int64
	for i, l := range o.Lines {
		if l.Qty <= 0 {
			continue
		}
		placedGoods += l.PriceCents * int64(l.Qty)
		cur := l.Qty - max(0, l.RefundedQty)
		if cur <= 0 {
			continue
		}
		link, err := tx.ChannelItemLinkByExternal(ctx, b.ID, repository.ChannelItemSKU, l.ExternalSKUID)
		if errors.Is(err, repository.ErrChannelNotFound) || (err == nil && l.ExternalSKUID == "") {
			return fail("第 %d 行「%s」（渠道 SKU %s）没有链到 keel 的 SKU", i+1, l.Title, l.ExternalSKUID)
		}
		if err != nil {
			return nil, err
		}
		lines = append(lines, keelLine{extLine: l.ExternalLineID, sku: link.KeelID, title: l.Title, qty: cur, placed: l.Qty,
			price: l.PriceCents})
	}
	if len(lines) == 0 {
		return fail("订单里没有还要发的商品行")
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
	switch {
	case placedGoods != am.GoodsCents:
		return fail("金额：商品行合计 %d 分，平台给的商品金额 %d 分", placedGoods, am.GoodsCents)
	case am.BuyerPaidCents != am.GoodsCents+am.FreightCents-discount || discount < 0 || am.FreightCents < 0:
		return fail("金额：平台快照对不上（商品 %d + 运费 %d − 优惠 %d ≠ 实付 %d，单位分）",
			am.GoodsCents, am.FreightCents, discount, am.BuyerPaidCents)
	}
	// 优惠按下单时的行金额比例摊到平台的每一行（最大余数），摊不进的记运费优惠；剩余件数少于下单件数的行按件数比例
	// 留下它那一份（退掉 / 移除的件带走它们的优惠）。没有退掉的件时 keel 实付 = BuyerPaid。
	type placedLine struct {
		amount int64
		kept   int // lines 里的下标，-1 = 剩 0 件、没进 keel 订单
	}
	var placed []placedLine
	k := 0
	for _, l := range o.Lines {
		if l.Qty <= 0 {
			continue
		}
		pl := placedLine{amount: l.PriceCents * int64(l.Qty), kept: -1}
		if l.Qty-max(0, l.RefundedQty) > 0 {
			pl.kept, k = k, k+1
		}
		placed = append(placed, pl)
	}
	lineDiscount := min(discount, placedGoods)
	all := make([]int64, len(placed))
	if placedGoods > 0 {
		var given int64
		for i, pl := range placed {
			all[i] = lineDiscount * pl.amount / placedGoods
			given += all[i]
		}
		for i := 0; given < lineDiscount; i = (i + 1) % len(placed) {
			if all[i] < placed[i].amount {
				all[i]++
				given++
			}
		}
	}
	freightDiscount := discount - lineDiscount
	if freightDiscount > am.FreightCents {
		return fail("金额：优惠 %d 分超过了商品与运费的合计", discount)
	}
	shares := make([]int64, len(lines))
	for i, pl := range placed {
		if pl.kept >= 0 {
			l := lines[pl.kept]
			shares[pl.kept] = all[i] * int64(l.qty) / int64(l.placed)
		}
	}
	var goods, keptDiscount int64
	for i, l := range lines {
		goods += l.price * int64(l.qty)
		keptDiscount += shares[i]
	}
	keelDiscount := keptDiscount + freightDiscount
	payable := goods + am.FreightCents - keelDiscount
	// 建单时平台上已有的带行退款：退掉的件已经不在 keel 订单里（上面按剩余件数建），之后不再记成 keel 退款单。
	// 推送基线：接单时只减剩余件数；平台那边是「减下单件数、再加回退款放回的件数」，放回了就对得上，
	// 没放回（NO_RESTOCK）就差这几件，下一次推送记一条差异、按 keel 的数覆盖。只退钱的退款（没有行）不吸收，接单后照常记。
	basis := channelKeelBasis{Items: map[string]int64{}, AbsorbedRefunds: []string{}}
	for _, rf := range o.Refunds {
		if rf.ExternalID != "" && len(rf.Lines) > 0 {
			basis.AbsorbedRefunds = append(basis.AbsorbedRefunds, rf.ExternalID)
		}
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
		StoreID: *co.StoreID, GoodsAmountCents: goods, FreightCents: am.FreightCents,
		FreightDiscountCents: freightDiscount, DiscountCents: keelDiscount, PayableCents: payable,
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
	// 平台行 → keel 订单行：同一个事务里按插入顺序建的，ListRefundableItems 按 id 排，一一对应。
	items, err := tx.ListRefundableItems(ctx, draft.ID)
	if err != nil {
		return nil, err
	}
	if len(items) != len(lines) {
		return nil, fmt.Errorf("渠道单 %d 的 keel 草稿 %s 建了 %d 行，读回来 %d 行", co.ID, orderNo, len(lines), len(items))
	}
	for i, l := range lines {
		if l.extLine != "" {
			basis.Items[l.extLine] = items[i].ID
		}
	}
	basis.OrderNo = orderNo
	basisJSON, _ := json.Marshal(basis)
	if err := tx.SetChannelOrderKeelBasis(ctx, co.ID, basisJSON); err != nil {
		return nil, err
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
	// 同一个变体在平台单上占两行时 keel 订单也是两行，扣减按 SKU 并起来（同 openChannelOrderTx 的 deduct）。
	saga.lines = mergeSKULines(saga.lines)
	sort.Slice(saga.lines, func(i, j int) bool { return saga.lines[i].SKUID < saga.lines[j].SKUID })
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

// enqueueChannelAction 入队一个对渠道订单的动作（同一张单同一种动作只排一次；对申请的动作按申请各排一次：
// job_key = act:<id>:<kind>[:<外部申请 ID>]）。
func enqueueChannelAction(ctx context.Context, tx repository.Tx, channelOrderID int64, a channel.Action) error {
	key := "act:" + strconv.FormatInt(channelOrderID, 10) + ":" + string(a.Kind)
	if a.ExternalRequestID != "" {
		key += ":" + a.ExternalRequestID
	}
	if a.IdemKey == "" {
		a.IdemKey = key
	}
	payload, _ := json.Marshal(channelActionJob{ChannelOrderID: channelOrderID, Action: a})
	_, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelOrderAction,
		JobKey: key, Payload: payload, MaxAttempts: channelPushMaxAttempts})
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
