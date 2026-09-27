package service

// 库存对账（微服务拆分阶段 2，docs/电商系统-微服务拆分方案.md 1b 条目末尾「留给阶段 2」）。
//
// ===========================================================================
// 为什么要对账
// ===========================================================================
//
// 拆分之后 core 与库存各有一个库，00076 删掉了库存表指向 skus / stores 的外键，
// 活动配额（activity_stocks）与活动商品（promotion_skus）分在两边，关单 / 退款的回补走 outbox。
// 每一处都有自己的一致性论证，但论证覆盖不到的只有三类东西，而它们都**不报错**：
//
//  1. 孤儿库存行：库存库里有、core 里 SKU 或门店根本不存在的 (sku_id, store_id)。
//     拆分之前外键挡着，之后只可能来自数据迁移（split-migrate）或手工改库。无害但说明两边
//     的数据不是同一份 —— 对着的很可能是错的库。SKU / 门店只是软删了的行另算（stale）：
//     软删从来不清库存行（拆分前也是），它们是预期内的遗留，只计数、不告警。
//  2. 活动配额的差集：上线中且未结束的活动，promotion_skus 里有而 activity_stocks 里没有的
//     （计价按「配额未同步」跳过报价、库存服务按配额不足拒绝 —— 活动静默失效，少卖），
//     以及反过来多出来的（不会被命中，但说明整组同步漏了一半）。**配额数不比**：00075 起
//     配额只在库存服务里有一份（promotion_skus.stock_qty 停用、不再写），core 没有可比的数；
//     「已售不超过配额」由 chk_activity_stock_qty 在库存库里兜着。
//  3. 死信的 inventory.release 任务（关单释放 order_release 与退款回补 refund_restock 同一个队列）：
//     200 次、约 16 小时都没做成，意味着那一单的库存永久被占着（少卖）。worker 进死信时打过一条
//     Error，这里每一轮再报一次累计值，直到有人处理。
//
// ===========================================================================
// 只读，不修
// ===========================================================================
//
// 三类都不自动修。孤儿行删不删、配额按哪一边补，要人判断是哪一边错了（迁移漏了、还是 core 删错了）；
// 死信重跑之前要先看 last_error。自动修复的代价是把一次「对着错库」的事故放大成两边一起被改坏。
// 所以它只做一件事：发现了就用 WARN 喊出来，带上前几条的键，并把本轮结果留在 Last() 里。
//
// ===========================================================================
// 跑在哪、多久一次
// ===========================================================================
//
// core 发起（all 与 core 两种角色，inventory 不跑后台任务）：core 看得见自己的 skus / stores /
// promotion_skus / jobs，库存那一半经 inventory.Service 批量问回来（StockKeys 分页、ActivityStock），
// 差集在这里算。两边各自只读自己的表，TestQueryFilesStayOnTheirSideOfTheSplit 不需要任何豁免。
// 一小时一轮：它发现的都是「已经发生、不会自己变好」的东西，频率只决定多久之后有人知道，
// 而每一轮要把全部库存行的键过一遍。按活跃商家逐家进租户事务（RLS），死信是一条跨租户查询
// （jobs 没有 RLS，repository/jobs.go 文件头）。
//
// 瞬态说明：活动上线的那一刻与整组同步之间、后台两步编辑之间，差集可能短暂非空；
// 下一轮还在才是真的。所以单轮的 WARN 是「请看一眼」，不是「一定坏了」。

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// DefaultInventoryReconcileInterval 一小时一轮（文件头第三节）。
const DefaultInventoryReconcileInterval = time.Hour

// 对账发现的几类差异（ReconcileFinding.Kind）。
const (
	ReconcileStockOrphan     = "stock_orphan"     // 库存行的 SKU 或门店在 core 里不存在
	ReconcileStockStale      = "stock_stale"      // 库存行的 SKU 或门店在 core 里已软删（预期内，不告警）
	ReconcileActivityMissing = "activity_missing" // 上线活动的活动商品在库存服务里没有配额行
	ReconcileActivityExtra   = "activity_extra"   // 库存服务里有、上线活动的活动商品里没有
	ReconcileDeadJob         = "dead_job"         // 死信的 inventory.release 任务
)

