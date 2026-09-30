package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 多收款退回（00150）：订单不认的每一笔到账原路退回。
//
// 哪些到账订单不认：重复支付（订单已由另一笔入账）、订单已取消或关闭之后才到的、金额与应付不符的。
// 支付回调（payment.go 的 settle）在落支付单的同一个事务里判出这三种，当场开退回单；兜底扫描（Run）
// 再按「渠道成功、但与 orders.paid_cents / paid_at 对不上」找一遍 —— 历史上已经落了的、以及任何
// 别的路径漏掉的都会被补上。开了单就提交给渠道：沙箱开着时服务端扮演渠道当场退回（与售后退款的沙箱
// 同一个做法，refund.go 的 submitToChannel）；真实渠道适配器本轮没有，单子停在 30 等回调，回调走
// /webhooks/refunds/{channel}，按单号前缀 PR 分派到这里（RefundService.Notify）。
//
// 与售后退款（refunds）刻意分开：这不是买家的售后，不动订单的退款维度、不回补库存、不进售后统计，
// 也不需要人审 —— 订单不认的钱本来就不是我们的。

// PaymentReturnNoPrefix 是退回单号的前缀。渠道回调里的单号以它开头就不是售后退款单。
const PaymentReturnNoPrefix = "PR"

// PaymentReturnInterval 是兜底扫描与重试的间隔。
const PaymentReturnInterval = time.Minute

// PaymentReturnRepository 是多收款退回要的仓库。
type PaymentReturnRepository interface {
	tenantRunner
	secretSource
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// PaymentReturnService 开、交、收多收款退回单。
type PaymentReturnService struct {
	repo PaymentReturnRepository
	cfg  PaymentConfig
	log  *slog.Logger
	now  func() time.Time
}

// NewPaymentReturnService 建服务。cfg.Sandbox 决定提交时是否当场模拟渠道退款。
func NewPaymentReturnService(repo PaymentReturnRepository, cfg PaymentConfig, log *slog.Logger) *PaymentReturnService {
	if log == nil {
		log = slog.Default()
	}
	return &PaymentReturnService{repo: repo, cfg: cfg, log: log, now: time.Now}
}

// returnReasonOf 判一笔订单不认的到账是哪一种。
func returnReasonOf(order repository.Order, amountCents int64) int16 {
	switch {
	case order.PaidAt != nil:
		return repository.PaymentReturnDuplicate // 订单已由另一笔入账
	case order.Status == orderStatusPending && amountCents != order.PayableCents:
		return repository.PaymentReturnAmountMismatch
	default:
		return repository.PaymentReturnOrderClosed
	}
}

// openReturn 在调用方的事务里给一笔到账开退回单（payment.go 的 settle 用）。返回单号；已有时返回空串。
func openReturn(ctx context.Context, tx repository.Tx, paymentID int64, reason int16, now time.Time) (string, error) {
	no, err := newReturnNo(now)
	if err != nil {
		return "", err
	}
	ok, err := tx.InsertPaymentReturn(ctx, no, paymentID, reason)
	if err != nil || !ok {
		return "", err
	}
	return no, nil
}

// Submit 把一张待提交（10）的退回单交给渠道。沙箱：当场模拟渠道退款并入账（→ 40）；真实渠道：→ 30 等回调。
// 渠道侧失败（没配回调密钥）记在单上、留在 10，下一轮扫描重试。不是 10 的单什么都不做。
func (s *PaymentReturnService) Submit(ctx context.Context, returnNo string) error {
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		r, err := tx.LockPaymentReturnByNo(ctx, returnNo)
		if err != nil {
			return err
		}
		if r.Status != repository.PaymentReturnPending {
			return nil
		}
		if !s.cfg.Sandbox {
			s.log.WarnContext(ctx, "多收款退回单已提交；真实退款渠道尚未对接，等渠道回调", "return_no", returnNo)
			return tx.MarkPaymentReturnSubmitted(ctx, r.ID)
		}
		channel, ok := refundChannelNames[r.Channel]
		if !ok {
			return tx.MarkPaymentReturnAttemptFailed(ctx, r.ID, fmt.Sprintf("渠道 %d 不支持原路退回", r.Channel))
		}
		// 走 tx 而不是 s.repo：已经在事务里，再去池上拿第二条连接会让并发的提交把池占死。
		secret, err := tx.ChannelNotifySecret(ctx, channel)
		if err != nil {
			return err
		}
		if secret == "" {
			return tx.MarkPaymentReturnAttemptFailed(ctx, r.ID, "这家店没配该渠道的回调密钥，沙箱渠道造不出可验签的退款回调")
		}
		txn, err := newSandboxRefundID(s.now())
		if err != nil {
			return err
		}
		body, err := json.Marshal(refundNotification{RefundNo: r.ReturnNo, ChannelRefundID: txn, AmountCents: r.AmountCents})
		if err != nil {
			return err
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if err := verifyChannelSignature(ctx, tx, s.log, channel, body, hex.EncodeToString(mac.Sum(nil))); err != nil {
			return err
		}
		_, err = s.settleTx(ctx, tx, r, r.Channel, refundNotification{RefundNo: r.ReturnNo, ChannelRefundID: txn,
			AmountCents: r.AmountCents}, body)
		return err
	})
}

