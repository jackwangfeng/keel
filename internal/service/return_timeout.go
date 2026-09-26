package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 退货超时未寄回自动关闭（数据模型 §11 退款单状态机那条「20 超时未寄回 → 60」）。
//
// 退货退款审核通过后停在 20 待买家退货，等买家把货寄回、填寄回物流。买家一直不寄，
// 这张单就永远停在 20：它占着「在途件数」（refund_items ⋈ refunds.status IN (10,20,30)），
// 这几件从此申请不了新的售后；订单的自动确认收货也因为「有在途售后」一直暂停
// （auto_confirm.go）。状态机里画着 20 → 60 这条边，此前只有买家撤回在走。
//
// # 口径
//
// 从审核通过（refunds.audited_at）起算，超过店铺设置的 return_ship_days 天（默认 7，
// shop_preferences，00059）仍没填寄回物流的，关到 60 已取消。
//
//   - 从审核时间算，不从申请时间算：审核通过那一刻买家才知道要寄，
//     从申请算会把商家审核拖延的时间算到买家头上。
//   - 填过寄回物流的不关，不管填了多久：货已经在路上，「超时」说的是买家没动作，
//     不是物流慢。商家迟迟不确认收货是另一件事，不该由关买家的单来收场。
//   - 天数按**每一轮当时**的店铺设置算（与自动确认同一个口径）：商家把 7 天改成 3 天，
//     审核已过 3 天的单下一轮就关。
//
// # 和自动确认收货是同一套机制
//
// 跑法照 auto_confirm.go：枚举活跃商家、逐家进租户事务，「每租户上限 + 总预算 + 兜底
// + 轮转起点」的公平调度是 sweep.go 的 fairRound。十分钟一轮。
//
// # 走的是买家撤回同一段收尾
//
// 处置一张 = 锁订单 → 锁退款单 → ExpireReturnRefund（20 → 60）→ leaveRefunding
// （整单退款中的订单回 20、资金维度重算）→ 发 refund_return_expired 给买家。
// 状态迁移之后的收尾与买家撤回（RefundService.Cancel）是同一个函数：「退款单没退成
// 之后订单怎么收尾」只有一份。在途件数是聚合出来的（状态离开 20 就不再计入），
// 不需要额外释放。
//
// 锁的顺序与填寄回物流、撤回、确认收到退货相同：先订单、后退款单。买家在扫描之后
// 填了物流，那次提交要么排在我们前面（ExpireReturnRefund 的谓词看见 return_submitted_at
// 已经有值，影响 0 行，记为 Raced），要么排在我们后面（它锁到的是一张已经 60 的单，
// 自己的谓词 status = 20 不成立，买家收到 409）。不存在「货寄出了、单被关了」的交错。
type ReturnTimeoutService struct {
	repo ReturnTimeoutRepository
	log  *slog.Logger
	cfg  SweepConfig

	// cursor 是轮转起点，语义与 SweepService.cursor 相同。
	cursor uint64

	now func() time.Time
}

// DefaultReturnTimeoutInterval 十分钟一轮，理由同 DefaultAutoConfirmInterval。
const DefaultReturnTimeoutInterval = 10 * time.Minute

// ReturnTimeoutRepository 是这个任务需要的仓储能力。
type ReturnTimeoutRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
	ReturnShipDays(ctx context.Context) (int, error)
}

// ReturnTimeoutReport 是一轮的结果。
type ReturnTimeoutReport struct {
	Tenants int
	// Expired 20 → 60 成功的张数。
	Expired int
	// Raced 扫到了，但在行锁之下已经不满足（买家刚填了物流、刚撤回、商家刚确认收货）。正常路径。
	Raced int
	// Failed 处置时真的出错了。非零就该有人看日志。
	Failed   int
	Fallback bool
}

// NewReturnTimeoutService 建退货超时服务。cfg.Interval <= 0 时用 DefaultReturnTimeoutInterval。
func NewReturnTimeoutService(r ReturnTimeoutRepository, cfg SweepConfig, log *slog.Logger) *ReturnTimeoutService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.PerTenantCap <= 0 {
		cfg.PerTenantCap = DefaultPerTenantCap
	}
	if cfg.RoundBudget <= 0 {
		cfg.RoundBudget = DefaultRoundBudget
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultReturnTimeoutInterval
	}
	return &ReturnTimeoutService{repo: r, log: log, cfg: cfg, now: time.Now}
}