// InventoryReconcileRepository 是对账需要的仓储能力（*repository.Repo 满足它）。
type InventoryReconcileRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
	DeadJobs(ctx context.Context, queue string, limit int) (map[int64]int64, []repository.DeadJob, error)
}

// InventoryReconcileConfig 是对账的参数。零值即默认。
type InventoryReconcileConfig struct {
	// Interval 两轮之间的间隔。<= 0 时用 DefaultInventoryReconcileInterval。
	Interval time.Duration
	// PageSize 一次向库存服务要多少个库存键。<= 0 时用 inventory.DefaultKeysPage。
	PageSize int
	// SampleSize 报告与日志里至多带几条明细（计数不受它限制）。<= 0 时取 20。
	SampleSize int
}

// ReconcileFinding 是一条差异的明细。按 Kind 只有相应的几个字段有值。
type ReconcileFinding struct {
	Kind        string
	MerchantID  int64
	SKUID       int64
	StoreID     int64
	PromotionID int64
	JobKey      string
	Detail      string
}

// InventoryReconcileReport 是一轮的结果。计数是全量，Findings 至多 SampleSize 条。
type InventoryReconcileReport struct {
	Tenants int
	// Failed 这一轮有几家商户没对完（库存服务不在、查询出错）。非零就该看日志。
	Failed int
	// StockRows 过了多少行库存键。
	StockRows       int
	StockOrphan     int
	StockStale      int
	ActivityMissing int
	ActivityExtra   int
	DeadJobs        int64
	Findings        []ReconcileFinding
}

// Drift 这一轮有没有需要人看的差异（stale 不算，文件头第一条）。
func (r InventoryReconcileReport) Drift() bool {
	return r.StockOrphan > 0 || r.ActivityMissing > 0 || r.ActivityExtra > 0 || r.DeadJobs > 0
}

// InventoryReconcileService 是库存对账任务。
type InventoryReconcileService struct {
	repo InventoryReconcileRepository
	inv  inventory.Service
	cfg  InventoryReconcileConfig
	log  *slog.Logger
	now  func() time.Time

	mu     sync.Mutex
	last   InventoryReconcileReport
	lastAt time.Time
	runs   int64
}

// NewInventoryReconcileService 建对账任务。inv 是这个进程的库存服务（单体进程内、core 远端）。
func NewInventoryReconcileService(r InventoryReconcileRepository, inv inventory.Service,
	cfg InventoryReconcileConfig, log *slog.Logger) *InventoryReconcileService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInventoryReconcileInterval
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = inventory.DefaultKeysPage
	}
	if cfg.SampleSize <= 0 {
		cfg.SampleSize = 20
	}
	return &InventoryReconcileService{repo: r, inv: inv, cfg: cfg, log: log, now: time.Now}
}

// WithClock 换掉时钟，只给测试用（「上线中且未结束」的边界）。
func (s *InventoryReconcileService) WithClock(now func() time.Time) *InventoryReconcileService {
	s.now = now
	return s
}

// Last 最近一轮的结果、完成时刻与累计轮数（还没跑完过一轮时 runs 为 0）。
func (s *InventoryReconcileService) Last() (InventoryReconcileReport, time.Time, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.lastAt, s.runs
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。启动后立刻跑一轮，单轮出错不退出。
func (s *InventoryReconcileService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		rep, err := s.ReconcileOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "库存对账这一轮没跑起来", "err", err)
		case rep.Drift():
			s.log.WarnContext(ctx, "库存对账发现差异（明细见上面按商户的 WARN；只报不修，见 service/inventory_reconcile.go 文件头）",
				"tenants", rep.Tenants, "failed", rep.Failed, "stock_rows", rep.StockRows,
				"stock_orphan", rep.StockOrphan, "stock_stale", rep.StockStale,
				"activity_missing", rep.ActivityMissing, "activity_extra", rep.ActivityExtra,
				"dead_jobs", rep.DeadJobs)
		default:
			s.log.InfoContext(ctx, "库存对账完成一轮，两边一致", "tenants", rep.Tenants, "failed", rep.Failed,
				"stock_rows", rep.StockRows, "stock_stale", rep.StockStale)
		}
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "库存对账任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

