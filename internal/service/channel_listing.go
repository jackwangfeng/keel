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
// carriesPrice：这家门店是不是这个 binding 的价格出处（渠道按门店定价时每家都是；全渠道一个价时只有价格源门店是）。
type listingTarget struct {
	binding      repository.OutletBinding
	skuID        int64
	qty          int32
	price        int64
	carriesPrice bool
	external     repository.ChannelItemLink
	prev         *repository.ChannelListing
	// held：keel 这格可卖且有货（available > 0）。qty 为 0 时它区分挂零时段的两种（00330）：分配规则算 0 / keel 自己没货。
	held bool
	// zeroHeld：上次推的就是 0 时，那段还挂着的挂零时段的 held；nil = 没有挂着的段或没去查。
	zeroHeld *bool
	// zeroSpanMissing：上次成功推的是 0、这次还是 0，却没有挂着的段（上线前就是 0 的格子、段被清理过）。
	// 再推一次同样的 0，让推送成功的事务开段 —— 挂零时段只在那个事务里写（00330 文件头）。
	zeroSpanMissing bool
}

// unchanged：上次推出去的就是这个值，而且那一次是成功的。上次失败（last_error 非空，比如 CAS 冲突时记下的是
// 渠道上的数、价格并没有推上去）一律当作要推。不出价格的门店只比可售数：它的价格变了不用推任何东西。
func (t listingTarget) unchanged() bool {
	if t.prev == nil || t.prev.LastError != nil || t.prev.PublishedQty != t.qty {
		return false
	}
	// 一直是 0 但 held 变了（keel 补了货但规则仍算 0，或反过来）：再推一次同样的 0，让推送成功的事务关旧段开新段。
	if t.qty == 0 && (t.zeroSpanMissing || (t.zeroHeld != nil && *t.zeroHeld != t.held)) {
		return false
	}
	return !t.carriesPrice || t.prev.PublishedCents == t.price
}

// publishedCents 是推成功之后记下的价格：出价格的门店记这次的价；不出价格的门店保留上次记下的（它的价从没推上去，
// 记成 keel 价的话，以后它变成价格源时会被误判成「价格没变」而不推）。
func (t listingTarget) publishedCents() int64 {
	if t.carriesPrice {
		return t.price
	}
	if t.prev != nil {
		return t.prev.PublishedCents
	}
	return 0
}

// pushPrice：这一格这次要不要连价格一起推。
func (t listingTarget) pushPrice() bool {
	return t.carriesPrice && (t.prev == nil || t.prev.LastError != nil || t.prev.PublishedCents != t.price)
}

// channelBindingConfig 是 channel_bindings.config 里渠道层认的字段（其余字段归适配器或以后的期数）。
type channelBindingConfig struct {
	DefaultCategoryID int64  `json:"default_category_id"` // 商品源拉进来的新商品挂哪个类目
	PriceStoreID      int64  `json:"price_store_id"`      // 全渠道一个价时价格从哪家门店出
	WebhookBaseURL    string `json:"webhook_base_url"`    // 回调地址前缀（https://演示站域名），首拉时据此装 webhook
	AutoAccept        bool   `json:"auto_accept"`         // 要商家接单的渠道（AcceptRequired）自动接单（第三期）
	// RequestPolicy 是平台申请的处理策略：manual（缺省，等人）/ auto_agree_unshipped（未发货的取消自动同意）。
	// 接单提醒提前几分钟（accept_remind_minutes，缺省 3）只在 SQL 里读（db/queries/channels.sql 的 DueAcceptReminders）。
	RequestPolicy string `json:"request_policy"`
	// CommissionBP 是渠道佣金率（万分比，缺省 0），只用于算单件净收入（channel_allocation.go）。
	CommissionBP int32 `json:"commission_bp"`
}

func parseBindingConfig(raw json.RawMessage) channelBindingConfig {
	var c channelBindingConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c) // 配错的字段按没配处理；后台写 config 时只校验是 JSON 对象
	}
	return c
}