// Notify 处理一次退回单的渠道回调（RefundService.Notify 按单号前缀分派过来，验签已做）。
// 返回的 outcome 与售后退款同一个约定：非空表示「报文收下了但我们不认」（金额不符）。
func (s *PaymentReturnService) Notify(ctx context.Context, channel int16, n refundNotification, rawBody []byte) error {
	var outcome error
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		r, err := tx.LockPaymentReturnByNo(ctx, n.RefundNo)
		if errors.Is(err, repository.ErrPaymentReturnNotFound) {
			return fmt.Errorf("%w: %s", ErrRefundWebhookUnknown, n.RefundNo)
		}
		if err != nil {
			return err
		}
		outcome, err = s.settleTx(ctx, tx, r, channel, n, rawBody)
		return err
	})
	if errors.Is(err, repository.ErrDuplicateChannelRefund) {
		return fmt.Errorf("%w: %s", ErrWebhookDuplicate, n.ChannelRefundID)
	}
	if err != nil {
		return err
	}
	return outcome
}

// settleTx 记一张退回单退回成功。已是 40 的：同一个渠道流水号当作重放（nil），别的流水号是重复退款（报错，回滚）。
func (s *PaymentReturnService) settleTx(ctx context.Context, tx repository.Tx, r repository.PaymentReturn, channel int16,
	n refundNotification, rawBody []byte) (outcome error, err error) {
	if channel != r.Channel {
		return nil, fmt.Errorf("%w: 退回单 %s 走的是渠道 %d，回调来自渠道 %d", ErrWebhookBadPayload, r.ReturnNo, r.Channel, channel)
	}
	if r.Status == repository.PaymentReturnReturned {
		if r.ChannelRefundID != nil && *r.ChannelRefundID == n.ChannelRefundID {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: 退回单 %s 已退回过（%v），又来一笔 %s", ErrWebhookDuplicate, r.ReturnNo,
			r.ChannelRefundID, n.ChannelRefundID)
	}
	if n.AmountCents != r.AmountCents {
		s.log.ErrorContext(ctx, "多收款退回的渠道回调金额对不上，不认这笔回调，退回单状态不动",
			"return_no", r.ReturnNo, "want", r.AmountCents, "got", n.AmountCents)
		return fmt.Errorf("%w: 退回单 %s 应退 %d，渠道回报 %d", ErrWebhookAmountMismatch, r.ReturnNo, r.AmountCents,
			n.AmountCents), nil
	}
	if _, err := tx.SettlePaymentReturn(ctx, r.ID, n.ChannelRefundID, rawBody); err != nil {
		return nil, err
	}
	s.log.InfoContext(ctx, "多收款已原路退回", "return_no", r.ReturnNo, "order_no", r.OrderNo, "amount_cents", r.AmountCents)
	return nil, nil
}