// ReconcileOnce 跑一轮。导出的理由同 SweepOnce：测试与运维入口要能不等 ticker 驱动它。
// 返回 err 只在连商户清单都读不到时；单家商户的失败记在 Failed 里、不打断别家。
func (s *InventoryReconcileService) ReconcileOnce(ctx context.Context) (InventoryReconcileReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return InventoryReconcileReport{}, err
	}
	rep := InventoryReconcileReport{Tenants: len(merchants), Findings: []ReconcileFinding{}}
	for _, m := range merchants {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		s.reconcileTenant(ctx, m, &rep)
	}
	s.deadJobs(ctx, &rep)

	s.mu.Lock()
	s.last, s.lastAt = rep, s.now()
	s.runs++
	s.mu.Unlock()
	return rep, nil
}

func (s *InventoryReconcileService) add(rep *InventoryReconcileReport, f ReconcileFinding) {
	if len(rep.Findings) < s.cfg.SampleSize {
		rep.Findings = append(rep.Findings, f)
	}
}

// reconcileTenant 对一家商户做前两项检查。
func (s *InventoryReconcileService) reconcileTenant(ctx context.Context, merchantID int64, rep *InventoryReconcileReport) {
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)
	before := *rep
	failed := false
	if err := s.stock(tctx, merchantID, rep); err != nil {
		log.ErrorContext(ctx, "库存对账：孤儿库存行这一项没对完", "err", err,
			"inventory_unavailable", inventory.IsUnavailable(err))
		failed = true
	}
	if err := s.activity(tctx, merchantID, rep); err != nil {
		log.ErrorContext(ctx, "库存对账：活动配额这一项没对完", "err", err,
			"inventory_unavailable", inventory.IsUnavailable(err))
		failed = true
	}
	if failed {
		rep.Failed++
	}
	orphan, missing, extra := rep.StockOrphan-before.StockOrphan,
		rep.ActivityMissing-before.ActivityMissing, rep.ActivityExtra-before.ActivityExtra
	if orphan+missing+extra > 0 {
		log.WarnContext(ctx, "库存对账：这家商户两边对不上",
			"stock_orphan", orphan, "activity_missing", missing, "activity_extra", extra,
			"samples", samplesOf(rep.Findings[len(before.Findings):]))
	}
	if stale := rep.StockStale - before.StockStale; stale > 0 {
		log.InfoContext(ctx, "库存对账：有 SKU / 门店已软删的库存行（预期内的遗留，不处理）", "stock_stale", stale)
	}
}

// stock 是第一项：按键分页拿库存行，逐页回 core 问这些 SKU / 门店的状态。
// 每页一个 core 的租户事务（页与页之间不攥着连接等库存服务）。
func (s *InventoryReconcileService) stock(ctx context.Context, merchantID int64, rep *InventoryReconcileReport) error {
	after := inventory.StockKey{}
	for {
		keys, err := s.inv.StockKeys(ctx, inventory.KeysQuery{After: after, Limit: s.cfg.PageSize})
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		rep.StockRows += len(keys)
		skuSet, storeSet := map[int64]struct{}{}, map[int64]struct{}{}
		for _, k := range keys {
			skuSet[k.SKUID] = struct{}{}
			storeSet[k.StoreID] = struct{}{}
		}
		var skus, stores map[int64]repository.EntityState
		if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var err error
			if skus, err = tx.SKUStates(ctx, idsOf(skuSet)); err != nil {
				return err
			}
			stores, err = tx.StoreStates(ctx, idsOf(storeSet))
			return err
		}); err != nil {
			return err
		}
		for _, k := range keys {
			sk, st := skus[k.SKUID], stores[k.StoreID]
			switch {
			case !sk.Exists || !st.Exists:
				rep.StockOrphan++
				s.add(rep, ReconcileFinding{Kind: ReconcileStockOrphan, MerchantID: merchantID,
					SKUID: k.SKUID, StoreID: k.StoreID, Detail: missingSide(sk.Exists, st.Exists)})
			case sk.Deleted || st.Deleted:
				rep.StockStale++
			}
		}
		if len(keys) < s.cfg.PageSize {
			return nil
		}
		after = keys[len(keys)-1]
	}
}

