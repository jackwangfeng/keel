package service

// 库存的 outbox：关单释放与退款回补（微服务拆分阶段 1b，docs/电商系统-微服务拆分方案.md
// 「要拆掉的耦合」第 3 节）。
//
// ===========================================================================
// 为什么是 outbox，而不是在 core 事务里直接调库存服务
// ===========================================================================
//
// 拆分前「关单 + 回补库存 + 放配额 + 放限购 + 解锁券 + 通知」是一个本地事务。库存搬走之后，
// 回补与配额在另一个库里，没有一个事务装得下两边。做法是 core 的事务只做 core 的部分，
// **在同一个事务里入队一条任务**（jobs，与通知外发同一个队列机制），任务调库存服务：
//
//	关单 / 取消   inventory.release  job_key = release:<订单号>   → ReleaseForOrder（按订单号幂等）
//	退款到账      inventory.release  job_key = restock:<退款单号> → RestockForRefund（按退款单号幂等）
//
// 任务与状态变化同生共死：关单提交了，放回库存这件事就一定会被做（至少一次，库存服务那一侧只生效一次）；
// 关单回滚了，任务也不存在。单体形态下也走同一条路 —— 库存服务是进程内实现，差别只在一次函数调用。
//
// ===========================================================================
// 提交之后就地跑一次
// ===========================================================================
//
// 任务入队之后，发起关单的那段代码在**事务提交之后**立刻按 job_key 把它占下来跑一遍（kick）：
// 占下、调库存服务、成功就标完成、失败就按封顶退避放回去 —— 与 worker 走同一个状态机，只是不等
// 下一次轮询。于是正常情况下关单与放回之间只隔几毫秒；库存服务不在时（拆分形态），关单照样成功，
// 放回留给 worker 重试到它回来为止。那段窗口里库存看起来还被占着 —— 方向是「少卖」，不是「超卖」。
//
// 就地跑必须在事务**之外**：单体形态下库存池就是业务池，攥着一条业务连接再去要一条是整池互等；
// 拆分形态下是攥着连接等一次网络往返（与阶段 1a 同一条规矩，inventory_admin.go 的文件头）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/worker"
)

// QueueInventoryRelease 是库存 outbox 的队列名（jobs.queue）。
const QueueInventoryRelease = "inventory.release"

const (
	// inventoryJobMaxAttempts 与 inventoryJobMaxBackoff 合起来决定「库存服务不在时，放回库存这件事
	// 最久等多久才进死信」：前八次按 2、4 …… 256 秒退避，之后每 5 分钟一次，200 次约 16 小时。
	// 进死信意味着永久少卖（有 Error 日志），所以这个数宁大勿小。
	inventoryJobMaxAttempts = 200
	inventoryJobMaxBackoff  = 5 * time.Minute

	inventoryJobStuckAfter = 5 * time.Minute
	inventoryJobRetention  = 7 * 24 * time.Hour
	inventoryJobPurge      = 1000

	jobKindOrderRelease  = "order_release"
	jobKindRefundRestock = "refund_restock"
	// jobKindStockFlagSeed：新门店 / 新商品的有货排序标记当场没种上（库存服务不在），重试到它回来（stock_flags.go）。
	jobKindStockFlagSeed = "stock_flag_seed"
)

// inventoryJob 是任务的载荷：只放定位业务对象的最小标识（00022 的约定）。
// 订单的门店与行、退款单的行在执行时回 core 的库里读 —— 它们在下单 / 建退款单之后就不再变。
type inventoryJob struct {
	Kind     string `json:"kind"`
	OrderNo  string `json:"order_no,omitempty"`
	BizType  int16  `json:"biz_type,omitempty"`
	RefundNo string `json:"refund_no,omitempty"`
	// StoreIDs / ProductIDs 是 jobKindStockFlagSeed 的范围，nil 即全部（refreshStockFlags 的约定）。
	StoreIDs   []int64 `json:"store_ids,omitempty"`
	ProductIDs []int64 `json:"product_ids,omitempty"`
}

func releaseJobKey(orderNo string) string  { return "release:" + orderNo }
func restockJobKey(refundNo string) string { return "restock:" + refundNo }

// enqueueOrderRelease 在关单的那个事务里入队「放回这一单的库存与活动配额」。
func enqueueOrderRelease(ctx context.Context, tx repository.Tx, orderNo string, bizType int16) error {
	return enqueueInventoryJob(ctx, tx, releaseJobKey(orderNo),
		inventoryJob{Kind: jobKindOrderRelease, OrderNo: orderNo, BizType: bizType})
}