// carriesPrice 判断 storeID 是不是 binding b 的价格出处（见 listingTarget）。
func (s *ChannelService) carriesPrice(ctx context.Context, tx repository.Tx, b repository.OutletBinding, storeID int64) (bool, error) {
	if a, ok := s.reg.Lookup(b.Channel); ok && a.Caps().PricePerStore {
		return true, nil
	}
	links, err := tx.ListChannelStoreLinks(ctx, b.ID)
	if err != nil {
		return false, err
	}
	want := parseBindingConfig(b.Config).PriceStoreID
	var lowest int64
	for _, l := range links {
		if l.StoreID == want {
			return storeID == want, nil
		}
		if lowest == 0 || l.StoreID < lowest {
			lowest = l.StoreID
		}
	}
	if want != 0 {
		s.log.WarnContext(ctx, "binding 的 price_store_id 不在它的门店映射里，改用 id 最小的映射门店出价", "binding_id", b.ID,
			"price_store_id", want, "fallback_store_id", lowest)
	}
	return storeID == lowest, nil
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
			carries, err := s.carriesPrice(ctx, tx, b, storeID)
			if err != nil {
				return err
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
				catalogOwned := channel.Role(b.Roles)&channel.RoleCatalogSource != 0
				if catalogOwned && (prevBy[sku] == nil || !levels[sku].Exists) {
					// 商品源 binding 上没有推送基线（商品拉进来时这家门店还没映射、或平台在那个门店没备货）、
					// 或 keel 这家门店还没有库存行（初始库存还没写进去）的格子不推：推出去就是拿 keel 的 0
					// 覆盖平台上的现货。基线与初始库存由下一次拉商品补上（channel_catalog.go）。
					continue
				}
				var qty int32
				held := false
				if offer.Sellable(catalogOwned) {
					qty = channel.PublishedQty(levels[sku].Available, channel.ResolveStockRule(stockRules, storeID, sku))
					held = levels[sku].Available > 0
				}
				price := channel.PublishedPrice(offer.PriceCents, channel.ResolvePriceRule(priceRules, sku))
				out = append(out, listingTarget{binding: b, skuID: sku, qty: qty, price: price, carriesPrice: carries,
					external: link, prev: prevBy[sku], held: held})
			}
			if err := loadZeroHeld(ctx, tx, b.ID, storeID, out); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// loadZeroHeld 给「上次成功推的是 0、这次还是 0」的格子（只看 binding 这一组）补上还挂着的挂零时段的 held；
// 没有挂着的段的标 zeroSpanMissing（要补推一次开段）。只有这种格子才需要查：别的格子值变了本来就要推。
// 没有这种格子时不发查询。
func loadZeroHeld(ctx context.Context, tx repository.Tx, bindingID, storeID int64, ts []listingTarget) error {
	var skus []int64
	for _, t := range ts {
		if t.binding.ID == bindingID && t.qty == 0 && t.prev != nil && t.prev.LastError == nil && t.prev.PublishedQty == 0 {
			skus = append(skus, t.skuID)
		}
	}
	if len(skus) == 0 {
		return nil
	}
	open, err := tx.OpenChannelZeroHeld(ctx, bindingID, storeID, skus)
	if err != nil {
		return err
	}
	for i := range ts {
		t := &ts[i]
		if t.binding.ID != bindingID || t.qty != 0 || t.prev == nil || t.prev.LastError != nil || t.prev.PublishedQty != 0 {
			continue
		}
		if h, ok := open[t.skuID]; ok {
			t.zeroHeld = &h
		} else {
			t.zeroSpanMissing = true
		}
	}
	return nil
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

// SKUsChanged：keel 里改了这些 SKU 的价格或能不能卖（门店价 / 大区价 / 基准价、SKU 停售或删除、商品上下架或删除）。
// 写提交之后调，尽力而为：对每家映射到启用中销售渠道的门店重算这批 SKU，和上次推的不同就入队。
// 失败只记日志 —— 写已经提交，不能让后台那次改价报错；这一格在下一次库存变化或整店重算时会补上。
//
// 调用方持有的是可能为 nil 的 *ChannelService（KEEL_CHANNELS 关着时），nil 接收者直接返回：开关关闭时零开销。
func (s *ChannelService) SKUsChanged(ctx context.Context, skuIDs []int64) {
	if s == nil || len(skuIDs) == 0 {
		return
	}
	stores := map[int64]bool{}
	var order []int64
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		bs, err := tx.ListChannelBindings(ctx)
		if err != nil {
			return err
		}
		for _, b := range bs {
			if !b.IsActiveOutlet() {
				continue
			}
			links, err := tx.ListChannelStoreLinks(ctx, b.ID)
			if err != nil {
				return err
			}
			for _, l := range links {
				if !stores[l.StoreID] {
					stores[l.StoreID] = true
					order = append(order, l.StoreID)
				}
			}
		}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "改价 / 改在售之后没能重算渠道可售数（下一次变化会补上）", "err", err)
		return
	}
	for _, st := range order {
		if err := s.RecomputeListings(ctx, st, skuIDs, 0); err != nil {
			s.log.WarnContext(ctx, "改价 / 改在售之后重算渠道可售数失败（下一次变化会补上）", "store_id", st, "err", err)
		}
	}
}

// ProductChanged 同 SKUsChanged，按商品取它的全部 SKU（上下架、删商品）。
func (s *ChannelService) ProductChanged(ctx context.Context, productID int64) {
	if s == nil {
		return
	}
	var ids []int64
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		skus, err := tx.AdminListProductSKUs(ctx, productID)
		for _, k := range skus {
			ids = append(ids, k.ID)
		}
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "商品上下架之后没能列出它的 SKU 去重算渠道可售数", "product_id", productID, "err", err)
		return
	}
	s.SKUsChanged(ctx, ids)
}
