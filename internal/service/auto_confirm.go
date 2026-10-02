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

// 自动确认收货定时任务（数据模型 §5「发货的三条规则」之三）。
//
// 发货后 N 天（店铺设置的 auto_confirm_days，默认 7；00059 起在 shop_preferences），
// 仍停在 30 已发货的订单由系统推到 40 已完成。没有它，一个从不点「确认收货」
// 的买家会让订单永远停在 30 —— 而「已完成」是结算、评价、售后时效这些下游
// 事情的起点。
//
// # 和超时关单是同一套机制
//
// 跑法照 sweep.go 一字不改：枚举活跃商家（ActiveMerchants，merchants 没有 RLS），
// 一家一家进 WithTenant；每轮「每租户上限 + 总预算 + 兜底 + 轮转起点」
// 的公平调度就是 sweep.go 的 fairRound，两个任务共用一份。
// 数据模型 §5 原文说「挂在 jobs 上」，与超时关单同一处出入 —— 本仓库没有以平台
// 身份出队的 worker（sweep.go 文件头「这与数据模型 §12 有一处冲突」那一段），
// 所以按租户扫描，文档一并改了。
//
// # 走的是买家确认收货同一条迁移
//
// 处置一笔 = repository.ConfirmOrderReceipt(order_no, user_id)，也就是
// POST /orders/{order_no}/confirm 用的那一条条件 UPDATE（30 → 40，写 finished_at）。
// 自动确认就是「系统替买家点了确认收货」，状态迁移与副作用只有一份：
// 哪天确认收货多了一件副作用（发积分、开放评价），两条路一起有。
// 不走买家那个服务方法本身，是因为它要一个买家身份与幂等键 —— 定时任务两样都没有，
// 也不该有（伪造一个买家身份进 ctx，等于让「这是谁做的」在审计上说谎）。
//
// # 有在途售后的单：暂停，不确认
//
// 订单有一张 10 待审核 / 20 待买家退货 / 30 退款中 的退款单时跳过它。
// 买家正在走售后（典型：退货退款，货在寄回的路上），系统替他点「确认收货」
// 等于替他说「货没问题」—— 契约里确认收货不被部分退款阻断，那说的是**买家自己**
// 可以选择确认，不是系统可以替他选。
//
// 暂停而不是顺延：退款单结束（驳回、撤回、退完）之后，这一单在下一轮就会被确认，
// 因为它的发货时间早就过了 N 天。这是保守那一侧的简化 —— 真正的顺延
// （「售后结束后再给 N 天」）要一列记下暂停了多久，而一期的量级不值得为它加列。
// 代价写清楚：一张被驳回的退款单结束后，订单最迟一轮之内就会被自动确认，
// 买家没有「重新考虑」的余量。
//
// 判断做两次：扫描时 NOT EXISTS 预筛（不把明知要跳过的单占进本轮预算），
// 处置事务里在**订单行锁之下**再查一次。第二次才是裁判：申请退款也先锁订单行
// （db/queries/refunds.sql 的 LockUserOrderByNo），两边因此串行 —— 预筛与处置之间
// 刚插进来的一张退款单，在锁之后的那次读里一定看得见（READ COMMITTED 下每条语句
// 一个新快照）。只靠 UPDATE 里带一个 NOT EXISTS 做不到这一点：被锁挡住之后
// PostgreSQL 只按新版本重算**这一行**的谓词，子查询看的仍是旧快照。
type AutoConfirmService struct {
	repo AutoConfirmRepository
	log  *slog.Logger
	cfg  SweepConfig

	// cursor 是轮转起点，语义与 SweepService.cursor 相同。
	cursor uint64

	now func() time.Time
}

// DefaultAutoConfirmInterval 十分钟一轮。
//
// 天数粒度的规则不需要分钟级的扫描；而扫得太疏，「第 7 天」会变成「第 7 天加半天」。
// 十分钟让偏差可以忽略，扫描走部分索引（00036 的 idx_orders_auto_confirm），空轮很便宜。
const DefaultAutoConfirmInterval = 10 * time.Minute

// AutoConfirmRepository 是这个任务需要的仓储能力：租户事务、活跃商家清单、
// 这家店的自动确认天数（在 shop_preferences 里，自己开一个短的租户事务读，不占扫描的那个 Tx）。
type AutoConfirmRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
	AutoConfirmDays(ctx context.Context) (int, error)
}