// enqueueRefundRestock 在退款到账的那个事务里入队「把这张退款单的货加回门店库存」。
func enqueueRefundRestock(ctx context.Context, tx repository.Tx, refundNo string) error {
	return enqueueInventoryJob(ctx, tx, restockJobKey(refundNo),
		inventoryJob{Kind: jobKindRefundRestock, RefundNo: refundNo})
}

func enqueueInventoryJob(ctx context.Context, tx repository.Tx, key string, j inventoryJob) error {
	payload, err := json.Marshal(j)
	if err != nil {
		return err
	}
	// 返回 false（同一个 job_key 已经在队列里）是正常的：同一张单只会被关一次，
	// 真撞上了说明前一条还没做完，它做的就是同一件事。
	_, err = tx.EnqueueJob(ctx, repository.NewJob{
		Queue: QueueInventoryRelease, JobKey: key, Payload: payload, MaxAttempts: inventoryJobMaxAttempts,
	})
	return err
}

// outboxRepo 是库存 outbox 需要的仓储能力（*repository.Repo 满足它）。
type outboxRepo interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ClaimJobByKey(ctx context.Context, merchantID int64, queue, jobKey, workerID string) (repository.Job, bool, error)
	FinishJobs(ctx context.Context, ids []int64) error
	RetryJobCapped(ctx context.Context, id int64, reason string, maxBackoff time.Duration) error
}

// inventoryOutbox 执行库存任务：就地（kick）与 worker（InventoryOutboxService）共用。
type inventoryOutbox struct {
	repo     outboxRepo
	inv      inventory.Service
	log      *slog.Logger
	workerID string
}

// newInventoryOutbox 在 repo 满足 outboxRepo 且给了库存服务时返回一个可用的 outbox；
// 否则返回 nil（就地跑被关掉，任务照样在队列里等 worker —— 测试里的仓储替身走这一支）。
func newInventoryOutbox(r any, inv inventory.Service, log *slog.Logger) *inventoryOutbox {
	or, ok := r.(outboxRepo)
	if !ok || inv == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	host, _ := os.Hostname()
	return &inventoryOutbox{repo: or, inv: inv, log: log,
		workerID: fmt.Sprintf("inventory-outbox@%s:%d", host, os.Getpid())}
}

// kick 在业务事务提交之后把 job_key 那一条任务就地跑一遍（见文件头）。返回放回的件数；
// 任务不在（已经被 worker 取走、或根本没入队）或这一次失败都返回 0 —— 失败的会被 worker 接着重试。
// ctx 必须带着租户（调用方的请求或定时任务的租户上下文）。
func (o *inventoryOutbox) kick(ctx context.Context, key string) int32 {
	if o == nil {
		return 0
	}
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		o.log.ErrorContext(ctx, "库存任务就地执行拿不到租户，留给 worker", "job_key", key, "err", err)
		return 0
	}
	j, ok, err := o.repo.ClaimJobByKey(ctx, merchantID, QueueInventoryRelease, key, o.workerID+"/kick")
	if err != nil {
		o.log.WarnContext(ctx, "库存任务就地占位失败，留给 worker", "job_key", key, "err", err)
		return 0
	}
	if !ok {
		return 0
	}
	qty, _ := o.runClaimed(ctx, j)
	return qty
}

// runClaimed 跑一条已经占下的任务，并按结果标完成或退避放回。返回放回的件数与是否成功。
func (o *inventoryOutbox) runClaimed(ctx context.Context, j repository.Job) (int32, bool) {
	tctx := tenant.NewContext(ctx, j.MerchantID)
	log := o.log.With("merchant_id", j.MerchantID, "job_id", j.ID, "job_key", j.JobKey, "attempt", j.Attempts)
	qty, err := o.run(tctx, j.Payload)
	if err == nil {
		if ferr := o.repo.FinishJobs(ctx, []int64{j.ID}); ferr != nil {
			// 事已经做了（库存服务那一侧按单号幂等），标不上完成只会让它再被跑一次、什么都不改。
			log.WarnContext(ctx, "库存任务做完了但标完成失败（回收任务会接手，重跑无副作用）", "err", ferr)
		}
		return qty, true
	}
	rerr := o.repo.RetryJobCapped(ctx, j.ID, err.Error(), inventoryJobMaxBackoff)
	switch {
	case errors.Is(rerr, repository.ErrJobDeadLettered):
		log.ErrorContext(ctx, "库存任务重试次数用尽，已转死信 —— 这一单的库存没有放回（永久少卖），请人工处理", "err", err)
	case rerr != nil:
		log.ErrorContext(ctx, "库存任务放回队列失败（回收任务会接手）", "err", err, "retry_err", rerr)
	default:
		log.WarnContext(ctx, "库存任务失败，退避重试", "err", err)
	}
	return 0, false
}

