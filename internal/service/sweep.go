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

// 超时补偿定时任务（Task 6）。
//
// # 它为什么不是可选项
//
// 下单 SAGA 在**正向阶段**就真实扣减库存（架构 §5 / §7：SAGA 没有预留态，
// 所以也没有 Confirm）。这个取舍的代价写在架构里：**超卖为零、少卖存在** ——
// 用户下单未付、订单关闭前，这批库存对其他人不可售。
//
// 少卖本来是可恢复的，恢复它的就是这个任务。跑不起来，少卖就变成**永久漏卖**：
// 库存被一批永远不会付款的订单锁死，没有任何人会回补它，而水位看上去完全正常。
// M2 计划把这句话写成了「任务 6 不是可选项」。
//
// # 它扫两类行，处置完全不同
//
//	① status = 10 且 expire_at < now()   正常的超时未支付
//	   进过 SAGA，库存**已经真实扣减**。处置 = 关单 + 逐行回补 + 记流水（biz_type 3）。
//
//	② status = 0 且 expire_at < now()    孤儿草稿
//	   进程在「订单落库提交」与「SAGA 提交」之间死掉留下的（00013 文件头那笔
//	   明写的欠账）。它**从没进过 SAGA**：编排是「建单在前、库存在后」
//	   （service/order.go 的文件头），订单还停在 0 说明建单分支的正向没成功，
//	   而库存分支排在它后面，连开始都没开始。处置 = 只关单，**不回补**。
//
// 把 ② 也回补一次的后果不是「多做一次无害的事」：那是一次凭空增加的库存，
// 而且它会留下一行 biz_type = 3 的流水，对账时看起来完全正常。所以两类的处置
// 必须分开写，而不是共用一个「关单并回补」的函数再靠一个布尔开关。
//
// ② 的「不回补」靠的是另一个文件里的一个常量（order_saga.go 的 sagaSteps）。
// 那不是一条能长期自己成立的不变量，所以关单之前有一次点查核对
// （repository.ErrDraftHasInventoryLog）。理由写在那个 sentinel 上。
//
// # 定时任务的租户从哪来
//
// 这是多租户定时任务的核心难点：任务跑在任何 HTTP 请求之外，**没有 Host**，
// 而 tenant.Resolver 吃的正是 Host；也没有 gid，dtm.TenantContextFromGID 也用不上。
//
// 答案是**枚举 merchants**（repository.ActiveMerchants），再一家一家进 WithTenant。
// 它走得通是因为 merchants 是 tenant-root 类，**没有 RLS**，而且刻意没有 ——
// 租户解析本来就要在 SET LOCAL 之前读它。于是这条路不需要任何 RLS 豁免、
// 不需要平台角色、也不需要给 keel_app 一份能绕过 RLS 的凭据（那份凭据是
// db.NewPool 的自检明确拒绝的东西）。
//
// **这与数据模型 §12 有一处冲突，写在这里而不是悄悄绕过。** §12 说「worker 出队
// 以平台身份执行，不走 RLS 注入」——那是 jobs 表用一条 ORDER BY 给出跨租户全局
// 顺序的前提。本仓库没有那个身份。代价是扫描被切成每租户一段，全局顺序没了，
// 于是「谁先被扫」变成一个真实的选择，公平调度也就必须落在应用层。
//
// # 公平调度：照 §12 的「上限 + 兜底」，逐条说清差在哪
//
// §12 的形状是两条 SQL：第一条跳过在途数已达上限的租户；第一条没取到而确实
// 还有待办时，第二条去掉租户限制再取一次。理由那里写得很清楚：
// **公平的目的是防饿死，不是让机器闲着。**
//
// 这里照搬那个形状，三处按本任务的实际形态调整：
//
//	· 「在途上限」→「每轮每租户处理上限」（perTenantCap）。§12 的在途数是
//	  jobs.status = 1 那些行，本任务没有在途态 —— 每一单都在**一个事务里**
//	  占位并处置完，没有中间状态可数。能限的量只剩「这一轮给这家店多少配额」。
//
//	· 兜底那一趟的触发条件：第一趟里**有租户恰好被上限卡住**（取满了 cap），
//	  且本轮预算还没用完。与 §12 的「没取到但确实还有待办」是同一句话的
//	  另一种说法 —— 只剩一个租户有积压时，他不该被自己的上限卡住而 worker 空转。
//
//	· 多了一个 §12 没有的东西：**轮转起点**（cursor）。§12 不需要它，因为它的
//	  出队是一条全局 ORDER BY；这里扫描按租户切段，「谁排第一」是一个真实的
//	  选择，固定不变的话预算永远从 id 最小的那家开始花，尾部的店在预算紧张时
//	  一次也轮不到 —— 那正是公平调度要防的饿死，只不过换了一个发生的位置。
//
// # 每一单一个事务
//
// 不是「一个租户一个大事务」。两条理由，都不是洁癖：
//
//	· 一单处置失败（比如那一行 SKU 在本租户不可见）不该把同一批里已经处理好的
//	  其他单一起回滚。补偿任务最不该有的性质就是「一颗老鼠屎让整批都跑不完」——
//	  那会让少卖永远恢复不了。
//	· 一个持有几十行订单锁的长事务，会把支付回调挡在外面。而支付回调正是这批
//	  订单里随时可能到来的那个对手。
type SweepService struct {
	repo SweepRepository
	log  *slog.Logger
	cfg  SweepConfig

	// cursor 是轮转起点，每跑一轮前进一格。见上面公平调度那一段第三条。
	//
	// 它是纯进程内状态，重启即归零 —— 刻意不持久化：它要防的是「同一个进程
	// 连续跑很多轮时尾部租户一次也轮不到」，而一次重启本身就打断了那个连续。
	// 为它建一张表或加一列，换来的只是重启后少一次随机的偏袒。
	cursor uint64
}

