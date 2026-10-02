package service

// 对外可售数与价格：算、比、入队（推送本身在 channel_worker.go）。
//
//	库存服务 stock.changed {门店, SKU…} ─┐
//	后台改规则 / 映射门店 / 启用 binding ─┼→ 重算：对这家门店每个启用中的销售渠道 binding、每个映射过的 SKU，
//	                                     │   按规则算对外可售数与价格，和 channel_listings（上次推出去的）一样就跳过，
//	                                     └─  不一样就入队 channel.listing.push（job_key = binding:门店:SKU）
//
// 入队只是「这一格可能要推」的提示：jobs 的「同 key 未完成不重复入队」把一阵密集变化合并成一次，
// 任务真正执行时**再算一遍**当时的值去推（channel_worker.go），所以入队时算的数不必精确，也不进载荷。
//
// 读库存不在 core 事务里：单体下库存池就是业务池，攥着一条业务连接再去要一条是整池互等（与库存 outbox 同一条规矩）。

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// channelPushJob 是 channel.listing.push 的载荷（只有键）。
type channelPushJob struct {
	BindingID int64 `json:"binding_id"`
	StoreID   int64 `json:"store_id"`
	SKUID     int64 `json:"sku_id"`
}

// channelRecomputeJob 是 channel.listing.recompute 的载荷：一个 binding 在一家门店的整店重算。
type channelRecomputeJob struct {
	BindingID int64 `json:"binding_id"`
	StoreID   int64 `json:"store_id"`
}

// 渠道任务（推送、整店重算、回调处理）的重试：迟早要做成，比默认 5 次多给；退避封顶 10 分钟。
const (
	channelPushMaxAttempts = 20
	recomputePageSize      = 200
)

func (s *ChannelService) enqueueListingPush(ctx context.Context, tx repository.Tx, bindingID, storeID, skuID int64) error {
	payload, _ := json.Marshal(channelPushJob{BindingID: bindingID, StoreID: storeID, SKUID: skuID})
	_, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelListingPush,
		JobKey: fmt.Sprintf("%d:%d:%d", bindingID, storeID, skuID), Payload: payload, MaxAttempts: channelPushMaxAttempts})
	return err
}

func (s *ChannelService) enqueueRecompute(ctx context.Context, tx repository.Tx, bindingID, storeID int64) error {
	payload, _ := json.Marshal(channelRecomputeJob{BindingID: bindingID, StoreID: storeID})
	_, err := tx.EnqueueJob(ctx, repository.NewJob{Queue: QueueChannelListingRecompute,
		JobKey: fmt.Sprintf("%d:%d", bindingID, storeID), Payload: payload, MaxAttempts: channelPushMaxAttempts})
	return err
}

// enqueueRecomputeBinding 给一个 binding 映射的每家门店排一次整店重算（启用、改规则时）。
func (s *ChannelService) enqueueRecomputeBinding(ctx context.Context, tx repository.Tx, bindingID int64) error {
	links, err := tx.ListChannelStoreLinks(ctx, bindingID)
	if err != nil {
		return err
	}
	for _, l := range links {
		if err := s.enqueueRecompute(ctx, tx, bindingID, l.StoreID); err != nil {
			return err
		}
	}
	return nil
}

// StockChangedBranch 是 core 的 stock.changed 接收分支（单体 local://channel_stock_changed，微服务订阅主题）。
// 接收方不信消息内容以外的任何东西：消息只有门店与 SKU 的键，水位现问库存服务。
func (s *ChannelService) StockChangedBranch() dtm.BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		log := s.log.With("gid", gid, "branch_id", branchID, "op", op, "branch", inventory.BranchChannelStockChanged)
		if op != "action" {
			log.Error("stock.changed 的接收分支收到的 op 不是 action")
			return dtm.Unknown
		}
		m, err := inventory.DecodeStockMsg(gid, payload)
		var ctx context.Context
		if err == nil {
			ctx, _, err = dtm.TenantContextFromTenantGID(context.Background(), inventory.StockMsgGIDPrefix, gid)
		}
		if err != nil {
			log.Error("stock.changed 解不开（gid 或载荷），丢弃", "err", err, "payload", payload)
			return dtm.Success
		}
		if err := s.RecomputeListings(ctx, m.StoreID, m.SKUIDs, 0); err != nil {
			log.Warn("stock.changed 没处理完，按 Unknown 让协调器重试", "err", err)
			return dtm.Unknown
		}
		return dtm.Success
	}
}

// listingTarget 是一格（binding, 门店, SKU）现在应当推出去的值。
type listingTarget struct {
	binding  repository.OutletBinding
	skuID    int64
	qty      int32
	price    int64
	external repository.ChannelItemLink
	prev     *repository.ChannelListing
}

// unchanged：上次推出去的就是这个值，而且那一次是成功的。上次失败（last_error 非空，比如 CAS 冲突时记下的是
// 渠道上的数、价格并没有推上去）一律当作要推。
func (t listingTarget) unchanged() bool {
	return t.prev != nil && t.prev.LastError == nil && t.prev.PublishedQty == t.qty && t.prev.PublishedCents == t.price
}

