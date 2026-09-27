package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 商品列表按「这家店有没有货」排序（2026-09-27）用的冗余标记 product_store_stock（00087）。
//
// 库存归库存服务，列表那条 SQL JOIN 不到 inventories，于是 core 留一份「门店 × 商品 → 有没有货」，
// 只决定顺序；列表上显示的 in_stock 仍是现问库存服务的（product.go fillInStock）。
//
// 谁写它：
//
//	① 后台三条改库存（inventory_admin.go：按 SKU 设、按门店设、相对调整）成功之后，立刻刷那件商品在那家店；
//	② StockFlagService 每隔 Interval（默认 1 分钟）全量刷一轮，兜住不经 core 后台的变动 ——
//	   下单扣减、关单 / 退款回补都发生在库存服务里，core 不逐笔知道结果水位。
//
// 所以排序最多晚一轮；显示永远是准的。刷新失败（库存服务不在）只记日志：旧标记继续用，
// 下一轮再刷 —— 排序晚一点不值得让任何一条请求失败。
//
// 判据与详情页、检索、列表显示同一个：这家店里任意一个在售 SKU 可售数 > 0。

// DefaultStockFlagInterval 是全量刷新的间隔。
const DefaultStockFlagInterval = time.Minute

// stockFlagBatch 是一次问库存服务的 SKU 数上限（inventory.Local 的 maxBatch 是 5000，留余量）。
const stockFlagBatch = 1000

// refreshStockFlags 刷一批门店上一批商品的标记。storeIDs / productIDs 为 nil 表示全部。
// ctx 必须带租户（tenant.NewContext）。
func refreshStockFlags(ctx context.Context, repo tenantRunner, inv inventory.Service,
	storeIDs, productIDs []int64) error {
	var skus map[int64][]int64
	stores := storeIDs
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if productIDs == nil {
			skus, e = tx.AllOnSaleSKUs(ctx)
		} else {
			skus, e = tx.OnSaleSKUsOfProducts(ctx, productIDs)
		}
		if e != nil {
			return e
		}
		if stores == nil {
			stores, e = tx.StoreIDsForStockFlags(ctx)
		}
		return e
	})
	if err != nil {
		return err
	}
	products := productIDs
	if products == nil {
		for pid := range skus {
			products = append(products, pid)
		}
	}
	if len(products) == 0 || len(stores) == 0 {
		return nil
	}
	var all []int64
	for _, list := range skus {
		all = append(all, list...)
	}
	for _, storeID := range stores {
		levels := make(map[int64]inventory.Level, len(all))
		for i := 0; i < len(all); i += stockFlagBatch {
			j := min(i+stockFlagBatch, len(all))
			got, err := inv.StoreStock(ctx, storeID, all[i:j])
			if err != nil {
				return err
			}
			for k, v := range got {
				levels[k] = v
			}
		}
		flags := make([]bool, len(products))
		for i, pid := range products {
			// 一个在售 SKU 都没有的商品（刚被下架最后一个规格）算无货。
			for _, id := range skus[pid] {
				if levels[id].Available > 0 {
					flags[i] = true
					break
				}
			}
		}
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			return tx.UpsertProductStoreStock(ctx, storeID, products, flags)
		}); err != nil {
			return err
		}
	}
	return nil
}

// refreshSKUStockFlag 是后台改库存之后的那一刷：这个 SKU 所属商品在这家店。
// 尽力而为：失败只记日志，改库存本身已经成功，不能因为排序标记没刷上就回错。
func refreshSKUStockFlag(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID, skuID int64) {
	var pid int64
	err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		pid, e = tx.ProductOfSKU(ctx, skuID)
		return e
	})
	if err == nil {
		err = refreshStockFlags(ctx, repo, inv, []int64{storeID}, []int64{pid})
	}
	if err != nil {
		slog.WarnContext(ctx, "改完库存没刷上商品列表的有货排序标记（下一轮全量刷新会补上）",
			"store_id", storeID, "sku_id", skuID, "err", err)
	}
}

// StockFlagRepository 是全量刷新需要的仓储能力。
type StockFlagRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// StockFlagService 是全量刷新的定时任务。
type StockFlagService struct {
	repo     StockFlagRepository
	inv      inventory.Service
	interval time.Duration
	log      *slog.Logger
}

func NewStockFlagService(r StockFlagRepository, inv inventory.Service, interval time.Duration,
	log *slog.Logger) *StockFlagService {
	if interval <= 0 {
		interval = DefaultStockFlagInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &StockFlagService{repo: r, inv: inv, interval: interval, log: log}
}

// Run 立刻刷一轮，之后每隔 interval 一轮，直到 ctx 结束。
func (s *StockFlagService) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		if err := s.RefreshOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.ErrorContext(ctx, "商品列表有货排序标记这一轮没刷起来", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RefreshOnce 按商户逐个全量刷新。一个商户失败不影响其余商户，失败数只进日志。
func (s *StockFlagService) RefreshOnce(ctx context.Context) error {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return err
	}
	for _, m := range merchants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := refreshStockFlags(tenant.NewContext(ctx, m), s.repo, s.inv, nil, nil); err != nil {
			s.log.WarnContext(ctx, "商品列表有货排序标记：这家商户没刷完，沿用旧标记",
				"merchant_id", m, "err", err, "inventory_unavailable", inventory.IsUnavailable(err))
		}
	}
	return nil
}