// run 执行一条任务的载荷。门店与行从 core 的库里读（任务载荷只有单号）。
func (o *inventoryOutbox) run(ctx context.Context, payload []byte) (int32, error) {
	var j inventoryJob
	if err := json.Unmarshal(payload, &j); err != nil {
		return 0, fmt.Errorf("库存任务的载荷解不开: %w", err)
	}
	switch j.Kind {
	case jobKindOrderRelease:
		var req inventory.ReleaseRequest
		err := o.repo.WithTenant(ctx, func(tx repository.Tx) error {
			order, err := tx.FindOrderByNo(ctx, j.OrderNo)
			if err != nil {
				return err
			}
			lines, err := tx.ListOrderLines(ctx, order.ID)
			if err != nil {
				return err
			}
			req = inventory.ReleaseRequest{OrderNo: order.OrderNo, StoreID: order.StoreID,
				BizType: j.BizType, Lines: orderLinesOf(lines)}
			return nil
		})
		if err != nil {
			return 0, err
		}
		res, err := o.inv.ReleaseForOrder(ctx, req)
		return res.Qty, err
	case jobKindRefundRestock:
		var req inventory.RestockRequest
		err := o.repo.WithTenant(ctx, func(tx repository.Tx) error {
			r, err := tx.FindRefundByNo(ctx, j.RefundNo)
			if err != nil {
				return err
			}
			order, err := tx.FindOrderByNo(ctx, r.OrderNo)
			if err != nil {
				return err
			}
			// 每一行按哪个活动价成交：未发货的退款把那部分活动配额一起放回（库存服务那边），
			// 否则货回到门店库存了、活动的「已售」却还算着它，配额凭空少掉（2026-09-28 破坏性测试遗留）。
			// 每人限购不放回（在 core 的 promotion_purchases，这里不碰）：防「特价买了退、退了再买」。
			// 自营单里同一个 SKU 至多一行（pricing 拒重复），按 SKU 对上；渠道单同一个 SKU 可以占几行（都没有活动），
			// 退款行按 SKU 并起来再给库存服务。
			ol, err := tx.ListOrderLines(ctx, order.ID)
			if err != nil {
				return err
			}
			promo := make(map[int64]*int64, len(ol))
			for _, l := range ol {
				promo[l.SKUID] = l.PricePromotionID
			}
			req = inventory.RestockRequest{RefundNo: r.RefundNo, StoreID: order.StoreID}
			for _, it := range r.Items {
				req.Lines = append(req.Lines, inventory.OrderLine{SKUID: it.SKUID, Qty: it.Quantity, PromotionID: promo[it.SKUID]})
			}
			req.Lines = mergeSKULines(req.Lines)
			return nil
		})
		if err != nil {
			return 0, err
		}
		res, err := o.inv.RestockForRefund(ctx, req)
		return res.Qty, err
	case jobKindStockFlagSeed:
		// 按库存服务当前水位重算那一批（读 → 写 → 复读核对），重跑无副作用。
		return 0, refreshStockFlags(ctx, o.repo, o.inv, j.StoreIDs, j.ProductIDs)
	default:
		return 0, fmt.Errorf("不认识的库存任务 %q", j.Kind)
	}
}

// orderLinesOf 把订单行变成库存服务的行（扣减载荷与关单释放共用）。同一个 SKU 的几行并成一行（mergeSKULines）。
func orderLinesOf(lines []repository.OrderLine) []inventory.OrderLine {
	out := make([]inventory.OrderLine, 0, len(lines))
	for _, ln := range lines {
		out = append(out, inventory.OrderLine{SKUID: ln.SKUID, Qty: ln.Quantity, PromotionID: ln.PricePromotionID})
	}
	return mergeSKULines(out)
}

// mergeSKULines 把同一个 SKU 的几行并成一行（件数相加，活动取第一行的），保持首次出现的顺序。
// 库存服务拒收同一个 SKU 出现两次的请求（inventory.normalizeLines）；自营单一个 SKU 至多一行（pricing 拒重复），
// 这里什么都不变；渠道单照平台的行建，同一个变体可以占两行（价不同 / 分开加购，第三期审查修复）。
func mergeSKULines(lines []inventory.OrderLine) []inventory.OrderLine {
	at := make(map[int64]int, len(lines))
	out := make([]inventory.OrderLine, 0, len(lines))
	for _, ln := range lines {
		if i, ok := at[ln.SKUID]; ok {
			out[i].Qty += ln.Qty
			continue
		}
		at[ln.SKUID] = len(out)
		out = append(out, ln)
	}
	return out
}

