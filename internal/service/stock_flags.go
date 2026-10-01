package service

import (
	"context"
	"fmt"
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
// 谁写它（2026-09-30 起，docs/电商系统-总体架构.md「派生数据同步约定」）：
//
//	① 库存服务在可售数**跨过 0** 时发的二阶段消息（inventory 包 stock_msg.go）：扣减、SAGA 补偿、关单释放、
//	   退款回补、后台设值 / 调整、建 SKU 首行，全部在改库存的那个本地事务里判、在同一个事务里登记消息。
//	   core 的接收分支（StockMsgBranch，local://stock_changed 或内网 /internal/v1/saga/stock_changed）
//	   **不信消息内容**，只拿它定位「哪家店、哪几个 SKU」，回源问库存服务当前水位、重算那几件商品；
//	   子事务屏障挡住重复投递。不跨 0 的变动（10 → 9）改变不了任何商品级标记，不发。
//	② 后台三条改库存（inventory_admin.go：按 SKU 设、按门店设、相对调整）成功之后，照旧立刻刷那件商品在那家店。
//	   ① 也会覆盖这三条（它们跨 0 时同样发消息），留着 ② 的理由：它是同步的 —— 店员改完库存回到列表，
//	   排序已经是新的，不必等消息投递；而且它不依赖协调器（没接通知的装配里照样对）。两者写的是同一个
//	   按当前水位重算的结果，谁先谁后都一样。
//	③ StockFlagService 每隔 Interval（默认 1 小时）全量刷一轮，降成兜底：兜的是「将来有人新加一条改可售数的
//	   路径、忘了发消息」，以及 SKU 上下架这类 core 自己的变化（上下架时这里不逐件刷）。
//
// **乱序与并发**：三个写入方都走 refreshStore —— 读水位 → 写 → **复读核对**，变了的再写。
// 两个写入方交错时（A 读到 0、B 读到 5、B 先写 true、A 后写 false），后写的那个在复读时看见 5，自己改回来；
// 每个写入方在自己最后一次写之后都核对过当前水位，所以最后落地的值总是对的，不需要跨服务的锁。
// 不在一个事务里「锁住再问库存服务」：单体下库存池就是业务池，事务里再要一个连接会整池互等
// （inventory_admin.go 的同一条规矩）。
//
// 刷新失败（库存服务不在）只记日志：旧标记继续用 —— 排序晚一点不值得让任何一条请求失败；消息那条路上
// 返回 Unknown，协调器会重试到库存服务回来。
//
// **缺行按无货**（2026-10-01，db/queries/products.sql 文件头「缺行为什么按无货」）：列表排序、
// GET /products?in_stock_only=true 的过滤与计数都这么算。缺行只出现在「新建之后还没种上」的短窗口
// （下面的 seedStoreStockFlags / seedProductStockFlags）；种失败时进库存 outbox 的队列重试
// （inventory_outbox.go 的 jobKindStockFlagSeed），库存服务回来之后几分钟内补上，不等整点的全量刷新。
// 之前缺行按「有没有在售 SKU」算（≈ 有货），新店种失败的那一小时里整店商品排进有货段（破坏性测试）。
//
// 判据与详情页、检索、列表显示同一个：这家店里任意一个在售 SKU 可售数 > 0。

// DefaultStockFlagInterval 是全量刷新的间隔。跨 0 的变化由消息即时同步（上面 ①），这一轮只兜底。
const DefaultStockFlagInterval = time.Hour

// stockFlagBatch 是一次问库存服务的 SKU 数上限（inventory.Local 的 maxBatch 是 5000，留余量）。
const stockFlagBatch = 1000

// stockFlagVerifyRounds 是 refreshStore 复读核对的轮数上限。水位在这几轮之间一直来回跨 0 时停下：
// 每一次跨 0 本身都会带来一条新消息、一次新的 refreshStore，不需要这一次追到底。
const stockFlagVerifyRounds = 3

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
	for _, storeID := range stores {
		if err := refreshStore(ctx, repo, inv, storeID, products, skus, plainFlagWrite(repo, storeID)); err != nil {
			return err
		}
	}
	return nil
}

// flagWrite 是 refreshStore 的第一次写。done 为真表示「这件事已经有人做过了」（屏障判成重复），不必核对。
type flagWrite func(ctx context.Context, products []int64, flags []bool) (done bool, err error)

func plainFlagWrite(repo tenantRunner, storeID int64) flagWrite {
	return func(ctx context.Context, products []int64, flags []bool) (bool, error) {
		return false, repo.WithTenant(ctx, func(tx repository.Tx) error {
			return tx.UpsertProductStoreStock(ctx, storeID, products, flags)
		})
	}
}