func missingSide(skuExists, storeExists bool) string {
	switch {
	case !skuExists && !storeExists:
		return "SKU 与门店在 core 里都不存在"
	case !skuExists:
		return "SKU 在 core 里不存在"
	}
	return "门店在 core 里不存在"
}

// activity 是第二项：上线中且未结束的活动，两边 (活动, SKU) 的差集。
func (s *InventoryReconcileService) activity(ctx context.Context, merchantID int64, rep *InventoryReconcileReport) error {
	var ids []int64
	var keys []repository.PromotionSKUKey
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		ids, keys, err = tx.LivePromotionSKUs(ctx, s.now())
		return err
	}); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	act, err := s.inv.ActivityStock(ctx, inventory.ActivityQuery{PromotionIDs: ids})
	if err != nil {
		return err
	}
	core := make(map[inventory.ActivityKey]struct{}, len(keys))
	for _, k := range keys {
		key := inventory.ActivityKey{PromotionID: k.PromotionID, SKUID: k.SKUID}
		core[key] = struct{}{}
		if _, ok := act[key]; !ok {
			rep.ActivityMissing++
			s.add(rep, ReconcileFinding{Kind: ReconcileActivityMissing, MerchantID: merchantID,
				PromotionID: k.PromotionID, SKUID: k.SKUID, Detail: "活动商品在库存服务里没有配额行（报价不生效）"})
		}
	}
	extra := make([]inventory.ActivityKey, 0)
	for key := range act {
		if _, ok := core[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].PromotionID != extra[j].PromotionID {
			return extra[i].PromotionID < extra[j].PromotionID
		}
		return extra[i].SKUID < extra[j].SKUID
	})
	for _, key := range extra {
		rep.ActivityExtra++
		s.add(rep, ReconcileFinding{Kind: ReconcileActivityExtra, MerchantID: merchantID,
			PromotionID: key.PromotionID, SKUID: key.SKUID, Detail: "库存服务里有配额行，活动商品里没有这个 SKU"})
	}
	return nil
}

// deadJobs 是第三项：inventory.release 队列的死信（跨租户一条查询）。
func (s *InventoryReconcileService) deadJobs(ctx context.Context, rep *InventoryReconcileReport) {
	counts, sample, err := s.repo.DeadJobs(ctx, QueueInventoryRelease, s.cfg.SampleSize)
	if err != nil {
		s.log.ErrorContext(ctx, "库存对账：读死信的库存任务失败", "err", err)
		rep.Failed++
		return
	}
	for _, n := range counts {
		rep.DeadJobs += n
	}
	for _, j := range sample {
		s.add(rep, ReconcileFinding{Kind: ReconcileDeadJob, MerchantID: j.MerchantID, JobKey: j.JobKey,
			Detail: j.LastError})
	}
	if rep.DeadJobs > 0 {
		keys := make([]string, 0, len(sample))
		for _, j := range sample {
			keys = append(keys, j.JobKey)
		}
		s.log.WarnContext(ctx, "库存对账：有死信的库存任务（关单释放 / 退款回补没做成，那部分库存一直被占着）。"+
			"看过 last_error、确认库存服务正常之后，把任务改回 status = 0、attempts = 0 即可重跑（两种任务都按单号幂等）",
			"dead_jobs", rep.DeadJobs, "by_merchant", counts, "recent_job_keys", keys)
	}
}

func idsOf(set map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func samplesOf(fs []ReconcileFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		switch f.Kind {
		case ReconcileStockOrphan:
			out = append(out, f.Kind+" sku="+strconv.FormatInt(f.SKUID, 10)+" store="+strconv.FormatInt(f.StoreID, 10)+"（"+f.Detail+"）")
		case ReconcileActivityMissing, ReconcileActivityExtra:
			out = append(out, f.Kind+" promotion="+strconv.FormatInt(f.PromotionID, 10)+" sku="+strconv.FormatInt(f.SKUID, 10))
		}
	}
	return out
}