// ---------------------------------------------------------------------------
// worker
// ---------------------------------------------------------------------------

// InventoryOutboxRepository 是 worker 需要的仓储能力。
type InventoryOutboxRepository interface {
	outboxRepo
	DequeueJobs(ctx context.Context, req repository.DequeueRequest) ([]repository.Job, error)
	ReapStuckJobs(ctx context.Context, queue string, olderThan time.Duration) (int64, error)
	PurgeFinishedJobs(ctx context.Context, queue string, retain time.Duration, limit int) (int64, error)
}

// InventoryOutboxConfig 是 worker 的参数，零值即默认。
type InventoryOutboxConfig struct {
	BatchSize         int
	PerTenantInflight int
	PollInterval      time.Duration
	HousekeepInterval time.Duration
}

// InventoryOutboxReport 是一批的结果。
type InventoryOutboxReport struct {
	Jobs    int
	Done    int
	Retried int
	Qty     int
}

// InventoryOutboxService 消费 inventory.release 队列（关单释放、退款回补）。
type InventoryOutboxService struct {
	repo InventoryOutboxRepository
	ob   *inventoryOutbox
	cfg  InventoryOutboxConfig
	log  *slog.Logger
}

// NewInventoryOutboxService 建 worker。inv 是这个进程的库存服务（单体进程内、core 远端）。
func NewInventoryOutboxService(r InventoryOutboxRepository, inv inventory.Service,
	cfg InventoryOutboxConfig, log *slog.Logger) *InventoryOutboxService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 20
	}
	if cfg.PerTenantInflight <= 0 {
		cfg.PerTenantInflight = cfg.BatchSize
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.HousekeepInterval <= 0 {
		cfg.HousekeepInterval = time.Minute
	}
	return &InventoryOutboxService{repo: r, ob: newInventoryOutbox(r, inv, log), cfg: cfg, log: log}
}

// Run 一直消费到 ctx 取消。
func (s *InventoryOutboxService) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 任务内部另起的 goroutine 接不住 Runner 的 recover，这里自己套一层（worker.Supervise）。
		worker.Supervise(ctx, s.log, "inventory_outbox.housekeep", 0, 0, func(ctx context.Context) {
			t := time.NewTicker(s.cfg.HousekeepInterval)
			defer t.Stop()
			for {
				s.housekeep(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		})
	}()
	for {
		rep, err := s.WorkOnce(ctx)
		if err != nil {
			s.log.ErrorContext(ctx, "库存任务这一批没跑起来", "err", err)
		}
		if err == nil && rep.Jobs > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func (s *InventoryOutboxService) housekeep(ctx context.Context) {
	if n, err := s.repo.ReapStuckJobs(ctx, QueueInventoryRelease, inventoryJobStuckAfter); err != nil {
		s.log.ErrorContext(ctx, "回收卡死的库存任务失败", "err", err)
	} else if n > 0 {
		s.log.WarnContext(ctx, "回收了卡在执行中的库存任务", "count", n)
	}
	if _, err := s.repo.PurgeFinishedJobs(ctx, QueueInventoryRelease, inventoryJobRetention, inventoryJobPurge); err != nil {
		s.log.ErrorContext(ctx, "清理过期的库存任务失败", "err", err)
	}
}

// WorkOnce 取一批到期的任务跑完。导出给测试驱动（不等轮询）。
func (s *InventoryOutboxService) WorkOnce(ctx context.Context) (InventoryOutboxReport, error) {
	jobs, err := s.repo.DequeueJobs(ctx, repository.DequeueRequest{
		Queue: QueueInventoryRelease, Limit: s.cfg.BatchSize,
		PerTenantInflight: s.cfg.PerTenantInflight, WorkerID: s.ob.workerID,
	})
	if err != nil || len(jobs) == 0 {
		return InventoryOutboxReport{}, err
	}
	rep := InventoryOutboxReport{Jobs: len(jobs)}
	for _, j := range jobs {
		qty, ok := s.ob.runClaimed(ctx, j)
		if ok {
			rep.Done++
			rep.Qty += int(qty)
		} else {
			rep.Retried++
		}
	}
	return rep, nil
}

// Drain 一直跑到队列里没有到期的任务（测试用）。
func (s *InventoryOutboxService) Drain(ctx context.Context) (InventoryOutboxReport, error) {
	var total InventoryOutboxReport
	for {
		one, err := s.WorkOnce(ctx)
		total.Jobs += one.Jobs
		total.Done += one.Done
		total.Retried += one.Retried
		total.Qty += one.Qty
		if err != nil || one.Jobs == 0 || one.Done == 0 {
			return total, err
		}
	}
}