// AutoConfirmReport 是一轮的结果。分这么细的理由同 SweepReport。
type AutoConfirmReport struct {
	Tenants int

	// Confirmed 30 → 40 成功的笔数。
	Confirmed int

	// Paused 扫到了，但在行锁之下发现有在途售后，暂停。正常路径。
	Paused int

	// Raced 扫到了但已经不在 30（买家刚自己确认了）。正常路径。
	Raced int

	// Failed 处置时真的出错了。非零就该有人看日志。
	Failed int

	// Reminded 这一轮发出的「自动确认收货即将到期」提醒（到期前一天，数据模型 §16）。
	Reminded int

	Fallback bool
}

// errAutoConfirmPaused：行锁之下发现有在途售后，这一单这一轮不确认。
var errAutoConfirmPaused = errors.New("订单有在途售后，暂停自动确认收货")

// NewAutoConfirmService 建自动确认收货服务。cfg.Interval <= 0 时用
// DefaultAutoConfirmInterval；两个预算的默认值与超时关单相同。
func NewAutoConfirmService(r AutoConfirmRepository, cfg SweepConfig, log *slog.Logger) *AutoConfirmService {
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
		cfg.Interval = DefaultAutoConfirmInterval
	}
	return &AutoConfirmService{repo: r, log: log, cfg: cfg, now: time.Now}
}

// WithClock 换掉时钟，**只给测试用**：「发货第 N 天之前不确认、之后确认」
// 这条边界要能在不等 N 天的情况下被验到。
func (s *AutoConfirmService) WithClock(now func() time.Time) *AutoConfirmService {
	s.now = now
	return s
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。形状与 SweepService.Run 相同：
// 启动后立刻跑一轮，单轮出错不退出。
func (s *AutoConfirmService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.ConfirmOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "自动确认收货这一轮没跑起来", "err", err)
		case rep.Confirmed == 0 && rep.Paused == 0 && rep.Raced == 0 && rep.Failed == 0 && rep.Reminded == 0:
			s.log.DebugContext(ctx, "自动确认收货这一轮没有到期的订单", "tenants", rep.Tenants)
		default:
			s.log.InfoContext(ctx, "自动确认收货完成一轮",
				"tenants", rep.Tenants, "confirmed", rep.Confirmed, "paused", rep.Paused,
				"raced", rep.Raced, "failed", rep.Failed, "reminded", rep.Reminded, "fallback", rep.Fallback)
		}
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "自动确认收货任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

// ConfirmOnce 跑一轮。导出的理由同 SweepOnce：测试要能不等 ticker 驱动它。
func (s *AutoConfirmService) ConfirmOnce(ctx context.Context) (AutoConfirmReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return AutoConfirmReport{}, err
	}
	rep := AutoConfirmReport{Tenants: len(merchants)}
	if len(merchants) == 0 {
		return rep, nil
	}
	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++
	rep.Fallback = fairRound(merchants, start, s.cfg.PerTenantCap, s.cfg.RoundBudget,
		func(merchantID int64, limit int) int {
			return s.confirmTenant(ctx, merchantID, limit, &rep)
		})
	return rep, nil
}

// confirmTenant 处理一家商户至多 limit 笔，返回动过的笔数（含暂停、竞态与失败）。
func (s *AutoConfirmService) confirmTenant(ctx context.Context, merchantID int64, limit int,
	rep *AutoConfirmReport) int {
	if limit <= 0 {
		return 0
	}
	// 租户上下文在这里产生 —— 与 sweepTenant 同一个理由，这是定时任务进入租户世界的门。
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	days, err := s.repo.AutoConfirmDays(tctx)
	if err != nil {
		log.ErrorContext(ctx, "读自动确认收货天数失败", "err", err)
		rep.Failed++
		return 0
	}
	if days <= 0 {
		// chk_auto_confirm_days（00036）拦着，走不到这里。真走到了，宁可不确认，
		// 也不按「发货即确认」去推一批订单 —— 那一步是推不回来的。
		log.ErrorContext(ctx, "自动确认收货天数不是正数，这家店本轮跳过", "days", days)
		rep.Failed++
		return 0
	}
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour)

	var due []repository.AutoConfirmCandidate
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		due, err = tx.ListAutoConfirmableOrders(tctx, cutoff, int32(limit))
		return err
	}); err != nil {
		log.ErrorContext(ctx, "扫描到期的已发货订单失败", "err", err)
		rep.Failed++
		return 0
	}
	for _, o := range due {
		s.confirmOne(tctx, log, o, cutoff, days, rep)
	}
	// 「即将自动确认」的提醒与确认共用这一家的配额：先确认到期的，剩下的给提醒。
	return len(due) + s.remindTenant(tctx, log, days, limit-len(due), rep)
}