// SweepConfig 是一轮扫描的两个预算。
type SweepConfig struct {
	// PerTenantCap 是每轮每租户至多处理的订单数。<= 0 时用 DefaultPerTenantCap。
	PerTenantCap int

	// RoundBudget 是每轮总共至多处理的订单数。<= 0 时用 DefaultRoundBudget。
	//
	// 有总预算才有公平可言：没有它，「每租户上限」只是把一轮拉得更长，
	// 谁也没被饿死但谁都得等。有了它，上限决定的是这份有限预算怎么分。
	RoundBudget int

	// Interval 是两轮之间的间隔。<= 0 时用 DefaultSweepInterval。
	Interval time.Duration
}

// 三个默认值。
//
// 它们写在这里而不是配置里，理由与 orderExpireIn 一样：本轮还没有 shop_settings
// 上对应的列，而一个假装可配置的常量比一个诚实的常量更糟。
const (
	// DefaultPerTenantCap 一轮里单家商户至多处理 50 笔。
	//
	// 50 是按「一轮一分钟、单机日订单 1 万（架构 §9 的容量目标）」倒推的：
	// 一天 1440 轮 × 50 = 7.2 万笔，对单家商户的超时单是宽裕一个量级的余量。
	DefaultPerTenantCap = 50

	// DefaultRoundBudget 一轮总共至多 500 笔。
	//
	// 它同时是这个任务对数据库的压力上限：500 笔 = 500 个短事务，
	// 每个事务里是一次条件 UPDATE 加几行回补。
	DefaultRoundBudget = 500

	// DefaultSweepInterval 一分钟一轮。
	//
	// 它决定的是「超时之后最久多久库存被放回去」，也就是少卖窗口的尾巴有多长。
	// 订单本身的超时是 30 分钟（orderExpireIn），再多一分钟的粒度可以忽略；
	// 而扫得更密不会更快地放回任何库存，只会更频繁地扫到空。
	DefaultSweepInterval = time.Minute
)

// SweepRepository 是这个任务需要的仓储能力。
//
// 比别的服务多一个 ActiveMerchants —— 那是定时任务拿到租户的唯一入口，
// 理由写在 repository/sweep.go 的文件头。
type SweepRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// SweepReport 是一轮的结果。
//
// 分这么细不是为了好看：`Raced` 与 `Failed` 混在一起的话，「支付回调抢先了」
// （完全正常，每天都会发生）与「库存回补报错了」（少卖恢复不了）会变成同一个
// 数字，而前者的量级远大于后者 —— 后者会被永远淹没。
type SweepReport struct {
	// Tenants 这一轮走过几家商户。
	Tenants int

	// Released 超时未支付：关单 + 库存回补，成功的笔数。
	Released int

	// ReleasedQty 这一轮总共放回去多少件货。**这是这个任务存在的意义本身。**
	ReleasedQty int

	// ClosedDrafts 孤儿草稿：只关单，成功的笔数。
	ClosedDrafts int

	// Raced 扫到了但没占下 —— 支付回调抢先，或另一个实例先处理了。正常路径。
	Raced int

	// Failed 处置时真的出错了。非零就该有人看日志。
	Failed int

	// Fallback 这一轮跑过兜底那一趟（去掉每租户上限）。
	Fallback bool
}

