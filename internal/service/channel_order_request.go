package service

// 人工接单 / 拒单、平台申请流与截止扫描（第三期 Task 6，spec §5.2 第 4、5 条）。渠道层通用，按 Caps 分支。
//
//	AcceptChannelOrder（AcceptRequired 且在等人）：回读平台 → applyChannelOrderOpts(accept) 走 Task 4 的建单 + SAGA；
//	    收尾分支入队 Act(接单)，缺货时补偿分支关单、标异常、入队 Act(拒单)（Review Focus 4 后半）。
//	RejectChannelOrder：渠道单 → 7 已拒单、入队 Act(拒单)；不建 keel 订单。之后平台上迟到的「新单」快照不把它盖回去。
//	orderRequest（EventOrderRequest 的处理器）：申请 → channel_order_requests，按（渠道单, 外部申请 ID）幂等；
//	    binding config.request_policy：manual（缺省）等人，门店收通知；
//	    auto_agree_unshipped：keel 订单还没发货的「取消」自动同意（入队 Act(agree_request)），其余照旧等人。
//	DecideRequest：同意 → Act(agree_request)，拒绝 → Act(reject_request)。
//
// # 同意了也不动 keel 订单（不变量 2）
//
// 同意只是对平台的一个回复。平台确认之后会有订单事件（取消 / 退款），orderChanged → applyPlatformFacts
// 才整单退款 / 记退款、回补库存（Task 5 的路径，按平台退款 ID 幂等）。这样「平台没确认」「回复没送达」
// 「平台按超时规则自己处理了」三种情况下 keel 都只跟着平台的事实走，不会先退了款再发现平台没取消。
//
// # 截止扫描（SweepChannelDeadlines，channel_worker.go 的 housekeep 每分钟一次）
//
//	接单：离 accept_deadline 不到 config.accept_remind_minutes（缺省 3）分钟、还在等人的单，提醒门店一次。
//	      「提醒过」不另记一列：通知表的去重键（merchant_channel_order_pending:accept_remind:<渠道单 id>）就是记录，
//	      查询用 NOT EXISTS 跳过、插入撞唯一键不重发（db/queries/channels.sql 的 DueAcceptReminders）。
//	申请：过了平台截止还待处理 → 4 超时自动同意（平台会按自己的规则处理，keel 等平台事实）+ 门店通知。
//
// # 通知目标
//
// 等接单的渠道单还没有 keel 订单号，而通知的 'order' 目标要 order_no 非空（Task 4 的映射异常因此没法通知）。
// 00325 给通知加了第四种目标 channel_orders（只要 store_id，跳门店的渠道订单页），这里的三种提醒都用
// merchant_channel_order_pending 发到那里，正文写明平台单号。渠道单没映射到 keel 门店（store_id 空）时发不了，记日志。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
)

// binding config.request_policy 的取值。
const (
	requestPolicyManual             = "manual"
	requestPolicyAutoAgreeUnshipped = "auto_agree_unshipped"
)

// channelDeadlineBatch 是截止扫描一家商家一轮最多处理的条数（每分钟一轮，积压下一轮接着来）。
const channelDeadlineBatch = 100

var (
	// ErrChannelOrderNotAcceptable：这张渠道单不在「等人接单」（渠道不要求接单、已接 / 已拒 / 已取消、有异常、
	// 已经有 keel 订单）——接单与拒单都返回它（409）。有异常的走「重试」（RetryChannelOrder）。
	ErrChannelOrderNotAcceptable = errors.New("这张渠道单不在等待接单")
	// ErrChannelOrderAcceptFailed：接了，但没成单（缺货 / 映射不全），原因跟在后面；渠道单已标异常，
	// 缺货时已自动入队拒单（422）。
	ErrChannelOrderAcceptFailed = errors.New("接单没成功")
	// ErrChannelRequestDecided：申请已经处置过（同意 / 拒绝 / 超时 / 平台撤销），不能再处置（409）。
	ErrChannelRequestDecided = errors.New("这个申请已经处理过了")
)