// SweepOnce 逐家店扫一轮：补开退回单（兜底），再提交所有待提交的。返回本轮新开的单数。
func (s *PaymentReturnService) SweepOnce(ctx context.Context) (int, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return 0, err
	}
	opened := 0
	for _, m := range merchants {
		if ctx.Err() != nil {
			return opened, ctx.Err()
		}
		n, err := s.SweepMerchant(tenant.NewContext(ctx, m))
		if err != nil {
			s.log.ErrorContext(ctx, "这家店的多收款退回扫描出错", "merchant_id", m, "err", err)
		}
		opened += n
	}
	return opened, nil
}

// SweepMerchant 扫一家店（ctx 已带租户）。导出给测试：全库扫会碰到别的测试的店。
func (s *PaymentReturnService) SweepMerchant(ctx context.Context) (int, error) {
	opened := 0
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		ps, err := tx.ListUnacceptedPayments(ctx, 100)
		if err != nil {
			return err
		}
		for _, p := range ps {
			reason := repository.PaymentReturnOrderClosed
			switch {
			case p.OrderPaid:
				reason = repository.PaymentReturnDuplicate
			case p.OrderStatus == orderStatusPending && p.AmountCents != p.PayableCents:
				reason = repository.PaymentReturnAmountMismatch
			case p.OrderStatus == orderStatusPending:
				// 待支付、金额也对，却没入账：只可能是入账那一段还没提交（与本扫描并发）。不动它，下一轮再看。
				continue
			}
			no, err := openReturn(ctx, tx, p.PaymentID, reason, s.now())
			if err != nil {
				return err
			}
			if no != "" {
				opened++
				s.log.WarnContext(ctx, "兜底扫描发现一笔订单不认的到账，已开多收款退回单", "payment_id", p.PaymentID,
					"return_no", no, "reason", reason)
			}
		}
		return nil
	})
	if err != nil {
		return opened, err
	}
	var pending []string
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		pending, e = tx.ListPaymentReturnsToSubmit(ctx, 100)
		return e
	}); err != nil {
		return opened, err
	}
	for _, no := range pending {
		if err := s.Submit(ctx, no); err != nil {
			s.log.ErrorContext(ctx, "多收款退回提交出错，下一轮重试", "return_no", no, "err", err)
		}
	}
	return opened, nil
}

// Run 每 PaymentReturnInterval 扫一轮，直到 ctx 结束。
func (s *PaymentReturnService) Run(ctx context.Context) {
	t := time.NewTicker(PaymentReturnInterval)
	defer t.Stop()
	for {
		if n, err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "多收款退回扫描出错", "err", err)
		} else if n > 0 {
			s.log.WarnContext(ctx, "多收款退回扫描补开了退回单", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PaymentReturnPage 是后台列表的一页。
type PaymentReturnPage struct {
	Items    []repository.PaymentReturn
	Total    int64
	Page     int
	PageSize int
}

// List 实现 GET /admin/payment-returns：全店范围的员工（资金面，同券）。
func (s *PaymentReturnService) List(ctx context.Context, status *int16, page, pageSize int) (PaymentReturnPage, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return PaymentReturnPage{}, err
	}
	page, pageSize = clampPaging(page, pageSize)
	out := PaymentReturnPage{Page: page, PageSize: pageSize}
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		out.Items, out.Total, e = tx.ListPaymentReturns(ctx, status, int32(pageSize), int32((page-1)*pageSize))
		return e
	})
	return out, err
}

// isPaymentReturnNo：渠道回调里的单号是不是退回单。
func isPaymentReturnNo(no string) bool { return strings.HasPrefix(no, PaymentReturnNoPrefix) }

// newReturnNo：PR + UTC 时刻 + 随机十六进制（与售后退款单号同一个形状，只多一个前缀）。
func newReturnNo(now time.Time) (string, error) {
	var b [orderNoRandomBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成退回单号失败: %w", err)
	}
	return PaymentReturnNoPrefix + now.UTC().Format("20060102150405") + hex.EncodeToString(b[:]), nil
}