// confirmOne 处置一笔：锁订单 → 复核（仍在 30、发货早于截止、没有在途售后）→
// 买家确认收货同一条 UPDATE。一个事务，一单一个（理由同 sweep.go「每一单一个事务」）。
func (s *AutoConfirmService) confirmOne(ctx context.Context, log *slog.Logger,
	o repository.AutoConfirmCandidate, cutoff time.Time, days int, rep *AutoConfirmReport) {
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		order, err := tx.LockOrderByID(ctx, o.ID)
		if err != nil {
			return err
		}
		if order.Status != orderStatusShipped || order.ShippedAt == nil || !order.ShippedAt.Before(cutoff) {
			return fmt.Errorf("%w: 订单 %s 此刻状态 %d", repository.ErrOrderNotClaimed, o.OrderNo, order.Status)
		}
		open, err := tx.OrderHasOpenRefund(ctx, order.ID)
		if err != nil {
			return err
		}
		if open {
			return errAutoConfirmPaused
		}
		if order.Source == repository.OrderSourceChannel {
			// 渠道单（00320）没有 keel 买家：不走买家确认收货那条（谓词里有 user_id），
			// 也不发买家通知（平台自己通知顾客）。
			ok, err := tx.FinishChannelOrder(ctx, order.ID)
			if errors.Is(err, repository.ErrIllegalOrderTransition) {
				ok, err = false, nil
			}
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("渠道单 %s 在行锁之下从 30 推 40 失败", o.OrderNo)
			}
			return nil
		}
		if order.UserID == nil {
			return fmt.Errorf("自营订单 %s 没有买家（chk_order_buyer 应当挡住）", o.OrderNo)
		}
		ok, err := tx.ConfirmOrderReceipt(ctx, order.OrderNo, *order.UserID)
		if errors.Is(err, repository.ErrIllegalOrderTransition) {
			ok, err = false, nil
		}
		if err != nil {
			return err
		}
		if !ok {
			// 持有行锁、刚读到 30，这条 UPDATE 却匹配 0 行：谓词被改坏了。
			return fmt.Errorf("订单 %s 在行锁之下从 30 推 40 失败", o.OrderNo)
		}
		// 通知与 30 → 40 同一个事务（数据模型 §16）。
		return notifyOrderAutoFinished(ctx, tx, order, days)
	})

	switch {
	case err == nil:
		rep.Confirmed++
		log.InfoContext(ctx, "发货已满自动确认天数：已替买家确认收货", "order_no", o.OrderNo)
	case errors.Is(err, errAutoConfirmPaused):
		rep.Paused++
		log.InfoContext(ctx, "订单有在途售后，暂停自动确认收货", "order_no", o.OrderNo)
	case errors.Is(err, repository.ErrOrderNotClaimed):
		rep.Raced++
		log.InfoContext(ctx, "这一单已经不在已发货（多半是买家刚自己确认了），跳过", "order_no", o.OrderNo)
	default:
		rep.Failed++
		log.ErrorContext(ctx, "自动确认收货失败", "order_no", o.OrderNo, "err", err)
	}
}

// remindTenant 给这一家「到期前一天」的已发货订单发「即将自动确认收货」，至多 limit 笔，
// 返回发出的笔数。
//
// 候选是仍停在 30、发货早于 now - (N - 1) 天、没有在途售后、还没提醒过的单
// （db/queries/notifications.sql 的 ListAutoConfirmReminders）。「还没提醒过」靠通知的
// 去重键：提醒一旦写进去，这一单就不再是候选 —— 每十分钟一轮的扫描不会重复提醒。
//
// 一批一个事务：这里只写通知，不动任何业务状态；一条写失败整批回滚，下一轮原样再来，
// 没有「提醒了一半」的中间态需要收拾。
func (s *AutoConfirmService) remindTenant(ctx context.Context, log *slog.Logger, days, limit int,
	rep *AutoConfirmReport) int {
	if limit <= 0 {
		return 0
	}
	remindBefore := s.now().Add(-time.Duration(days-1) * 24 * time.Hour)
	sent := 0
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		due, err := tx.ListAutoConfirmReminders(ctx, remindBefore, int32(limit))
		if err != nil {
			return err
		}
		for _, r := range due {
			if err := notifyAutoConfirmSoon(ctx, tx, r, days); err != nil {
				return err
			}
		}
		sent = len(due)
		return nil
	})
	if err != nil {
		rep.Failed++
		log.ErrorContext(ctx, "发「即将自动确认收货」提醒失败", "err", err)
		return 0
	}
	rep.Reminded += sent
	return sent
}