// NewSweepService 建超时补偿服务。
func NewSweepService(r SweepRepository, cfg SweepConfig, log *slog.Logger) *SweepService {
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
		cfg.Interval = DefaultSweepInterval
	}
	return &SweepService{repo: r, log: log, cfg: cfg}
}

// Run 按 Interval 一轮一轮地扫，直到 ctx 被取消。
//
// **第一轮在启动后立刻跑一次**，不等第一个 tick：进程刚重启时，上一次运行留下
// 的超时单可能已经压了很久（崩溃之后尤其如此），再等一个间隔没有任何好处。
//
// 一轮出错不会让 Run 退出：这个任务停掉的代价是永久漏卖，而单轮失败的成因
// （数据库抖了一下）下一轮通常就没有了。错误逐轮记在日志里。
func (s *SweepService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "超时补偿任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

func (s *SweepService) runOnce(ctx context.Context) {
	rep, err := s.SweepOnce(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "超时补偿这一轮没跑起来", "err", err)
		return
	}
	if rep.Released == 0 && rep.ClosedDrafts == 0 && rep.Raced == 0 && rep.Failed == 0 {
		// 空轮是常态（大多数分钟里没有订单超时），Debug 级别，
		// 否则这条日志会把别的东西淹掉。
		s.log.DebugContext(ctx, "超时补偿这一轮没有可处理的订单", "tenants", rep.Tenants)
		return
	}
	s.log.InfoContext(ctx, "超时补偿完成一轮",
		"tenants", rep.Tenants, "released", rep.Released, "released_qty", rep.ReleasedQty,
		"closed_drafts", rep.ClosedDrafts, "raced", rep.Raced,
		"failed", rep.Failed, "fallback", rep.Fallback)
}

// SweepOnce 跑一轮。导出是为了让测试能在不等 ticker 的情况下驱动它 ——
// 用 sleep 去测一个一分钟一轮的任务，要么测试慢一分钟，要么间隔短到不反映真实配置。
func (s *SweepService) SweepOnce(ctx context.Context) (SweepReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return SweepReport{}, err
	}
	rep := SweepReport{Tenants: len(merchants)}
	if len(merchants) == 0 {
		return rep, nil
	}

	// 轮转起点。见文件头公平调度那一段第三条。
	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++

	rep.Fallback = fairRound(merchants, start, s.cfg.PerTenantCap, s.cfg.RoundBudget,
		func(merchantID int64, limit int) int {
			return s.sweepTenant(ctx, merchantID, limit, &rep)
		})
	return rep, nil
}

// fairRound 跑一轮公平调度：两趟、每租户上限、总预算、轮转起点（文件头那三条）。
//
// work(merchantID, limit) 处理一家至多 limit 笔，返回**动过的**笔数（含竞态与失败，
// 理由见 sweepTenant）。返回值是这一轮有没有跑兜底那一趟。
//
// 超时关单与自动确认收货（auto_confirm.go）共用它。两个任务的「处置一笔」完全不同，
// 而「这一轮先给谁、给多少、谁被卡住了要不要再扫一趟」是同一个问题 ——
// 写成两份的话，哪天有人只在其中一份里修了一个饿死问题，另一个任务照旧饿着。
func fairRound(merchants []int64, start, perTenantCap, budget int, work func(int64, int) int) bool {
	// 第一趟：每家至多 perTenantCap。
	capped := false
	for i := 0; i < len(merchants) && budget > 0; i++ {
		take := min(perTenantCap, budget)
		done := work(merchants[(start+i)%len(merchants)], take)
		budget -= done
		if done >= take {
			// 这家取满了配额，说明它**可能**还有积压。记下来，兜底那一趟据此触发。
			capped = true
		}
	}

	// 第二趟（兜底）：预算没用完、而第一趟有人被上限卡住。
	//
	// §12 的原话：「只有第一条的话，系统里只剩一个租户有任务时，他会被自己的
	// 上限卡住，而 worker 全都空转。公平的目的是防饿死，不是让机器闲着。」
	//
	// 两个条件缺一不可。少了 capped，一轮里所有店都没积压时也会白扫第二遍；
	// 少了 budget > 0，兜底会把总预算这道闸门整个绕开，而那道闸门是这个任务
	// 对数据库的压力上限。
	if !capped || budget <= 0 {
		return false
	}
	for i := 0; i < len(merchants) && budget > 0; i++ {
		budget -= work(merchants[(start+i)%len(merchants)], budget)
	}
	return true
}

