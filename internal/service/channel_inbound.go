package service

// 渠道回调的入口与处理（POST /api/v1/webhooks/channels/{binding_id}，handler/webhook_channel.go）。
//
//	Host 定租户（同支付回调，v1 组的 res.Middleware）→ 取 binding → 适配器验签解析
//	→ 每个事件写 channel_inbound_events（同一外部事件 ID 只落一行）→ 入队 channel.inbound → 立刻回平台要的 ack
//
// 处理是异步的（workInbound）：平台对回调的超时都很短（Shopify 5 秒），而处理要调平台 API 回读、建订单。
// 按事件类别分发给登记过的处理器（OnInbound）；没有处理器的类别标「忽略」—— 第一期只有入口，处理器在第二、三期。
//
// 对外只说两句话：200（含 ack）与空的 401。binding 不存在、适配器没编进来、验签失败、没配密钥都是同一个 401，
// 不让回调地址变成「这家店接没接某个渠道」的探测器（与支付回调同一个理由）。验签通过之后的问题
// （事件解不开、binding 已停用）一律 200 + 服务端日志：平台重推也没用。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// MaxChannelWebhookBytes 是回调体的上限（验签要读完全部字节，所以闸门必须在读之前）。
const MaxChannelWebhookBytes = 1 << 20

// ErrChannelWebhookRejected：回不出 ack 的回调（binding 不存在 / 适配器没编进来 / 验签失败）。入口回空 401。
var ErrChannelWebhookRejected = errors.New("渠道回调被拒绝")

// InboundHandler 处理一类回调事件。返回错误时任务退避重试（处理器要幂等：同一事件可能被处理多次）。
type InboundHandler func(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error

// OnInbound 登记一类事件的处理器（装配时调用；第二期起 Shopify 商品、第三期起订单）。
func (s *ChannelService) OnInbound(kind channel.EventKind, h InboundHandler) {
	if s.handlers == nil {
		s.handlers = map[channel.EventKind]InboundHandler{}
	}
	s.handlers[kind] = h
}

type channelInboundJob struct {
	EventID int64 `json:"event_id"`
}

// Inbound 处理一次回调：返回要回给平台的 ack。ctx 里的租户来自 Host。
func (s *ChannelService) Inbound(ctx context.Context, bindingID int64, r *http.Request, body []byte) ([]byte, error) {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	log := s.log.With("merchant_id", merchantID, "binding_id", bindingID)
	var b repository.ChannelBinding
	var ab channel.Binding
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if b, e = tx.GetChannelBinding(ctx, bindingID); e != nil {
			return e
		}
		ab, e = adapterBinding(ctx, tx, merchantID, b)
		return e
	})
	if errors.Is(err, repository.ErrChannelNotFound) {
		log.WarnContext(ctx, "渠道回调指向不存在的 binding")
		return nil, ErrChannelWebhookRejected
	}
	if err != nil {
		return nil, err
	}
	a, ok := s.reg.Lookup(b.Channel)
	if !ok {
		log.ErrorContext(ctx, "渠道回调的 binding 指向一个这个进程没编进来的渠道", "channel", b.Channel)
		return nil, ErrChannelWebhookRejected
	}
	evs, ack, err := a.ParseInbound(ab, r, body)
	if errors.Is(err, channel.ErrBadSignature) {
		log.WarnContext(ctx, "渠道回调验签失败", "channel", b.Channel)
		return nil, ErrChannelWebhookRejected
	}
	if err != nil {
		// 验签过了但解不开：平台重推也一样，回 ack 并留日志。
		log.ErrorContext(ctx, "渠道回调解不开，已应答、未处理", "channel", b.Channel, "err", err)
		return ack, nil
	}
	active := b.Status == repository.ChannelBindingActive
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		for _, ev := range evs {
			if ev.ExternalID == "" {
				log.ErrorContext(ctx, "渠道回调的事件没有外部 ID，没法去重，丢弃", "topic", ev.Topic)
				continue
			}
			status := repository.ChannelEventPending
			if !active || ev.Kind == channel.EventIgnored {
				status = repository.ChannelEventIgnored
			}
			stored, _ := json.Marshal(ev)
			id, inserted, err := tx.InsertChannelInboundEvent(ctx, repository.ChannelInboundEventInput{BindingID: b.ID,
				ExternalEventID: ev.ExternalID, Topic: topicOf(ev), Payload: stored, Status: status})
			if err != nil {
				return err
			}
			if !inserted || status != repository.ChannelEventPending {
				continue
			}
			payload, _ := json.Marshal(channelInboundJob{EventID: id})
			if _, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelInbound,
				JobKey: fmt.Sprintf("evt:%d", id), Payload: payload, MaxAttempts: channelPushMaxAttempts}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ack, nil
}

func topicOf(ev channel.Event) string {
	if ev.Topic != "" {
		return ev.Topic
	}
	return string(ev.Kind)
}

func (s *ChannelService) handleInbound(ctx context.Context, j repository.Job) {
	var p channelInboundJob
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		s.retry(ctx, j, fmt.Errorf("回调任务的载荷解不开: %w", err))
		return
	}
	var stored repository.ChannelInboundEvent
	var b repository.ChannelBinding
	var secrets json.RawMessage
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if stored, e = tx.GetChannelInboundEvent(ctx, p.EventID); e != nil {
			return e
		}
		if b, e = tx.GetChannelBinding(ctx, stored.BindingID); e != nil {
			return e
		}
		secrets, e = tx.ChannelBindingSecrets(ctx, b.ID) // 只用来给处理器的错误脱敏
		return e
	})
	if errors.Is(err, repository.ErrChannelNotFound) {
		s.finish(ctx, j.ID) // binding 或事件删了
		return
	}
	if err != nil {
		s.retry(ctx, j, err)
		return
	}
	mark := func(status int16, msg *string) {
		if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			return tx.MarkChannelInboundEvent(ctx, stored.ID, status, msg)
		}); err != nil {
			s.log.WarnContext(ctx, "回调事件的处理结果没记下", "event_id", stored.ID, "err", err)
		}
	}
	var ev channel.Event
	if err := json.Unmarshal(stored.Payload, &ev); err != nil {
		msg := "存档的事件解不开：" + err.Error()
		mark(repository.ChannelEventFailed, &msg)
		s.finish(ctx, j.ID)
		return
	}
	h := s.handlers[ev.Kind]
	if h == nil || b.Status != repository.ChannelBindingActive {
		msg := "没有这类事件的处理器"
		if h != nil {
			msg = "binding 不在启用中"
		}
		mark(repository.ChannelEventIgnored, &msg)
		s.finish(ctx, j.ID)
		return
	}
	if err := h(ctx, b, ev); err != nil {
		err = channel.RedactError(err, secrets)
		msg := err.Error()
		mark(repository.ChannelEventFailed, &msg)
		s.retry(ctx, j, err)
		return
	}
	mark(repository.ChannelEventDone, nil)
	s.finish(ctx, j.ID)
}
