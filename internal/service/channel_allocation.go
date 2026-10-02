package service

// 渠道库存分配的事实与基线建议：MCP 工具 channel_allocation_review
// （docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §4.1）。只读。
//
// 一家门店、一批 SKU，每个 SKU 每个销售渠道一行（自营一行 + 这家门店映射了的每个启用 binding、且这个 SKU 在它上面
// 映射过的一行）。窗口是「现在往前 days × 24 小时」，所有数都按这一个窗口算：
//
//   - 卖出：订单 20 / 30 / 40 / 50（paid_at 在窗口里），与 restock_plan 同一口径；10、90、60（整单退款）不算。
//     自营 = 来源 0 的单；渠道 = 来源 1、按渠道单的 binding 归。
//   - 有货时的日均：卖出 ÷ (days − 挂零小时 / 24)，分母下限 1。渠道的挂零小时是挂零时段（00330，held 与否合计）；
//     自营没有挂零时段，用 keel 的断货天数（StockoutDays，与 restock_plan 一样去分母）。
//   - 缺货拒单：渠道单接单 SAGA 走了补偿（有一张 90 的来源 1 keel 订单）、最终没接成、异常是「缺货：」或已拒单，
//     按渠道单行件数（db/queries/channels.sql ChannelStockoutRejects）。
//   - 单件净收入：渠道价 × (1 − config.commission_bp / 10000) − 成本价；成本价为 0 不减并标 cost_missing。
//     自营佣金 0、价格 = 门店价。
//
// keel 可售 ≤ 0 的 SKU 不给建议：缺的是货，不是分配（Review Focus 4）。
//
// 读库存不在 core 事务里（同 channel_listing.go 文件头）。

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/repository"
)

const (
	allocationDefaultDays = 14
	allocationMinDays     = 7
	allocationMaxDays     = 30
	allocationMaxSKUs     = 50
	// 基线建议的参数（spec §4.1）。
	allocationHeldZeroHours = 24   // 分配造成的挂零到这么多小时才建议上调
	allocationRatioStepBP   = 1000 // 上调 10 个百分点
	allocationDemandDays    = 3    // 「货够不够分」按日均 × 3 天的需求算
)

// AllocationReviewInput 是 channel_allocation_review 的入参。Days 7–30，0 = 默认 14；SKUIDs 空 = 自动挑，至多 50 个。
type AllocationReviewInput struct {
	StoreID int64
	SKUIDs  []int64
	Days    int
}

// AllocationReview 是一次计算的结果。Note 非空时是给 AI 的说明（渠道没开、门店没接渠道……）。
type AllocationReview struct {
	StoreID int64           `json:"store_id"`
	Days    int             `json:"days"`
	Note    string          `json:"note,omitempty"`
	SKUs    []AllocationSKU `json:"skus"`
}

// AllocationSKU 是一个 SKU 在这家门店的分配事实与建议。
type AllocationSKU struct {
	SKUID        int64                  `json:"sku_id"`
	Title        string                 `json:"title"`
	Available    int32                  `json:"available"`
	StockoutDays int                    `json:"stockout_days"`
	CostMissing  bool                   `json:"cost_missing"`
	Channels     []AllocationChannel    `json:"channels"`
	Suggestions  []AllocationSuggestion `json:"suggestions"`
}

// AllocationChannel 是一个销售渠道上的一行。BindingID 为空 = 自营（没有规则，Rule / RuleLevel 为空）。
type AllocationChannel struct {
	BindingID       *int64             `json:"binding_id,omitempty"`
	Name            string             `json:"name"`
	Rule            *channel.StockRule `json:"rule,omitempty"`
	RuleLevel       string             `json:"rule_level,omitempty"` // binding | store | sku | default
	PublishedQty    int32              `json:"published_qty"`
	Sold            int64              `json:"sold"`
	DailyVelocity   float64            `json:"daily_velocity"`
	HeldZeroHours   float64            `json:"held_zero_hours"`
	EmptyZeroHours  float64            `json:"empty_zero_hours"`
	StockoutRejects int64              `json:"stockout_rejects"`
	UnitNetCents    int64              `json:"unit_net_cents"`
}

// AllocationSuggestion 是一条基线建议：把 binding 在这家门店的这个 SKU（SKUID 为空 = 门店级）的规则改成这组值。
type AllocationSuggestion struct {
	BindingID int64  `json:"binding_id"`
	SKUID     *int64 `json:"sku_id,omitempty"`
	RatioBP   int32  `json:"ratio_bp"`
	SafetyQty int32  `json:"safety_qty"`
	CapQty    *int32 `json:"cap_qty,omitempty"`
	Why       string `json:"why"`
}