// sweepTenant 处理一家商户至多 limit 笔，返回**真的动过**的笔数（含竞态与失败）。
//
// 返回「动过的笔数」而不是「成功的笔数」，是为了让预算扣得准：一笔被支付回调
// 抢走的订单同样花掉了一次事务，不把它算进预算的话，一个高并发的租户能靠竞态
// 把一轮的实际工作量撑到预算的好几倍。
func (s *SweepService) sweepTenant(ctx context.Context, merchantID int64, limit int, rep *SweepReport) int {
	if limit <= 0 {
		return 0
	}
	// 租户上下文在这里产生，也只在这里产生。
	//
	// 它是这个文件里唯一一处 tenant.NewContext —— 定时任务没有 Host 也没有 gid，
	// 所以这一处是它进入租户世界的门。往下每一次读写都经 WithTenant，
	// 而 WithTenant 只从 ctx 取租户（调用方没有那个参数可以传错）。
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	var pending, drafts []repository.ExpiredOrder
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		if pending, err = tx.ListExpiredPendingOrders(ctx, int32(limit)); err != nil {
			return err
		}
		// 两类共用一份配额，超时未支付优先 —— 它压着真实的库存，
		// 而孤儿草稿只是一行不占任何东西的垃圾。
		rest := limit - len(pending)
		if rest <= 0 {
			return nil
		}
		drafts, err = tx.ListExpiredDraftOrders(ctx, int32(rest))
		return err
	}); err != nil {
		log.ErrorContext(ctx, "扫描超时订单失败", "err", err)
		rep.Failed++
		return 0
	}

	touched := 0
	for _, o := range pending {
		s.releasePending(tctx, log, o, rep)
		touched++
	}
	for _, o := range drafts {
		s.closeDraft(tctx, log, o, rep)
		touched++
	}
	return touched
}

// releasePending 处置第一类：关单 + 回补 + 记流水，**一个事务**。
//
// 关单那条 UPDATE 带着 `status = 10`（见 db/queries/orders.sql 的
// ClaimExpiredPendingOrder），它同时是占位：返回 1 才算这一单归我。这就是
// 「超时任务与支付回调撞车」那条竞态的全部处理 —— 不在应用层加锁，
// 让数据库回答「谁赢了」，输的那一边什么也不做。
//
// 顺序是「先关单再回补」而不是反过来。在同一个事务里两者原子，所以顺序不影响
// 结果，但它影响**冲突窗口**：先发那条带 status = 10 的 UPDATE，这一行的写锁
// 立刻拿到手，支付回调那条同样带 status = 10 的 UPDATE 会在锁上排队，
// 等我们提交后看到 status = 90 而匹配 0 行。反过来先做几次库存回补，
// 订单那一行在这段时间里是不加锁的，窗口白白拉长。
func (s *SweepService) releasePending(ctx context.Context, log *slog.Logger,
	o repository.ExpiredOrder, rep *SweepReport) {
	qty := 0
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.ClaimExpiredPendingOrder(ctx, o.OrderNo); err != nil {
			return err
		}
		var err error
		qty, err = releaseClosedOrder(ctx, tx, o.ID, o.OrderNo, o.StoreID,
			repository.InventoryLogTimeoutRelease)
		if err != nil {
			return err
		}
		// 通知与关单同一个事务（数据模型 §16）。
		return notifyOrderTimeoutClosed(ctx, tx, o.OrderNo)
	})

	switch {
	case err == nil:
		rep.Released++
		rep.ReleasedQty += qty
		log.InfoContext(ctx, "超时未支付：已关单并回补库存",
			"order_no", o.OrderNo, "restored_qty", qty)

	case errors.Is(err, repository.ErrOrderNotClaimed):
		// **正常路径。** 支付回调在扫描与处置之间把这一单推到了 20，
		// 或者另一个实例先处理了。什么都不做是对的 —— 一件货都没回补，
		// 因为整个事务回滚了。
		rep.Raced++
		log.InfoContext(ctx, "超时未支付：这一单已被别人处理（多半是支付回调抢先），跳过",
			"order_no", o.OrderNo)

	case errors.Is(err, repository.ErrSKUNotInTenant):
		// 不是业务分支，是 bug 或数据坏了。这一单的库存回补不了，
		// 少卖在这一单上恢复不了，值得一条 Error。
		rep.Failed++
		log.ErrorContext(ctx, "超时未支付：回补时发现 SKU 在本租户不可见 —— "+
			"这一单的库存放不回去了", "order_no", o.OrderNo, "err", err)

	default:
		rep.Failed++
		log.ErrorContext(ctx, "超时未支付：处置失败，这一单的库存还锁着",
			"order_no", o.OrderNo, "err", err)
	}
}

