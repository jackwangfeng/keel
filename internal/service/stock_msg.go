package service

// 跨 0 通知（库存 → 有货排序标记）的接收分支。与全量刷新分开放在这个文件，是 tenant_context_test.go
// 那条机械检查要求的：stock_flags.go 的全量刷新按登记手搓租户上下文（枚举 merchants），而分支的租户
// 只许从 gid 来 —— 两者不能在同一个文件里。算法本身（refreshStore）在 stock_flags.go，文件头也在那里。

import (
	"context"
	"errors"

	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// StockMsgBranch 是跨 0 通知的接收分支（inventory.BranchStockChanged）：单体注册成 local://stock_changed，
// 拆分挂在 core 的内网端口上（dtm.MountBranches 到 rpc.Routes.Saga）。
//
// 消息只告诉我们「哪家店、哪几个 SKU 跨过了 0」（载荷里，inventory.DecodeStockMsg；0.12 之前的消息编在 gid 里）；
// 这里回源：SKU → 商品 → 那几件商品当前的在售 SKU → 问库存服务当前水位 → 重算、写、复读核对（refreshStore）。
// 第一次写与子事务屏障同一个事务：同一条消息投递两次，第二次被屏障判成重复，什么都不做。
// 两条消息谁先到都一样：两次都按当时的水位算。
//
// 微服务形态下它订阅在主题 inventory.TopicStockZeroCrossing 上，分支号是 01 或 01-0n（扇出），屏障按 gid + branch_id 去重。
func (s *StockFlagService) StockMsgBranch() dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		log := s.log.With("gid", gid, "branch_id", branchID, "op", op)
		if op != "action" {
			log.Error("跨 0 通知的接收分支收到的 op 不是 action")
			return dtm.Unknown
		}
		m, err := inventory.DecodeStockMsg(gid, payload)
		var ctx context.Context
		if err == nil {
			// 租户只从 gid 来，经 dtm 的那一个产生者（tenant_context_test.go）。
			ctx, _, err = dtm.TenantContextFromTenantGID(context.Background(), inventory.StockMsgGIDPrefix, gid)
		}
		if err != nil {
			// 二阶段消息的目标分支没有「失败」可言：dtmrs 对失败的 action 也是重试（msg_advance）。
			// 一条解不开的 gid 重试一万次也解不开，记下来、吞掉。
			log.Error("跨 0 通知解不开（gid 或载荷），丢弃", "err", err, "payload", payload)
			return dtm.Success
		}
		if err := s.refreshNotified(ctx, gid, branchID, op, m); err != nil {
			log.Warn("跨 0 通知没处理完，按 Unknown 让协调器重试", "err", err,
				"inventory_unavailable", inventory.IsUnavailable(err))
			return dtm.Unknown
		}
		return dtm.Success
	}
}

func (s *StockFlagService) refreshNotified(ctx context.Context, gid, branchID, op string, m inventory.StockMsg) error {
	var products []int64
	var skus map[int64][]int64
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		seen := map[int64]bool{}
		for _, id := range m.SKUIDs {
			pid, err := tx.ProductOfSKU(ctx, id)
			if errors.Is(err, repository.ErrCatalogNotFound) {
				continue // SKU 不在了（不会：SKU 只软删）；没有商品可刷
			}
			if err != nil {
				return err
			}
			if !seen[pid] {
				seen[pid] = true
				products = append(products, pid)
			}
		}
		if len(products) == 0 {
			return nil
		}
		var e error
		skus, e = tx.OnSaleSKUsOfProducts(ctx, products)
		return e
	})
	if err != nil || len(products) == 0 {
		return err
	}
	return refreshStore(ctx, s.repo, s.inv, m.StoreID, products, skus,
		func(ctx context.Context, ps []int64, flags []bool) (bool, error) {
			d, err := s.repo.WithSagaBranch(ctx, gid, branchID, op, func(tx repository.Tx) error {
				return tx.UpsertProductStoreStock(ctx, m.StoreID, ps, flags)
			})
			return d == repository.DecisionDuplicated, err
		})
}
