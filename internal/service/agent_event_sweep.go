package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 事件扫描（AI 经营 M10 §3）：每 5 分钟按商户逐个跑一轮，写 stock_low 与 search_zero_spike 两类事件。
//
// stock_low 与库存预警报表（ReportService.InventoryAlerts）同一个口径、同一条取数路径：core 给出门店范围
// （未软删的门店）与要排除的 SKU（软删的），库存服务按「显式门店列表」取可售不高于预警线的行 ——
// 拆分形态下库存在自己的库里，这里只经 inventory.Service，不碰库存表。
// 每个（门店，SKU）24 小时内至多一条（最近 24 小时已有的跳过），去重键再带上 UTC 日期，
// 两轮并发跑也只落一行。
//
// search_zero_spike：近 1 小时同一个无结果词 ≥ 5 次（口径同搜索概况），每词每天（UTC）至多一条。
// 全店口径（检索日志没有门店维度），store_id 为空，只有全店范围的 AI 员工看得到。
//
// 一家店出错（库存服务不在、库挂了）只记日志、接着扫下一家：晚一轮不丢任何东西，下一轮会补上。

// AgentEventSweepInterval 是扫描间隔。
const AgentEventSweepInterval = 5 * time.Minute

const (
	// stockLowSweepLimit 是每家店每轮至多写几条 stock_low（按最缺的在前取）。
	stockLowSweepLimit = 500
	// zeroSpikeMinCount 是一个无结果词近 1 小时出现多少次算突增。
	zeroSpikeMinCount = 5
	// zeroSpikeLimit 是每家店每轮至多写几条 search_zero_spike。
	zeroSpikeLimit = 50
)

// AgentEventSweepRepository 是扫描要的仓储能力。
type AgentEventSweepRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// AgentEventSweepReport 是一轮扫描新写的事件数。
type AgentEventSweepReport struct {
	StockLow        int
	SearchZeroSpike int
}

// AgentEventSweepService 是事件扫描。
type AgentEventSweepService struct {
	repo AgentEventSweepRepository
	inv  inventory.Service
	log  *slog.Logger
	now  func() time.Time
}

func NewAgentEventSweepService(repo AgentEventSweepRepository, inv inventory.Service, log *slog.Logger) *AgentEventSweepService {
	if log == nil {
		log = slog.Default()
	}
	return &AgentEventSweepService{repo: repo, inv: inv, log: log, now: time.Now}
}

// Run 每 AgentEventSweepInterval 扫一轮，直到 ctx 结束。
func (s *AgentEventSweepService) Run(ctx context.Context) {
	t := time.NewTicker(AgentEventSweepInterval)
	defer t.Stop()
	for {
		if rep, err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "AI 员工事件扫描出错", "err", err)
		} else if rep.StockLow+rep.SearchZeroSpike > 0 {
			s.log.InfoContext(ctx, "AI 员工事件扫描", "stock_low", rep.StockLow, "search_zero_spike", rep.SearchZeroSpike)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SweepOnce 逐家扫一轮。只有列不出商户才返回错误；单家的错误记日志后跳过。
func (s *AgentEventSweepService) SweepOnce(ctx context.Context) (AgentEventSweepReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return AgentEventSweepReport{}, err
	}
	var total AgentEventSweepReport
	for _, m := range merchants {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		rep := s.SweepMerchant(ctx, m)
		total.StockLow += rep.StockLow
		total.SearchZeroSpike += rep.SearchZeroSpike
	}
	return total, nil
}

// SweepMerchant 扫一家店。导出给测试：全库扫会给别的测试的店写事件。
func (s *AgentEventSweepService) SweepMerchant(ctx context.Context, merchantID int64) AgentEventSweepReport {
	tctx := tenant.NewContext(ctx, merchantID)
	var rep AgentEventSweepReport
	var err error
	if rep.StockLow, err = s.sweepStockLow(tctx); err != nil {
		s.log.ErrorContext(ctx, "这家店的 stock_low 扫描出错", "merchant_id", merchantID, "err", err)
	}
	if rep.SearchZeroSpike, err = s.sweepZeroSpikes(tctx); err != nil {
		s.log.ErrorContext(ctx, "这家店的 search_zero_spike 扫描出错", "merchant_id", merchantID, "err", err)
	}
	return rep
}

func (s *AgentEventSweepService) sweepStockLow(ctx context.Context) (int, error) {
	var (
		stores  []repository.ReportAlertStore
		exclude []int64
		recent  map[repository.StoreSKU]bool
	)
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		if stores, err = tx.ReportAlertStores(ctx, repository.ReportFilter{}); err != nil {
			return err
		}
		if exclude, err = tx.ReportAlertExcludedSKUs(ctx); err != nil {
			return err
		}
		recent, err = tx.RecentStockLowEvents(ctx)
		return err
	}); err != nil || len(stores) == 0 {
		return 0, err
	}
	ids := make([]int64, 0, len(stores))
	for _, st := range stores {
		ids = append(ids, st.ID)
	}
	page, err := s.inv.LowStock(ctx, inventory.LowStockQuery{StoreIDs: ids, ExcludeSKUIDs: exclude, Limit: stockLowSweepLimit})
	if err != nil {
		return 0, err
	}
	day := s.now().UTC().Format("2006-01-02")
	n := 0
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		for _, r := range page.Items {
			if recent[repository.StoreSKU{StoreID: r.StoreID, SKUID: r.SKUID}] {
				continue
			}
			store := r.StoreID
			ok, err := emitAgentEvent(ctx, tx, AgentEventStockLow, &store, map[string]any{
				"store_id": r.StoreID, "sku_id": r.SKUID, "available": r.Available, "threshold": r.Warning,
			}, fmt.Sprintf("stock_low:%d:%d:%s", r.StoreID, r.SKUID, day))
			if err != nil {
				return err
			}
			if ok {
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *AgentEventSweepService) sweepZeroSpikes(ctx context.Context) (int, error) {
	day := s.now().UTC().Format("2006-01-02")
	n := 0
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		n = 0
		spikes, err := tx.SearchZeroSpikes(ctx, zeroSpikeMinCount, zeroSpikeLimit)
		if err != nil {
			return err
		}
		for _, sp := range spikes {
			// 词可能很长：去重键里放它的摘要（前 16 位 hex 足够区分同一家店一天里的词）。
			sum := sha256.Sum256([]byte(sp.Term))
			ok, err := emitAgentEvent(ctx, tx, AgentEventSearchZeroSpike, nil, map[string]any{
				"query": sp.Term, "count": sp.Count,
			}, "search_zero_spike:"+day+":"+hex.EncodeToString(sum[:8]))
			if err != nil {
				return err
			}
			if ok {
				n++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