// closeDraft 处置第二类：只关单，**不回补**。
//
// 核对在前、关单在后，两者同一个事务：核对不过时关单跟着回滚，于是这一单
// 每一轮都会再被扫到、再报一次警，而不是被关掉之后线索一起消失。
func (s *SweepService) closeDraft(ctx context.Context, log *slog.Logger,
	o repository.ExpiredOrder, rep *SweepReport) {
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if err := tx.AssertNoInventoryLog(ctx, o.OrderNo); err != nil {
			return err
		}
		return tx.CloseExpiredDraftOrder(ctx, o.OrderNo)
	})

	switch {
	case err == nil:
		rep.ClosedDrafts++
		log.InfoContext(ctx, "孤儿草稿：已关单（未回补库存，它从没进过 SAGA）",
			"order_no", o.OrderNo)

	case errors.Is(err, repository.ErrOrderNotClaimed):
		// 一个跑了很久才回来的 SAGA 重放刚把它推到 10。它不再是孤儿。
		rep.Raced++
		log.InfoContext(ctx, "孤儿草稿：这一单已经不是草稿了，跳过", "order_no", o.OrderNo)

	case errors.Is(err, repository.ErrDraftHasInventoryLog):
		// **不变量被破坏。** 一笔 status = 0 的订单身上有库存流水，意味着
		// 「建单在前、库存在后」这条编排顺序不再成立 —— 而孤儿清理不回补库存
		// 的全部依据就是它。这一单没有被关掉，所以下一轮还会再报。
		rep.Failed++
		log.ErrorContext(ctx, "孤儿草稿身上有库存流水 —— 编排顺序（建单在前、库存在后）"+
			"被改过？孤儿清理不回补库存的依据不再成立，这一单没有关闭，请人工处理",
			"order_no", o.OrderNo, "err", err)

	default:
		rep.Failed++
		log.ErrorContext(ctx, "孤儿草稿：关单失败", "order_no", o.OrderNo, "err", err)
	}
}

// releaseClosedOrder 把一笔**刚在本事务里从 10 关到 90** 的订单占着的东西放回去：
// 逐行回补库存并记流水、把锁着的券退回「未使用」。
//
// 超时关单（releasePending）与买家取消（OrderService.Cancel）共用它 ——
// 两者的差别只在「谁、凭什么把这一单关掉」（那条条件 UPDATE）与流水的 biz_type，
// 关掉之后要放回去的东西一模一样。写成两份的话，哪天有人只给其中一份补上
// 「券也要退」，另一条路径就会静默地把券锁死在一笔已关闭的订单上
// （00026 之前超时关单正是这么漏过券的）。
//
// 调用方必须保证：① 已经在**同一个事务**里把这一单从 10 推到了 90（占位成功）；
// ② 这一单进过 SAGA，库存真实扣减过。孤儿草稿（status 0）不满足 ②，
// 走 closeDraft，不走这里。
func releaseClosedOrder(ctx context.Context, tx repository.Tx, orderID int64,
	orderNo string, storeID int64, bizType int16) (int, error) {
	lines, err := tx.ListOrderLines(ctx, orderID)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		// 一笔待支付订单一行都没有，说明它当初就没建全。返回错误会把关单一起
		// 回滚 —— 超时任务那边这一单每一轮都会再被扫到、再报一次，直到有人来看；
		// 买家取消那边是一次 500，同样值得人看。
		return 0, fmt.Errorf("订单 %s 是待支付状态却一行订单项都没有", orderNo)
	}
	qty := 0
	for _, ln := range lines {
		after, err := tx.RestoreInventory(ctx, ln.SKUID, storeID, ln.Quantity)
		if err != nil {
			return 0, err
		}
		if err := tx.AppendInventoryLog(ctx, ln.SKUID, storeID, ln.Quantity,
			bizType, orderNo, after-ln.Quantity, after); err != nil {
			return 0, err
		}
		qty += int(ln.Quantity)
	}
	// 这一单锁着的券退回「未使用」，与关单、回补库存同一个事务（数据模型 §7）。
	// 没挂券的订单受影响 0 行，是正常路径。
	if _, err := tx.UnlockCouponForOrder(ctx, orderID); err != nil {
		return 0, err
	}
	return qty, nil
}