// refreshStore 刷一家店上一批商品：读水位 → 写（first）→ 复读核对，变了的再写。理由见文件头「乱序与并发」。
func refreshStore(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID int64,
	products []int64, skus map[int64][]int64, first flagWrite) error {
	var all []int64
	for _, pid := range products {
		all = append(all, skus[pid]...)
	}
	flags, err := storeFlags(ctx, inv, storeID, products, skus, all)
	if err != nil {
		return err
	}
	done, err := first(ctx, products, flags)
	if err != nil || done {
		return err
	}
	for round := 0; round < stockFlagVerifyRounds; round++ {
		again, err := storeFlags(ctx, inv, storeID, products, skus, all)
		if err != nil {
			return err
		}
		var ps []int64
		var fs []bool
		for i := range products {
			if again[i] != flags[i] {
				ps, fs = append(ps, products[i]), append(fs, again[i])
			}
		}
		if len(ps) == 0 {
			return nil
		}
		if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
			return tx.UpsertProductStoreStock(ctx, storeID, ps, fs)
		}); err != nil {
			return err
		}
		flags = again
	}
	slog.InfoContext(ctx, "有货排序标记：复读核对几轮都在变（水位在来回跨 0），停在最后一次读到的值；后续的跨 0 通知会接着刷",
		"store_id", storeID)
	return nil
}

// storeFlags 问一家店这批 SKU 的水位，按商品算标记（与 products 一一对应）。
func storeFlags(ctx context.Context, inv inventory.Service, storeID int64, products []int64,
	skus map[int64][]int64, all []int64) ([]bool, error) {
	levels := make(map[int64]inventory.Level, len(all))
	for i := 0; i < len(all); i += stockFlagBatch {
		j := min(i+stockFlagBatch, len(all))
		got, err := inv.StoreStock(ctx, storeID, all[i:j])
		if err != nil {
			return nil, err
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
	return flags, nil
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
		slog.WarnContext(ctx, "改完库存没刷上商品列表的有货排序标记（跨 0 通知或下一轮全量刷新会补上）",
			"store_id", storeID, "sku_id", skuID, "err", err)
	}
}

// seedStoreStockFlags 给一家新门店补齐全部商品的标记（2026-09-30）。
//
// 新门店建好时标记表里一行都没有。从来没设过库存的 SKU 永远不会跨 0，只靠整点的全量刷新的话，
// 这家店要等到整点才有标记（宝安中心区店踩过）。所以建店之后当场按实际水位（新店一般全是 0）写一遍。
//
// 失败（库存服务不在）时缺行按无货（文件头），不会把没货的排到前面；但真有货的商品在补上之前排在后面、
// in_stock_only 看不见，所以在一个小事务里入队一条重试任务（库存 outbox 的队列，jobKindStockFlagSeed）。
// 选它而不是「熔断恢复 / 下一次成功的库存调用」触发：这条队列本来就是「库存服务不在时把一件事重试到它回来」
// 的机制（封顶 5 分钟退避，进程重启不丢，多实例只有一个占得到），每个 core 进程都在跑它的 worker；
// 另起一套回调要在熔断器里挂业务钩子，还会在多实例下各补一遍。入队本身也失败了只记日志，全量刷新兜底。
func seedStoreStockFlags(ctx context.Context, repo tenantRunner, inv inventory.Service, storeID int64) {
	if inv == nil {
		return
	}
	if err := refreshStockFlags(ctx, repo, inv, []int64{storeID}, nil); err != nil {
		slog.WarnContext(ctx, "新门店没补上商品列表的有货排序标记，进队列重试", "store_id", storeID, "err", err)
		retryStockFlagSeed(ctx, repo, fmt.Sprintf("stock_flags:store:%d", storeID),
			inventoryJob{Kind: jobKindStockFlagSeed, StoreIDs: []int64{storeID}})
	}
}

// seedProductStockFlags 给新商品 / 新 SKU 在所有门店补齐标记，理由同 seedStoreStockFlags：
// 没有初始库存的新 SKU 不会跨 0。失败时同样进队列重试。
func seedProductStockFlags(ctx context.Context, repo tenantRunner, inv inventory.Service, productIDs []int64) {
	if inv == nil || len(productIDs) == 0 {
		return
	}
	if err := refreshStockFlags(ctx, repo, inv, nil, productIDs); err != nil {
		slog.WarnContext(ctx, "新商品没补上商品列表的有货排序标记，进队列重试", "products", productIDs, "err", err)
		// job_key 带时刻：同一批商品的重试任务撞键时第二条会被当成重复丢掉，而两批的商品可能不一样。
		retryStockFlagSeed(ctx, repo, fmt.Sprintf("stock_flags:products:%d:%d", productIDs[0], time.Now().UnixNano()),
			inventoryJob{Kind: jobKindStockFlagSeed, ProductIDs: productIDs})
	}
}

// retryStockFlagSeed 入队一条种标记的重试任务（库存 outbox 的队列）。尽力而为。
func retryStockFlagSeed(ctx context.Context, repo tenantRunner, key string, j inventoryJob) {
	if err := repo.WithTenant(ctx, func(tx repository.Tx) error {
		return enqueueInventoryJob(ctx, tx, key, j)
	}); err != nil {
		slog.ErrorContext(ctx, "有货排序标记的重试任务没入上队（下一轮全量刷新兜底）", "job_key", key, "err", err)
	}
}

// StockFlagRepository 是全量刷新需要的仓储能力。
type StockFlagRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
	// WithSagaBranch 给跨 0 通知的接收分支用：屏障与标记同一个事务（repository/saga.go）。
	WithSagaBranch(ctx context.Context, gid, branchID, op string, fn func(repository.Tx) error) (repository.Decision, error)
}

// StockFlagService 是全量刷新的定时任务，也是跨 0 通知的接收方（StockMsgBranch）。
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