// AcceptChannelOrder 是后台「接单」：只对 AcceptRequired 的渠道上、还在等人接的单。回读平台的权威状态再建单
// （平台上已经取消了就只记下来，返回 ErrChannelOrderNotAcceptable）。
func (s *ChannelService) AcceptChannelOrder(ctx context.Context, id int64) error {
	b, ext, err := s.awaitingAccept(ctx, id, false)
	if err != nil {
		return err
	}
	o, err := s.fetchChannelOrder(ctx, b, ext)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("binding %d 的适配器不是销售渠道，回读不了订单", b.ID)
	}
	if err := s.applyChannelOrderOpts(ctx, b, *o, applyOpts{accept: true}); err != nil {
		return err
	}
	var co repository.ChannelOrder
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		co, err = tx.GetChannelOrder(ctx, id)
		return err
	}); err != nil {
		return err
	}
	switch {
	case co.Exception != nil:
		return fmt.Errorf("%w：%s", ErrChannelOrderAcceptFailed, *co.Exception)
	case co.OrderNo == nil && co.Status != repository.ChannelOrderAccepted:
		return ErrChannelOrderNotAcceptable // 回读时平台上已经不是新单了（取消 / 被拒）
	}
	return nil // 成单了，或 SAGA 还没落定（协调器接着推，结果由分支写进渠道单）
}

// RejectChannelOrder 是后台「拒单」：渠道单 → 已拒单、入队 Act(拒单)。reason 为空时用「商家拒单」。
func (s *ChannelService) RejectChannelOrder(ctx context.Context, id int64, reason string) error {
	if reason == "" {
		reason = "商家拒单"
	}
	reason = truncateRunes(reason, 200)
	_, _, err := s.awaitingAccept(ctx, id, true, func(ctx context.Context, tx repository.Tx, co repository.ChannelOrder) error {
		if err := tx.SetChannelOrderState(ctx, co.ID, repository.ChannelOrderState{Status: repository.ChannelOrderRejected,
			OrderNo: co.OrderNo, Exception: nil, AcceptDeadline: co.AcceptDeadline}); err != nil {
			return err
		}
		return enqueueChannelAction(ctx, tx, co.ID, channel.Action{Kind: channel.ActReject, Reason: reason})
	})
	return err
}

// awaitingAccept 校验渠道单在等人接单（lock 为真时锁住渠道单行），then 在同一个事务里接着做。
func (s *ChannelService) awaitingAccept(ctx context.Context, id int64, lock bool,
	then ...func(context.Context, repository.Tx, repository.ChannelOrder) error) (repository.ChannelBinding, string, error) {
	var b repository.ChannelBinding
	var ext string
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		get := tx.GetChannelOrder
		if lock {
			get = tx.LockChannelOrder
		}
		co, err := get(ctx, id)
		if err != nil {
			return err
		}
		if b, err = tx.GetChannelBinding(ctx, co.BindingID); err != nil {
			return err
		}
		a, ok := s.reg.Lookup(b.Channel)
		if !ok {
			return fmt.Errorf("%w：%q", ErrChannelUnknownKind, b.Channel)
		}
		live, err := liveChannelKeelOrder(ctx, tx, co)
		if err != nil {
			return err
		}
		if !a.Caps().AcceptRequired || co.Status != repository.ChannelOrderNew || co.Exception != nil || live != nil {
			return ErrChannelOrderNotAcceptable
		}
		ext = co.ExternalOrderID
		for _, f := range then {
			if err := f(ctx, tx, co); err != nil {
				return err
			}
		}
		return nil
	})
	return b, ext, err
}

// requestLine 是 channel_order_requests.lines 的一项。
type requestLine struct {
	ExternalSKUID string `json:"external_sku_id"`
	Qty           int32  `json:"qty"`
}

