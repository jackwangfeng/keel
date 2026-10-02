package service

// keel 对渠道订单的动作（第三期 Task 5）：channel.order.action 队列的 worker。
//
//	入队：接单 / 拒单（接单 SAGA，channel_order_saga.go）、发货回传（AdminOrderService.Ship → EnqueueShipTx，
//	      与 20 → 30 同一个事务）。job_key = act:<channel_order_id>:<kind>，同一张单同一种动作只排一次。
//	执行：取渠道单与 binding → 适配器 Act → 成功标完成；
//	      ErrUnsupported（渠道没有这个动作）→ 标完成、记 Warn；
//	      凭据失效 → binding 标「凭据失效」（停推送），任务照常退避（重新配好凭据后接着回传）；
//	      其余退避重试（错误文本过 RedactError），用尽进死信 → 渠道单标异常（「发货没回传上：…」），门店收通知。
//
// # 发货回传重放（Review Focus 6）
//
// 平台做了、响应丢了（超时 / 5xx）→ 任务重试 → 适配器先读平台上的发货单，已经没有可发的就当成功
// （Shopify：没有 OPEN / IN_PROGRESS 的 fulfillment order，见 shopify/order.go 的 ship），再加上
// @idempotent 键 = act:<id>:ship，所以平台上只有一条发货记录。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
)

// carrierCompanies 是承运商代码在平台物流信息里的名字（平台按名字识别承运商、生成跟踪链接；
// 与 notification.go 的 carrierNames 中文名分开）。认不出的原样传代码。
var carrierCompanies = map[string]string{
	"sf": "SF Express", "jd": "JD Logistics", "yto": "YTO Express", "zto": "ZTO Express", "sto": "STO Express",
	"yd": "Yunda Express", "ems": "EMS", "jt": "J&T Express", "db": "Deppon",
	"ups": "UPS", "usps": "USPS", "fedex": "FedEx", "dhl": "DHL Express",
}

// carrierCompany 是发货回传给平台的承运商名。
func carrierCompany(code string) string {
	if name, ok := carrierCompanies[strings.ToLower(strings.TrimSpace(code))]; ok {
		return name
	}
	return strings.TrimSpace(code)
}

// EnqueueShipTx 在后台发货的事务里入队一条发货回传（order 是 Ship 已经取出来的订单行）。
// 渠道层没开（nil 接收者）或订单不是渠道单时什么都不做。
func (s *ChannelService) EnqueueShipTx(ctx context.Context, tx repository.Tx, order repository.Order, carrierName, trackingNo string) error {
	if s == nil || order.Source != repository.OrderSourceChannel || order.ChannelOrderID == nil {
		return nil
	}
	return enqueueChannelAction(ctx, tx, *order.ChannelOrderID, channel.Action{Kind: channel.ActShip,
		TrackingCompany: carrierName, TrackingNo: trackingNo})
}

func (s *ChannelService) runAction(ctx context.Context, j repository.Job) {
	var p channelActionJob
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		s.retryAction(ctx, j, p, fmt.Errorf("渠道订单动作的载荷解不开: %w", err))
		return
	}
	log := s.log.With("merchant_id", j.MerchantID, "job_id", j.ID, "channel_order_id", p.ChannelOrderID, "action", p.Action.Kind)
	var (
		co repository.ChannelOrder
		b  repository.ChannelBinding
		ab channel.Binding
	)
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if co, e = tx.GetChannelOrder(ctx, p.ChannelOrderID); e != nil {
			return e
		}
		if b, e = tx.GetChannelBinding(ctx, co.BindingID); e != nil {
			return e
		}
		ab, e = adapterBinding(ctx, tx, j.MerchantID, b)
		return e
	})
	if errors.Is(err, repository.ErrChannelNotFound) {
		log.WarnContext(ctx, "渠道单或它的 binding 已经删了，这个动作不做了")
		s.finish(ctx, j.ID)
		return
	}
	if err != nil {
		s.retryAction(ctx, j, p, err)
		return
	}
	a, ok := s.reg.Lookup(b.Channel)
	outlet, isOutlet := a.(channel.Outlet)
	if !ok || !isOutlet {
		s.retryAction(ctx, j, p, fmt.Errorf("%w：%q（或它不是销售渠道）", ErrChannelUnknownKind, b.Channel))
		return
	}
	var snap struct{ ExternalStoreID string }
	_ = json.Unmarshal(co.LastPayload, &snap)
	err = outlet.Act(ctx, ab, channel.OrderRef{ExternalOrderID: co.ExternalOrderID, ExternalStoreID: snap.ExternalStoreID}, p.Action)
	err = channel.RedactError(err, ab.Secrets)
	switch {
	case err == nil:
		s.finish(ctx, j.ID)
	case errors.Is(err, channel.ErrUnsupported):
		log.WarnContext(ctx, "渠道不支持这个动作，跳过", "err", err)
		s.finish(ctx, j.ID)
	default:
		if errors.Is(err, channel.ErrCredentials) {
			s.markCredentialsBroken(ctx, b.ID, err)
		}
		s.retryAction(ctx, j, p, err)
	}
}

// actionLabel 是动作没做成时给人看的说法。
func actionLabel(k channel.ActionKind) string {
	switch k {
	case channel.ActShip:
		return "发货没回传上"
	case channel.ActAccept:
		return "接单没回传上"
	case channel.ActReject:
		return "拒单没回传上"
	default:
		return fmt.Sprintf("对平台的动作 %s 没做成", k)
	}
}

// retryAction 退避重试；进了死信就把渠道单标异常、通知门店（keel 订单不动）。
func (s *ChannelService) retryAction(ctx context.Context, j repository.Job, p channelActionJob, cause error) {
	if !s.retry(ctx, j, cause) || p.ChannelOrderID == 0 {
		return
	}
	reason := actionLabel(p.Action.Kind) + "：" + truncateRunes(cause.Error(), 200)
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err := tx.LockChannelOrder(ctx, p.ChannelOrderID)
		if err != nil {
			return err
		}
		if err := tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: co.Status, OrderNo: co.OrderNo,
			Exception: &reason, AcceptDeadline: co.AcceptDeadline}); err != nil {
			return err
		}
		if co.OrderNo == nil {
			return nil
		}
		order, err := tx.FindOrderByNo(ctx, *co.OrderNo)
		if err != nil {
			return err
		}
		return notifyChannelOrderAttention(ctx, tx, order, "act:"+string(p.Action.Kind), reason,
			"请到平台后台手工处理。")
	}); err != nil {
		s.log.ErrorContext(ctx, "渠道订单动作进了死信，标异常也失败了", "channel_order_id", p.ChannelOrderID, "err", err)
	}
}