// AllocationReview 实现 channel_allocation_review。判权与 slow_movers / restock_plan 相同（storeOperate）：
// 门店管理员只算得到自己的店。渠道层关着（s 为 nil）时返回空结果与说明。
func (s *ChannelService) AllocationReview(ctx context.Context, in AllocationReviewInput) (AllocationReview, error) {
	return s.AllocationReviewAt(ctx, in, time.Now())
}

// AllocationReviewAt 同 AllocationReview，窗口截到 now（复盘与测试用固定时钟）。
func (s *ChannelService) AllocationReviewAt(ctx context.Context, in AllocationReviewInput, now time.Time) (AllocationReview, error) {
	if _, err := requireStaff(ctx); err != nil {
		return AllocationReview{}, err
	}
	days := in.Days
	if days == 0 {
		days = allocationDefaultDays
	}
	if days < allocationMinDays || days > allocationMaxDays {
		return AllocationReview{}, fmt.Errorf("%w: days 取 %d–%d", ErrAdminListBadRequest, allocationMinDays, allocationMaxDays)
	}
	if len(in.SKUIDs) > allocationMaxSKUs {
		return AllocationReview{}, fmt.Errorf("%w: sku_ids 至多 %d 个", ErrAdminListBadRequest, allocationMaxSKUs)
	}
	out := AllocationReview{StoreID: in.StoreID, Days: days, SKUs: []AllocationSKU{}}
	if s == nil {
		out.Note = "没有启用的销售渠道（渠道层没开）"
		return out, nil
	}
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	var (
		outs    []repository.OutletBinding
		rules   = map[int64][]channel.StockRule{}
		sold    []repository.ChannelSKUQty
		rejects []repository.ChannelSKUQty
		infos   []repository.ChannelAllocationSKU
		offers  map[int64]repository.SKUOffer
		zero    []repository.ChannelZeroHours
		skuIDs  = dedupeIDs(in.SKUIDs)
	)
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		if _, err := authorizeStore(ctx, tx, in.StoreID, storeOperate); err != nil {
			return err
		}
		var err error
		if outs, err = tx.ListActiveOutletBindingsForStore(ctx, in.StoreID); err != nil || len(outs) == 0 {
			return err
		}
		for _, b := range outs {
			rs, err := tx.ChannelStockRulesForStore(ctx, b.ID, in.StoreID)
			if err != nil {
				return err
			}
			rules[b.ID] = toStockRules(rs)
		}
		if sold, err = tx.ChannelSoldBySource(ctx, in.StoreID, since); err != nil {
			return err
		}
		if rejects, err = tx.ChannelStockoutRejects(ctx, in.StoreID, since); err != nil {
			return err
		}
		if len(skuIDs) == 0 {
			zs, err := tx.ChannelZeroSpanSKUs(ctx, in.StoreID, since)
			if err != nil {
				return err
			}
			skuIDs = pickAllocationSKUs(sold, zs)
		}
		if infos, err = tx.ChannelAllocationSKUs(ctx, skuIDs); err != nil {
			return err
		}
		if offers, err = tx.ChannelSKUOffers(ctx, in.StoreID, skuIDs); err != nil {
			return err
		}
		zero, err = tx.ChannelZeroHours(ctx, in.StoreID, skuIDs, since, now)
		return err
	})
	if err != nil {
		return AllocationReview{}, err
	}
	if len(outs) == 0 {
		out.Note = "这家门店没有映射启用中的销售渠道"
		return out, nil
	}
	if len(infos) == 0 {
		out.Note = "窗口内这家门店没有在任何渠道卖出或挂零过的 SKU"
		return out, nil
	}
	ids := make([]int64, len(infos))
	for i, k := range infos {
		ids[i] = k.SKUID
	}

	// 库存与断货天数（事务外）。断货天数的窗口不早于 SKU 上架（同 restock.go）。
	tzName, err := s.repo.ShopTimezone(ctx)
	if err != nil {
		return AllocationReview{}, err
	}
	tzName, _ = reportLocation(tzName)
	levels, err := s.inv.StoreStock(ctx, in.StoreID, ids)
	if err != nil {
		return AllocationReview{}, err
	}
	byWindow := map[int][]int64{}
	for _, k := range infos {
		w := days
		if ex := int(math.Ceil(now.Sub(k.CreatedAt).Hours() / 24)); ex > 0 && ex < w {
			w = ex
		}
		byWindow[w] = append(byWindow[w], k.SKUID)
	}
	stockout := map[int64]int{}
	for w, part := range byWindow {
		so, err := s.inv.StockoutDays(ctx, in.StoreID, part, w, tzName)
		if err != nil {
			return AllocationReview{}, err
		}
		for k, v := range so {
			stockout[k] = v
		}
	}
	// 对外可售数与渠道价：与推送同一个算法（computeTargets）；没在这个 binding 上映射的 SKU 不出行。
	targets, err := s.computeTargets(ctx, in.StoreID, ids, 0)
	if err != nil {
		return AllocationReview{}, err
	}
	type cell struct{ binding, sku int64 }
	target := map[cell]listingTarget{}
	for _, t := range targets {
		target[cell{t.binding.ID, t.skuID}] = t
	}
	soldBy, rejectBy := map[cell]int64{}, map[cell]int64{}
	for _, r := range sold {
		soldBy[cell{r.BindingID, r.SKUID}] = r.Qty
	}
	for _, r := range rejects {
		rejectBy[cell{r.BindingID, r.SKUID}] = r.Qty
	}
	zeroBy := map[cell]repository.ChannelZeroHours{}
	for _, z := range zero {
		zeroBy[cell{z.BindingID, z.SKUID}] = z
	}

	for _, k := range infos {
		lv := levels[k.SKUID]
		price := offers[k.SKUID].PriceCents
		sku := AllocationSKU{SKUID: k.SKUID, Title: allocationTitle(k), Available: lv.Available,
			StockoutDays: stockout[k.SKUID], CostMissing: k.CostCents == 0}
		selfQty := lv.Available
		if selfQty < 0 {
			selfQty = 0
		}
		selfSold := soldBy[cell{0, k.SKUID}]
		sku.Channels = append(sku.Channels, AllocationChannel{Name: "自营", PublishedQty: selfQty, Sold: selfSold,
			DailyVelocity: allocationVelocity(selfSold, days, float64(sku.StockoutDays*24)),
			UnitNetCents:  allocationUnitNet(price, 0, k.CostCents)})
		for _, b := range outs {
			t, ok := target[cell{b.ID, k.SKUID}]
			if !ok {
				continue
			}
			id := b.ID
			r, level := channel.ResolveStockRuleLevel(rules[b.ID], in.StoreID, k.SKUID)
			z := zeroBy[cell{b.ID, k.SKUID}]
			n := soldBy[cell{b.ID, k.SKUID}]
			sku.Channels = append(sku.Channels, AllocationChannel{BindingID: &id, Name: b.Name, Rule: &r, RuleLevel: level,
				PublishedQty: t.qty, Sold: n, DailyVelocity: allocationVelocity(n, days, z.HeldHours+z.EmptyHours),
				HeldZeroHours: round2(z.HeldHours), EmptyZeroHours: round2(z.EmptyHours),
				StockoutRejects: rejectBy[cell{b.ID, k.SKUID}],
				UnitNetCents:    allocationUnitNet(t.price, parseBindingConfig(b.Config).CommissionBP, k.CostCents)})
		}
		if sku.Suggestions = suggestAllocation(sku, days); sku.Suggestions == nil {
			sku.Suggestions = []AllocationSuggestion{}
		}
		out.SKUs = append(out.SKUs, sku)
	}
	return out, nil
}