// orderRequest 是 EventOrderRequest 的处理器（文件头）。申请先于订单到（还没有渠道单）时先回读订单收下。
func (s *ChannelService) orderRequest(ctx context.Context, b repository.ChannelBinding, ev channel.Event) error {
	if b.Roles&repository.ChannelRoleOutlet == 0 {
		return nil
	}
	q := ev.Request
	if q == nil || ev.ExternalOrderID == "" || q.ExternalRequestID == "" ||
		q.Kind < channel.RequestCancel || q.Kind > channel.RequestStockout {
		s.log.WarnContext(ctx, "平台申请回调缺订单号 / 申请号或类别不认识，丢弃", "binding_id", b.ID, "topic", ev.Topic)
		return nil
	}
	for attempt := 0; ; attempt++ {
		err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			co, err := tx.LockChannelOrderByExternal(ctx, b.ID, ev.ExternalOrderID)
			if err != nil {
				return err
			}
			return s.recordRequest(ctx, tx, b, co, *q)
		})
		if !errors.Is(err, repository.ErrChannelNotFound) || attempt > 0 {
			return err
		}
		o, err := s.fetchChannelOrder(ctx, b, ev.ExternalOrderID)
		if err != nil || o == nil {
			return err
		}
		if err := s.applyChannelOrder(ctx, b, *o); err != nil {
			return err
		}
	}
}

// recordRequest 在渠道单行锁着的事务里记申请、按策略处置。
func (s *ChannelService) recordRequest(ctx context.Context, tx repository.Tx, b repository.ChannelBinding,
	co repository.ChannelOrder, q channel.OrderRequest) error {
	lines := make([]requestLine, 0, len(q.Lines))
	for _, l := range q.Lines {
		lines = append(lines, requestLine{ExternalSKUID: l.ExternalSKUID, Qty: l.Qty})
	}
	rawLines, _ := json.Marshal(lines)
	status := repository.ChannelRequestPending
	if q.Withdrawn {
		status = repository.ChannelRequestWithdrawn
	}
	amount := max(q.AmountCents, 0)
	req, inserted, err := tx.InsertOrLockChannelOrderRequest(ctx, repository.NewChannelOrderRequest{ChannelOrderID: co.ID,
		ExternalRequestID: truncateRunes(q.ExternalRequestID, 200), Kind: int16(q.Kind), Lines: rawLines,
		AmountCents: amount, Reason: truncateRunes(q.Reason, 500), Status: status, Deadline: q.Deadline})
	if err != nil {
		return err
	}
	if !inserted {
		if q.Withdrawn && req.Status == repository.ChannelRequestPending {
			_, err = tx.DecideChannelOrderRequest(ctx, req.ID, repository.ChannelRequestWithdrawn, nil)
		}
		return err // 重推的同一个申请：已经记过（处置过的不再动）
	}
	if status != repository.ChannelRequestPending {
		return nil
	}
	live, err := liveChannelKeelOrder(ctx, tx, co)
	if err != nil {
		return err
	}
	if parseBindingConfig(b.Config).RequestPolicy == requestPolicyAutoAgreeUnshipped && q.Kind == channel.RequestCancel &&
		unshipped(co, live) {
		if _, err := tx.DecideChannelOrderRequest(ctx, req.ID, repository.ChannelRequestAgreed, nil); err != nil {
			return err
		}
		return enqueueChannelAction(ctx, tx, co.ID, channel.Action{Kind: channel.ActAgreeRequest, ExternalRequestID: req.ExternalRequestID})
	}
	reason := fmt.Sprintf("平台单 %s 发来%s申请", orderNameOf(co), requestKindLabel(req.Kind))
	if req.AmountCents > 0 {
		reason += "，涉及 " + yuanText(req.AmountCents)
	}
	if req.Reason != "" {
		reason += "（" + truncateRunes(req.Reason, 60) + "）"
	}
	return s.notifyChannelPending(ctx, tx, co.StoreID, co.ID, fmt.Sprintf("request:%d", req.ID), reason,
		"请在平台截止前到渠道订单页同意或拒绝；同意后 keel 订单等平台确认再退款。")
}

// unshipped：渠道单与 keel 订单都还没发货（keel 订单没有，或在 0 / 10 / 20）。
func unshipped(co repository.ChannelOrder, live *repository.Order) bool {
	if co.Status != repository.ChannelOrderNew && co.Status != repository.ChannelOrderAccepted {
		return false
	}
	return live == nil || live.Status <= orderStatusPaid
}

func requestKindLabel(k int16) string {
	switch channel.RequestKind(k) {
	case channel.RequestCancel:
		return "取消"
	case channel.RequestPartialRefund:
		return "部分退款"
	default:
		return "缺货调整"
	}
}