// computeTargets 算一家门店一批 SKU 在各启用中的销售渠道上现在应当推的值。onlyBinding 非 0 时只算那一个。
// 读库存在事务之外（文件头）；没映射过的 SKU（渠道上没有对应商品）不算。
func (s *ChannelService) computeTargets(ctx context.Context, storeID int64, skuIDs []int64, onlyBinding int64) ([]listingTarget, error) {
	if len(skuIDs) == 0 {
		return nil, nil
	}
	var outs []repository.OutletBinding
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		outs, e = tx.ListActiveOutletBindingsForStore(ctx, storeID)
		return e
	}); err != nil {
		return nil, err
	}
	if onlyBinding != 0 {
		kept := outs[:0]
		for _, b := range outs {
			if b.ID == onlyBinding {
				kept = append(kept, b)
			}
		}
		outs = kept
	}
	if len(outs) == 0 {
		return nil, nil
	}
	levels, err := s.inv.StoreStock(ctx, storeID, skuIDs)
	if err != nil {
		return nil, err
	}
	var out []listingTarget
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		out = out[:0]
		offers, err := tx.ChannelSKUOffers(ctx, storeID, skuIDs)
		if err != nil {
			return err
		}
		for _, b := range outs {
			links, err := tx.ChannelItemLinks(ctx, b.ID, repository.ChannelItemSKU, skuIDs)
			if err != nil {
				return err
			}
			if len(links) == 0 {
				continue
			}
			srules, err := tx.ChannelStockRulesForStore(ctx, b.ID, storeID)
			if err != nil {
				return err
			}
			prules, err := tx.ListChannelPriceRules(ctx, b.ID)
			if err != nil {
				return err
			}
			prev, err := tx.ChannelListings(ctx, b.ID, storeID, skuIDs)
			if err != nil {
				return err
			}
			prevBy := make(map[int64]*repository.ChannelListing, len(prev))
			for i := range prev {
				prevBy[prev[i].SKUID] = &prev[i]
			}
			stockRules, priceRules := toStockRules(srules), toPriceRules(prules)
			for _, sku := range skuIDs {
				link, ok := links[sku]
				if !ok {
					continue
				}
				offer, ok := offers[sku]
				if !ok {
					// 拿不到门店价（SKU 不存在、不属于本店）：不推 —— 推出去就是一个 0 元的商品。
					continue
				}
				var qty int32
				if offer.Sellable {
					qty = channel.PublishedQty(levels[sku].Available, channel.ResolveStockRule(stockRules, storeID, sku))
				}
				price := channel.PublishedPrice(offer.PriceCents, channel.ResolvePriceRule(priceRules, sku))
				out = append(out, listingTarget{binding: b, skuID: sku, qty: qty, price: price, external: link, prev: prevBy[sku]})
			}
		}
		return nil
	})
	return out, err
}

// RecomputeListings 重算一家门店一批 SKU，把和上次推送不同的格子入队。onlyBinding 非 0 时只算那一个 binding。
func (s *ChannelService) RecomputeListings(ctx context.Context, storeID int64, skuIDs []int64, onlyBinding int64) error {
	targets, err := s.computeTargets(ctx, storeID, skuIDs, onlyBinding)
	if err != nil || len(targets) == 0 {
		return err
	}
	return s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		for _, t := range targets {
			if t.unchanged() {
				continue
			}
			if err := s.enqueueListingPush(ctx, tx, t.binding.ID, storeID, t.skuID); err != nil {
				return err
			}
		}
		return nil
	})
}

// recomputeStore 是整店重算：按页走这个 binding 映射过的全部 SKU。
func (s *ChannelService) recomputeStore(ctx context.Context, bindingID, storeID int64) error {
	var after int64
	for {
		var ids []int64
		if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var e error
			ids, e = tx.LinkedSKUIDsPage(ctx, bindingID, after, recomputePageSize)
			return e
		}); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := s.RecomputeListings(ctx, storeID, ids, bindingID); err != nil {
			return err
		}
		after = ids[len(ids)-1]
	}
}

func toStockRules(rs []repository.ChannelStockRule) []channel.StockRule {
	out := make([]channel.StockRule, len(rs))
	for i, r := range rs {
		out[i] = channel.StockRule{StoreID: r.StoreID, SKUID: r.SKUID, RatioBP: r.RatioBP, SafetyQty: r.SafetyQty, CapQty: r.CapQty}
	}
	return out
}

func toPriceRules(rs []repository.ChannelPriceRule) []channel.PriceRule {
	out := make([]channel.PriceRule, len(rs))
	for i, r := range rs {
		out[i] = channel.PriceRule{SKUID: r.SKUID, MarkupBP: r.MarkupBP, FixedCents: r.FixedCents}
	}
	return out
}