// pickAllocationSKUs 自动挑 SKU：窗口里在任一渠道卖过（按总件数从多到少）、再补挂零过的，至多 50 个。
func pickAllocationSKUs(sold []repository.ChannelSKUQty, zeroSKUs []int64) []int64 {
	total := map[int64]int64{}
	for _, r := range sold {
		total[r.SKUID] += r.Qty
	}
	ids := make([]int64, 0, len(total)+len(zeroSKUs))
	for id := range total {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if total[ids[i]] != total[ids[j]] {
			return total[ids[i]] > total[ids[j]]
		}
		return ids[i] < ids[j]
	})
	for _, id := range zeroSKUs {
		if _, ok := total[id]; !ok {
			ids = append(ids, id)
		}
	}
	if len(ids) > allocationMaxSKUs {
		ids = ids[:allocationMaxSKUs]
	}
	return ids
}

func dedupeIDs(in []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func allocationTitle(k repository.ChannelAllocationSKU) string {
	label := skuLabel(k.SKUCode, k.SpecValues)
	if label == "" {
		return k.ProductTitle
	}
	return k.ProductTitle + " " + label
}

// allocationVelocity：有货时的日均 = 卖出 ÷ (days − 挂零小时 / 24)，分母下限 1，保留两位小数。
func allocationVelocity(sold int64, days int, zeroHours float64) float64 {
	d := math.Max(float64(days)-zeroHours/24, 1)
	return round2(float64(sold) / d)
}

// allocationUnitNet：单件净收入 = 价格 × (1 − 佣金率) − 成本价（成本价 0 不减），四舍五入到分。
func allocationUnitNet(priceCents int64, commissionBP int32, costCents int64) int64 {
	net := int64(math.Round(float64(priceCents) * float64(10000-commissionBP) / 10000))
	if costCents > 0 {
		net -= costCents
	}
	return net
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// suggestAllocation 是基线建议（spec §4.1），纯函数。每个 binding 至多一条（多条规则命中时合成一条、理由连起来），
// 都落在门店 × SKU 级。keel 可售 ≤ 0 不给建议；自营没有规则，不给建议。
//
//  1. 分配造成的挂零 ≥ 24 小时且这个渠道日均 > 0：比例上调 10 个百分点（封顶 100%）；已经 100% 的，
//     把安全库存降到「其它渠道日均 × 0.5」取整（比现在低才降）。
//  2. 有缺货拒单：安全库存 + ceil(拒单件数 / 周数)。
//  3. 货不够分（各渠道日均 × 3 天合计 > 可售）：按单件净收入从高到低先保需求，净收入最低的那个 binding
//     比例下调到「剩下的货」刚好够它对外（只往下调）。
func suggestAllocation(sku AllocationSKU, days int) []AllocationSuggestion {
	if sku.Available <= 0 {
		return nil
	}
	type work struct {
		ch            AllocationChannel
		ratio, safety int32
		why           []string
	}
	var ws []*work
	totalVelocity := 0.0
	for _, c := range sku.Channels {
		totalVelocity += c.DailyVelocity
		if c.BindingID == nil || c.Rule == nil {
			continue
		}
		ws = append(ws, &work{ch: c, ratio: c.Rule.RatioBP, safety: c.Rule.SafetyQty})
	}
	if len(ws) == 0 {
		return nil
	}
	weeks := math.Max(float64(days)/7, 1)
	for _, w := range ws {
		c := w.ch
		if c.HeldZeroHours >= allocationHeldZeroHours && c.DailyVelocity > 0 {
			if w.ratio < 10000 {
				w.ratio = min(w.ratio+allocationRatioStepBP, 10000)
				w.why = append(w.why, fmt.Sprintf("keel 有货、规则却让它挂零了 %.0f 小时，这个渠道日均卖 %.2f 件：比例上调 10 个百分点",
					c.HeldZeroHours, c.DailyVelocity))
			} else if target := int32(math.Floor((totalVelocity - c.DailyVelocity) * 0.5)); target < w.safety {
				w.why = append(w.why, fmt.Sprintf("比例已是 100%%、安全库存 %d 件让它挂零了 %.0f 小时：安全库存降到其它渠道日均的一半（%d 件）",
					w.safety, c.HeldZeroHours, target))
				w.safety = target
			}
		}
		if c.StockoutRejects > 0 {
			add := int32(math.Ceil(float64(c.StockoutRejects) / weeks))
			w.why = append(w.why, fmt.Sprintf("近 %d 天这个渠道有 %d 件缺货拒单：安全库存加 %d 件", days, c.StockoutRejects, add))
			w.safety += add
		}
	}
	// 3. 货不够分。
	demand := func(c AllocationChannel) float64 { return c.DailyVelocity * allocationDemandDays }
	total := 0.0
	for _, c := range sku.Channels {
		total += demand(c)
	}
	if total > float64(sku.Available) {
		var lowest *work
		for _, w := range ws {
			if lowest == nil || w.ch.UnitNetCents < lowest.ch.UnitNetCents {
				lowest = w
			}
		}
		rest := float64(sku.Available) - (total - demand(lowest.ch))
		target := int64(math.Max(math.Floor(rest), 0))
		// published = floor(available × ratio / 10000) − safety ≤ target
		ratio := int32((target + int64(lowest.safety)) * 10000 / int64(sku.Available))
		if ratio < lowest.ratio {
			lowest.why = append(lowest.why, fmt.Sprintf("货不够分：各渠道 3 天需求 %.0f 件 > 可售 %d 件，它单件净收入最低（%d 分），"+
				"先保净收入高的渠道，它的比例下调到对外至多 %d 件", total, sku.Available, lowest.ch.UnitNetCents, target))
			lowest.ratio = max(ratio, 0)
		}
	}
	var out []AllocationSuggestion
	skuID := sku.SKUID
	for _, w := range ws {
		if len(w.why) == 0 || (w.ratio == w.ch.Rule.RatioBP && w.safety == w.ch.Rule.SafetyQty) {
			continue
		}
		out = append(out, AllocationSuggestion{BindingID: *w.ch.BindingID, SKUID: &skuID, RatioBP: w.ratio,
			SafetyQty: w.safety, CapQty: w.ch.Rule.CapQty, Why: strings.Join(w.why, "；")})
	}
	return out
}