func orderNameOf(co repository.ChannelOrder) string {
	if co.ExternalOrderName != "" {
		return co.ExternalOrderName
	}
	return co.ExternalOrderID
}

// DecideRequest 是后台对一个待处理申请的处置：同意 → Act(agree_request)，拒绝 → Act(reject_request)。
// keel 订单不动（文件头「同意了也不动 keel 订单」）。只锁申请行（不锁渠道单），与收申请的事务不交叉加锁。
func (s *ChannelService) DecideRequest(ctx context.Context, requestID int64, agree bool, staffID int64) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		req, err := tx.LockChannelOrderRequest(ctx, requestID)
		if err != nil {
			return err
		}
		if req.Status != repository.ChannelRequestPending {
			return ErrChannelRequestDecided
		}
		status, kind := repository.ChannelRequestRejected, channel.ActRejectRequest
		if agree {
			status, kind = repository.ChannelRequestAgreed, channel.ActAgreeRequest
		}
		ok, err := tx.DecideChannelOrderRequest(ctx, req.ID, status, &staffID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrChannelRequestDecided
		}
		return enqueueChannelAction(ctx, tx, req.ChannelOrderID, channel.Action{Kind: kind, ExternalRequestID: req.ExternalRequestID})
	})
}

// ListChannelOrderRequests 是一张渠道单上的申请（后台详情，新的在前）。
func (s *ChannelService) ListChannelOrderRequests(ctx context.Context, channelOrderID int64) ([]repository.ChannelOrderRequest, error) {
	var out []repository.ChannelOrderRequest
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := tx.GetChannelOrder(ctx, channelOrderID); err != nil {
			return err
		}
		var err error
		out, err = tx.ListChannelOrderRequests(ctx, channelOrderID)
		return err
	})
	return out, err
}

// sweepTenantDeadlines 是一家商家的截止扫描（文件头）。ctx 带着租户（SweepChannelDeadlines 逐家建）。
func (s *ChannelService) sweepTenantDeadlines(ctx context.Context) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		due, err := tx.DueAcceptReminders(ctx, channelDeadlineBatch)
		if err != nil {
			return err
		}
		for _, d := range due {
			mins := int(max(time.Until(d.AcceptDeadline).Minutes(), 0) + 0.5)
			store := d.StoreID
			reason := fmt.Sprintf("平台单 %s 还没接单，约 %d 分钟后到接单截止", d.ExternalOrderName, mins)
			if err := s.notifyChannelPending(ctx, tx, &store, d.ChannelOrderID, fmt.Sprintf("accept_remind:%d", d.ChannelOrderID),
				reason, "请到渠道订单页接单或拒单；过了截止平台会自动取消。"); err != nil {
				return err
			}
		}
		expired, err := tx.ExpiredChannelOrderRequests(ctx, channelDeadlineBatch)
		if err != nil {
			return err
		}
		for _, r := range expired {
			ok, err := tx.DecideChannelOrderRequest(ctx, r.ID, repository.ChannelRequestTimedOut, nil)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			name := r.ExternalOrderName
			reason := fmt.Sprintf("平台单 %s 的%s申请过了处理截止，平台会按它的规则自动处理", name, requestKindLabel(r.Kind))
			if err := s.notifyChannelPending(ctx, tx, r.StoreID, r.ChannelOrderID, fmt.Sprintf("request_timeout:%d", r.ID),
				reason, "keel 订单等平台确认取消 / 退款后再动，请到平台后台核对结果。"); err != nil {
				return err
			}
		}
		return nil
	})
}

// notifyChannelPending 发「渠道订单待处理」给渠道单所在的门店；没映射到 keel 门店的单发不了，记日志。
func (s *ChannelService) notifyChannelPending(ctx context.Context, tx repository.Tx, storeID *int64, channelOrderID int64,
	tag, reason, hint string) error {
	if storeID == nil {
		s.log.WarnContext(ctx, "渠道单没映射到 keel 门店，待处理提醒发不了", "channel_order_id", channelOrderID, "reason", reason)
		return nil
	}
	return emitNotification(ctx, tx, outgoing{Kind: KindMerchantChannelOrderPending, StoreID: *storeID,
		Params: notifyParams{Reason: reason, Hint: hint}, Dedupe: tag})
}