// WithClock 换掉时钟，**只给测试用**：「审核后第 N 天之前不关、之后关」这条边界要能在
// 不等 N 天的情况下被验到。
func (s *ReturnTimeoutService) WithClock(now func() time.Time) *ReturnTimeoutService {
	s.now = now
	return s
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。启动后立刻跑一轮，单轮出错不退出。
func (s *ReturnTimeoutService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.ExpireOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "退货超时关闭这一轮没跑起来", "err", err)
		case rep.Expired == 0 && rep.Raced == 0 && rep.Failed == 0:
			s.log.DebugContext(ctx, "退货超时关闭这一轮没有到期的售后单", "tenants", rep.Tenants)
		default:
			s.log.InfoContext(ctx, "退货超时关闭完成一轮", "tenants", rep.Tenants,
				"expired", rep.Expired, "raced", rep.Raced, "failed", rep.Failed, "fallback", rep.Fallback)
		}
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "退货超时关闭任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

// ExpireOnce 跑一轮。导出的理由同 SweepOnce：测试要能不等 ticker 驱动它。
func (s *ReturnTimeoutService) ExpireOnce(ctx context.Context) (ReturnTimeoutReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return ReturnTimeoutReport{}, err
	}
	rep := ReturnTimeoutReport{Tenants: len(merchants)}
	if len(merchants) == 0 {
		return rep, nil
	}
	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++
	rep.Fallback = fairRound(merchants, start, s.cfg.PerTenantCap, s.cfg.RoundBudget,
		func(merchantID int64, limit int) int {
			return s.expireTenant(ctx, merchantID, limit, &rep)
		})
	return rep, nil
}

// expireTenant 处理一家商户至多 limit 张，返回动过的张数（含竞态与失败）。
func (s *ReturnTimeoutService) expireTenant(ctx context.Context, merchantID int64, limit int,
	rep *ReturnTimeoutReport) int {
	if limit <= 0 {
		return 0
	}
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	days, err := s.repo.ReturnShipDays(tctx)
	if err != nil {
		log.ErrorContext(ctx, "读退货寄回时限失败", "err", err)
		rep.Failed++
		return 0
	}
	if days <= 0 {
		// chk_shop_pref_return_ship_days（00059）拦着，走不到这里。真走到了宁可不关 ——
		// 按「审核即超时」去关一批单，买家连寄的机会都没有，那一步推不回来。
		log.ErrorContext(ctx, "退货寄回时限不是正数，这家店本轮跳过", "days", days)
		rep.Failed++
		return 0
	}
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour)

	var due []repository.ReturnOverdueRefund
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		due, err = tx.ListReturnOverdueRefunds(tctx, cutoff, int32(limit))
		return err
	}); err != nil {
		log.ErrorContext(ctx, "扫描退货超时的售后单失败", "err", err)
		rep.Failed++
		return 0
	}
	for _, r := range due {
		s.expireOne(tctx, log, r, cutoff, days, rep)
	}
	return len(due)
}

// errReturnTimeoutRaced：行锁之下这张单已经不满足关闭条件。
var errReturnTimeoutRaced = errors.New("售后单此刻已不满足退货超时关闭的条件")

// expireOne 处置一张：锁订单 → 锁退款单 → 20 → 60 → 订单收尾 → 通知买家。
// 一个事务，一张一个（理由同 sweep.go「每一单一个事务」）。
func (s *ReturnTimeoutService) expireOne(ctx context.Context, log *slog.Logger,
	r repository.ReturnOverdueRefund, cutoff time.Time, days int, rep *ReturnTimeoutReport) {
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		order, status, err := lockRefund(ctx, tx, repository.Refund{ID: r.ID, OrderID: r.OrderID})
		if err != nil {
			return err
		}
		if status != repository.RefundAwaitingReturn {
			return fmt.Errorf("%w: 售后单 %s 此刻状态 %d", errReturnTimeoutRaced, r.RefundNo, status)
		}
		ok, err := tx.ExpireReturnRefund(ctx, r.ID, cutoff)
		if errors.Is(err, repository.ErrIllegalRefundTransition) {
			ok, err = false, nil
		}
		if err != nil {
			return err
		}
		if !ok {
			// 仍在 20，但谓词不成立：买家刚填了寄回物流（最常见），或审核时间被改过。
			return fmt.Errorf("%w: 售后单 %s 已填寄回物流", errReturnTimeoutRaced, r.RefundNo)
		}
		if err := leaveRefunding(ctx, tx, order); err != nil {
			return err
		}
		// 通知与 20 → 60 同一个事务（数据模型 §16）。
		return notifyRefundReturnExpired(ctx, tx, r.RefundNo, days)
	})

	switch {
	case err == nil:
		rep.Expired++
		log.InfoContext(ctx, "退货审核通过后超时未寄回：已关闭售后单", "refund_no", r.RefundNo, "days", days)
	case errors.Is(err, errReturnTimeoutRaced):
		rep.Raced++
		log.InfoContext(ctx, "这张售后单已经不等寄回了（多半是买家刚填了物流），跳过", "refund_no", r.RefundNo)
	default:
		rep.Failed++
		log.ErrorContext(ctx, "退货超时关闭失败", "refund_no", r.RefundNo, "err", err)
	}
}
