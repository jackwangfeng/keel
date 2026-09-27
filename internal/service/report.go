package service

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	// 把 IANA 时区库编进二进制（约 450 KB）。经营报表按店铺时区切「今天」，
	// 而运行镜像（debian:trixie-slim）不保证装了 tzdata：没有它时 time.LoadLocation
	// 对任何时区名都失败，报表会悄悄全部按回落时区算 —— 连回落的 Asia/Shanghai
	// 本身也加载不出来。编进来之后结果不再取决于部署机器上装了什么。
	_ "time/tzdata"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 经营报表（GET /admin/reports/*，契约 Report tag；迁移 00057 的索引）。
//
// # 口径只有一份
//
// 契约 ReportWindow / ReportMetrics 的描述是唯一真相源，这里照做：
//
//   - 时区：店铺设置的 timezone（shop_preferences，00059），没有那一行或不是合法的 IANA 名字时按 Asia/Shanghai；
//   - 窗口：today 是今天 0 点到此刻、yesterday 是昨天、last_7_days / last_30_days 是
//     最近 7 / 30 个**完整**自然日（不含今天）、custom 是 [start_date 0 点, end_date 次日 0 点)；
//   - 上一周期：紧挨着的等长一段；today 对比昨天的同一时段；
//   - 销售按支付时间、退款按到账时间落窗口（SQL 那一侧，db/queries/reports.sql）。
//
// 窗口与趋势的桶边界**全部在这里算**，SQL 只拿到一串算好的 timestamptz：
// 时区、夏令时、「一天有几个小时」只有这一份实现（resolveReportWindow），
// 数据库会话的 TimeZone 设置影响不到结果。
//
// # 为什么自定义窗口最多 366 天
//
// 每条报表都是对窗口内的明细现场聚合（没有汇总表，理由在迁移 00057 的文件头），
// 扫描量与窗口长度成正比。366 天覆盖「今年以来」「同比去年」这两种最长的日常问法；
// 再长的分析该去离线数仓，不该压在交易库上。上限挡在进事务之前，一次写错的
// 「2000-01-01 到今天」不会先扫二十年再报错。
//
// # 范围
//
// 与后台订单列表同一个判据：orderListScope（authz.go）。列表里看得见的单，报表里就算得进；
// 看不见的，一分钱都不进。搜索概况例外 —— 检索日志没有门店维度，只放全店范围的人
// （requireMerchantWide），理由写在契约 GET /admin/reports/search 上。
//
// # 为什么每条报表在一个事务里
//
// 概览要读本期与上期、支付与退款四个数；分开读的话，一次支付回调落在两次读之间，
// 「销售额」与「订单数」就不是同一个时刻的数。WithTenant 开的是一个事务，
// 读已提交级别下每条语句各自取快照 —— 这挡不住那种错位，但也不必更强：报表本来
// 就是一个时刻的近似，真正要挡的是「本期用了 A 口径、上期用了 B 口径」，那由
// 共用同一段 SQL 保证。

const (
	// ReportMaxCustomDays 是 period=custom 时起止（含首尾）最多跨几天。
	ReportMaxCustomDays = 366
	// ReportDefaultLimit / ReportMaxLimit 是 Top N 的默认值与上限（契约 ReportLimit）。
	ReportDefaultLimit = 10
	ReportMaxLimit     = 50
	// ReportAlertDefaultLimit / ReportAlertMaxLimit 是库存预警的条数（契约 inventory-alerts 的 limit）。
	ReportAlertDefaultLimit = 50
	ReportAlertMaxLimit     = 200
)

// 契约 ReportPeriod 的五个值。
const (
	ReportPeriodToday      = "today"
	ReportPeriodYesterday  = "yesterday"
	ReportPeriodLast7Days  = "last_7_days"
	ReportPeriodLast30Days = "last_30_days"
	ReportPeriodCustom     = "custom"
)

// ReportRepo 是报表要的两样：租户事务与店铺时区。
type ReportRepo interface {
	tenantRunner
	ShopTimezone(ctx context.Context) (string, error)
}

// ReportService 实现六条报表。
type ReportService struct {
	repo ReportRepo
	// inv 是库存服务（微服务拆分阶段 1a）：库存预警的行由它给，core 只补名字。
	inv inventory.Service
	now func() time.Time
}

// NewReportService 建报表服务。
func NewReportService(r ReportRepo, inv inventory.Service) *ReportService {
	return &ReportService{repo: r, inv: inv, now: time.Now}
}

// ReportQuery 是报表的公共参数（契约 ReportPeriod / ReportStartDate / ReportEndDate /
// ReportStoreId / ReportRegionId）。空串的 Period 即 today（契约的 default）。
type ReportQuery struct {
	Period             string
	StartDate, EndDate string
	StoreID, RegionID  *int64
}

// ReportRange 是一段半开区间 [Start, End)，以及它在店铺时区里的起止日期（都含）。
type ReportRange struct {
	Start, End         time.Time
	StartDate, EndDate time.Time // 店铺时区里那一天的 0 点；只用它的年月日
}

// ReportWindow 是解析好的时间窗口。
type ReportWindow struct {
	Period   string
	Timezone string
	Location *time.Location
	Current  ReportRange
	Previous ReportRange
}

// ReportMetrics 是一段窗口里的核心指标（契约 ReportMetrics）。
type ReportMetrics struct {
	PaidCents, RefundCents, NetSalesCents int64
	OrderCount, BuyerCount, RefundCount   int64
	AvgOrderValueCents                    int64
	RefundRate                            *float64
}

// ReportOverview 是经营概览。
type ReportOverview struct {
	Window            ReportWindow
	Current, Previous ReportMetrics
}

// ReportTrendPoint 是趋势里的一个点。
type ReportTrendPoint struct {
	BucketStart                           time.Time
	Label                                 string
	PaidCents, RefundCents, NetSalesCents int64
	OrderCount                            int64
}

// ReportTrend 是销售趋势。Granularity 是 "hour" 或 "day"。
type ReportTrend struct {
	Window      ReportWindow
	Granularity string
	Points      []ReportTrendPoint
}

// ReportProductRanking 是商品排行。
type ReportProductRanking struct {
	Window ReportWindow
	SortBy string
	Items  []repository.ReportProductRow
}

// ReportRegionRow 是门店对比里按大区的合计。
type ReportRegionRow struct {
	RegionID               int64
	RegionName             string
	StoreCount             int
	OrderCount             int64
	PaidCents, RefundCents int64
}

// ReportStoreComparison 是门店 / 大区对比。
type ReportStoreComparison struct {
	Window  ReportWindow
	Stores  []repository.ReportStoreRow
	Regions []ReportRegionRow
}

// ReportInventoryAlerts 是库存预警。
type ReportInventoryAlerts struct {
	Total int64
	Items []repository.ReportInventoryAlert
}

// ReportSearchOverview 是搜索概况。
type ReportSearchOverview struct {
	Window                  ReportWindow
	Totals                  repository.ReportSearchTotals
	ZeroResultRate          *float64
	TopQueries, ZeroQueries []repository.ReportSearchTerm
}

// ---------------------------------------------------------------------------
// 六条报表
// ---------------------------------------------------------------------------

// Overview 实现 GET /admin/reports/overview。
func (s *ReportService) Overview(ctx context.Context, q ReportQuery) (ReportOverview, error) {
	w, err := s.window(ctx, q)
	if err != nil {
		return ReportOverview{}, err
	}
	out := ReportOverview{Window: w}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if out.Current, err = reportMetrics(ctx, tx, filterOf(q, w.Current, only)); err != nil {
			return err
		}
		out.Previous, err = reportMetrics(ctx, tx, filterOf(q, w.Previous, only))
		return err
	})
	if err != nil {
		return ReportOverview{}, err
	}
	return out, nil
}

// Trend 实现 GET /admin/reports/trend。
func (s *ReportService) Trend(ctx context.Context, q ReportQuery) (ReportTrend, error) {
	w, err := s.window(ctx, q)
	if err != nil {
		return ReportTrend{}, err
	}
	gran, edges := reportBuckets(w)
	out := ReportTrend{Window: w, Granularity: gran, Points: make([]ReportTrendPoint, 0, len(edges))}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		buckets, err := tx.ReportBuckets(ctx, filterOf(q, w.Current, only), edges)
		if err != nil {
			return err
		}
		for i, b := range buckets {
			out.Points = append(out.Points, ReportTrendPoint{
				BucketStart: edges[i],
				Label:       bucketLabel(edges[i], w.Location, gran),
				PaidCents:   b.PaidCents, RefundCents: b.RefundCents,
				NetSalesCents: b.PaidCents - b.RefundCents,
				OrderCount:    b.OrderCount,
			})
		}
		return nil
	})
	if err != nil {
		return ReportTrend{}, err
	}
	return out, nil
}

// Products 实现 GET /admin/reports/products。sortBy 不认识的值按 amount（契约的 default）。
func (s *ReportService) Products(ctx context.Context, q ReportQuery, categoryID *int64,
	sortBy string, limit int) (ReportProductRanking, error) {

	w, err := s.window(ctx, q)
	if err != nil {
		return ReportProductRanking{}, err
	}
	if sortBy != "quantity" {
		sortBy = "amount"
	}
	limit = clampLimit(limit, ReportDefaultLimit, ReportMaxLimit)
	out := ReportProductRanking{Window: w, SortBy: sortBy}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		out.Items, err = tx.ReportProductRanking(ctx, filterOf(q, w.Current, only), categoryID, sortBy, limit)
		return err
	})
	if err != nil {
		return ReportProductRanking{}, err
	}
	return out, nil
}

// Stores 实现 GET /admin/reports/stores。
func (s *ReportService) Stores(ctx context.Context, q ReportQuery) (ReportStoreComparison, error) {
	w, err := s.window(ctx, q)
	if err != nil {
		return ReportStoreComparison{}, err
	}
	out := ReportStoreComparison{Window: w}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		f := filterOf(q, w.Current, only)
		f.StoreID = nil // 门店对比本来就按门店展开；契约里这一条没有 store_id
		out.Stores, err = tx.ReportStoreComparison(ctx, f)
		return err
	})
	if err != nil {
		return ReportStoreComparison{}, err
	}
	out.Regions = rollupRegions(out.Stores)
	return out, nil
}

// InventoryAlerts 实现 GET /admin/reports/inventory-alerts。
//
// 拆分前是一条查询（inventories JOIN stores / skus / products）。本轮（微服务拆分阶段 1a）
// 三段，结果与拆分前逐行相同：
//
//  1. core 事务：门店范围（未软删、落在 store_id / region_id 筛选与员工范围里的门店）
//     与要排除的 SKU（软删的、软删商品下的）—— 拆分前那几个 JOIN 条件做的就是这两件；
//  2. 库存服务按「显式的门店 id 列表 + 排除列表」取前 limit 条与总数（排序键不变）；
//  3. core 事务：给这一页补货号、规格、商品名（门店名第 1 段已经有了）。
//
// 员工范围在第 1 段就收成了门店 id 列表，所以库存服务那一侧不需要认识员工，
// 也不可能「忘了带范围」—— 空列表在它那里就是一条都没有。
//
// 库存服务不可用（拆分形态）时 503：total 与 items 在契约里都是必填，编不出来。
func (s *ReportService) InventoryAlerts(ctx context.Context, storeID, regionID *int64, limit int) (ReportInventoryAlerts, error) {
	if _, err := requireStaff(ctx); err != nil {
		return ReportInventoryAlerts{}, err
	}
	limit = clampLimit(limit, ReportAlertDefaultLimit, ReportAlertMaxLimit)
	var (
		stores  []repository.ReportAlertStore
		exclude []int64
	)
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		only, err := orderListScope(ctx)
		if err != nil {
			return err
		}
		if stores, err = tx.ReportAlertStores(ctx,
			repository.ReportFilter{StoreID: storeID, RegionID: regionID, Only: only}); err != nil {
			return err
		}
		exclude, err = tx.ReportAlertExcludedSKUs(ctx)
		return err
	})
	if err != nil {
		return ReportInventoryAlerts{}, err
	}
	out := ReportInventoryAlerts{Items: []repository.ReportInventoryAlert{}}
	if len(stores) == 0 {
		return out, nil
	}

	byStore := make(map[int64]repository.ReportAlertStore, len(stores))
	storeIDs := make([]int64, 0, len(stores))
	for _, st := range stores {
		byStore[st.ID] = st
		storeIDs = append(storeIDs, st.ID)
	}
	page, err := s.inv.LowStock(ctx, inventory.LowStockQuery{
		StoreIDs: storeIDs, ExcludeSKUIDs: exclude, Limit: limit,
	})
	if err != nil {
		return ReportInventoryAlerts{}, err
	}
	out.Total = page.Total
	if len(page.Items) == 0 {
		return out, nil
	}

	skuIDs := make([]int64, 0, len(page.Items))
	for _, it := range page.Items {
		skuIDs = append(skuIDs, it.SKUID)
	}
	var info map[int64]repository.ReportAlertSKU
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		info, err = tx.ReportAlertSKUInfo(ctx, skuIDs)
		return err
	}); err != nil {
		return ReportInventoryAlerts{}, err
	}
	for _, it := range page.Items {
		sk, ok := info[it.SKUID]
		st := byStore[it.StoreID]
		if !ok {
			// 库存有行、core 没有这个 SKU：单体形态下外键让它不可能；拆分形态下是孤儿行
			// （对账任务的事，阶段 2）。不编名字，跳过这一行 —— total 里仍算着它，
			// 与「库存服务说有这么多条」一致。
			continue
		}
		out.Items = append(out.Items, repository.ReportInventoryAlert{
			StoreID: it.StoreID, StoreName: st.Name, RegionID: st.RegionID,
			SKUID: it.SKUID, SKUCode: sk.SKUCode, SpecValues: sk.SpecValues,
			ProductID: sk.ProductID, ProductTitle: sk.ProductTitle,
			AvailableQty: int(it.Available), WarningQty: int(it.Warning),
		})
	}
	return out, nil
}

// Search 实现 GET /admin/reports/search。只放全店范围的人。
func (s *ReportService) Search(ctx context.Context, q ReportQuery, limit int) (ReportSearchOverview, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return ReportSearchOverview{}, err
	}
	w, err := s.window(ctx, q)
	if err != nil {
		return ReportSearchOverview{}, err
	}
	limit = clampLimit(limit, ReportDefaultLimit, ReportMaxLimit)
	out := ReportSearchOverview{Window: w}
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		f := repository.ReportFilter{Start: w.Current.Start, End: w.Current.End}
		var err error
		if out.Totals, err = tx.ReportSearchTotals(ctx, f); err != nil {
			return err
		}
		if out.TopQueries, err = tx.ReportSearchTerms(ctx, f, false, limit); err != nil {
			return err
		}
		out.ZeroQueries, err = tx.ReportSearchTerms(ctx, f, true, limit)
		return err
	})
	if err != nil {
		return ReportSearchOverview{}, err
	}
	out.ZeroResultRate = ratio(out.Totals.ZeroResultCount, out.Totals.SearchCount)
	return out, nil
}

// ---------------------------------------------------------------------------
// 口径的实现
// ---------------------------------------------------------------------------

// reportMetrics 读一段窗口的支付与退款，派生出其余指标。
func reportMetrics(ctx context.Context, tx repository.Tx, f repository.ReportFilter) (ReportMetrics, error) {
	o, err := tx.ReportOrderTotals(ctx, f)
	if err != nil {
		return ReportMetrics{}, err
	}
	r, err := tx.ReportRefundTotals(ctx, f)
	if err != nil {
		return ReportMetrics{}, err
	}
	return deriveMetrics(o, r), nil
}

// deriveMetrics 是契约 ReportMetrics 里全部派生指标的唯一实现：
// 净销售额 = 支付 − 退款；客单价 = 支付 ÷ 买家数（四舍五入到分，没有买家时 0）；
// 退款率 = 退款 ÷ 支付（支付为 0 时 nil，不是 0）。
func deriveMetrics(o repository.ReportOrderTotals, r repository.ReportRefundTotals) ReportMetrics {
	m := ReportMetrics{
		PaidCents: o.PaidCents, RefundCents: r.RefundCents,
		NetSalesCents: o.PaidCents - r.RefundCents,
		OrderCount:    o.OrderCount, BuyerCount: o.BuyerCount, RefundCount: r.RefundCount,
		RefundRate: ratio(r.RefundCents, o.PaidCents),
	}
	if o.BuyerCount > 0 {
		// 整数的四舍五入（半数进位）：金额非负，(2a + b) / 2b 就是 round(a / b)。
		// 不走浮点：int64 分到 float64 在超过 2^53 分（九百万亿元）之前是精确的，
		// 但「金额禁止用浮点」是契约第一条约定，这里不开例外。
		m.AvgOrderValueCents = (2*o.PaidCents + o.BuyerCount) / (2 * o.BuyerCount)
	}
	return m
}

// ratio 是 a ÷ b，b 为 0 时 nil。
func ratio(a, b int64) *float64 {
	if b == 0 {
		return nil
	}
	v := float64(a) / float64(b)
	return &v
}

func filterOf(q ReportQuery, r ReportRange, only repository.ScopeFilter) repository.ReportFilter {
	return repository.ReportFilter{Start: r.Start, End: r.End, StoreID: q.StoreID, RegionID: q.RegionID, Only: only}
}

func clampLimit(v, def, max int) int {
	if v < 1 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// rollupRegions 把门店行按大区合计（门店此刻所属的大区）。排序与门店相同：
// 净销售额倒序，并列按大区 id 升序。
func rollupRegions(stores []repository.ReportStoreRow) []ReportRegionRow {
	byID := map[int64]*ReportRegionRow{}
	order := []int64{}
	for _, st := range stores {
		r, ok := byID[st.RegionID]
		if !ok {
			r = &ReportRegionRow{RegionID: st.RegionID, RegionName: st.RegionName}
			byID[st.RegionID] = r
			order = append(order, st.RegionID)
		}
		r.StoreCount++
		r.OrderCount += st.OrderCount
		r.PaidCents += st.PaidCents
		r.RefundCents += st.RefundCents
	}
	out := make([]ReportRegionRow, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		ni, nj := out[i].PaidCents-out[i].RefundCents, out[j].PaidCents-out[j].RefundCents
		if ni != nj {
			return ni > nj
		}
		return out[i].RegionID < out[j].RegionID
	})
	return out
}

// window 判身份、读店铺时区、解析窗口。判身份排在最前：没有后台会话的请求
// 不该先去读一次 shop_settings。
func (s *ReportService) window(ctx context.Context, q ReportQuery) (ReportWindow, error) {
	if _, err := requireStaff(ctx); err != nil {
		return ReportWindow{}, err
	}
	name, err := s.repo.ShopTimezone(ctx)
	if err != nil {
		return ReportWindow{}, err
	}
	name, loc := reportLocation(name)
	return resolveReportWindow(q.Period, q.StartDate, q.EndDate, s.now(), name, loc)
}

// reportLocation 把店铺时区名换成 *time.Location；不是合法的 IANA 名字（空串、拼错、
// 数据库里被人手工写坏）时回落到 Asia/Shanghai，并回显实际用的那一个 ——
// 界面上写着的时区必须是真正参与计算的时区。
func reportLocation(name string) (string, *time.Location) {
	if strings.TrimSpace(name) != "" && !strings.EqualFold(name, "local") {
		if loc, err := time.LoadLocation(name); err == nil {
			return name, loc
		}
	}
	loc, err := time.LoadLocation(repository.DefaultShopTimezone)
	if err != nil {
		// time/tzdata 编进来了，走到这里只能是那个 import 被人删了。
		panic(fmt.Sprintf("加载默认店铺时区 %s 失败（time/tzdata 没编进来？）: %v", repository.DefaultShopTimezone, err))
	}
	return repository.DefaultShopTimezone, loc
}

// midnight 是 t 在 loc 里那一天的 0 点。
func midnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// dayRange 是 [from 那一天 0 点, 再往后 days 天的 0 点)。按日历加天数，不按 24 小时：
// 夏令时切换那一天有 23 或 25 个小时。
func dayRange(from time.Time, days int) ReportRange {
	end := from.AddDate(0, 0, days)
	return ReportRange{Start: from, End: end, StartDate: from, EndDate: end.AddDate(0, 0, -1)}
}

// resolveReportWindow 是契约 ReportWindow 那段口径的唯一实现。纯函数，now 由调用方给。
func resolveReportWindow(period, startDate, endDate string, now time.Time,
	tzName string, loc *time.Location) (ReportWindow, error) {

	if period == "" {
		period = ReportPeriodToday
	}
	w := ReportWindow{Period: period, Timezone: tzName, Location: loc}
	today := midnight(now, loc)
	switch period {
	case ReportPeriodToday:
		// 本期：今天 0 点 → 此刻；上期：昨天 0 点 → 昨天的此刻（按日历减一天，墙上时间相同）。
		yesterday := today.AddDate(0, 0, -1)
		w.Current = ReportRange{Start: today, End: now, StartDate: today, EndDate: today}
		w.Previous = ReportRange{Start: yesterday, End: now.In(loc).AddDate(0, 0, -1),
			StartDate: yesterday, EndDate: yesterday}
	case ReportPeriodYesterday:
		w.Current = dayRange(today.AddDate(0, 0, -1), 1)
		w.Previous = dayRange(today.AddDate(0, 0, -2), 1)
	case ReportPeriodLast7Days, ReportPeriodLast30Days:
		n := 7
		if period == ReportPeriodLast30Days {
			n = 30
		}
		w.Current = dayRange(today.AddDate(0, 0, -n), n)
		w.Previous = dayRange(today.AddDate(0, 0, -2*n), n)
	case ReportPeriodCustom:
		if startDate == "" || endDate == "" {
			return ReportWindow{}, fmt.Errorf("%w: period=custom 必须同时给 start_date 与 end_date", ErrAdminListBadRequest)
		}
		s, err := time.ParseInLocation(time.DateOnly, startDate, loc)
		if err != nil {
			return ReportWindow{}, fmt.Errorf("%w: start_date 不是合法的日期（形如 2026-09-01）：%q", ErrAdminListBadRequest, startDate)
		}
		e, err := time.ParseInLocation(time.DateOnly, endDate, loc)
		if err != nil {
			return ReportWindow{}, fmt.Errorf("%w: end_date 不是合法的日期（形如 2026-09-01）：%q", ErrAdminListBadRequest, endDate)
		}
		days := civilDays(s, e) + 1
		if days < 1 {
			return ReportWindow{}, fmt.Errorf("%w: start_date %s 晚于 end_date %s", ErrAdminListBadRequest, startDate, endDate)
		}
		if days > ReportMaxCustomDays {
			return ReportWindow{}, fmt.Errorf("%w: 自定义时间窗口最多 %d 天，%s 到 %s 是 %d 天",
				ErrAdminListBadRequest, ReportMaxCustomDays, startDate, endDate, days)
		}
		w.Current = dayRange(s, days)
		w.Previous = dayRange(s.AddDate(0, 0, -days), days)
	default:
		return ReportWindow{}, fmt.Errorf("%w: period 只能是 today / yesterday / last_7_days / last_30_days / custom，收到 %q",
			ErrAdminListBadRequest, period)
	}
	return w, nil
}

// civilDays 是两个日期（各自那一天的 0 点）之间隔了几个日历日：b − a。
// 按年月日换到 UTC 再相减，与夏令时无关。
func civilDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	ua := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	ub := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)
	return int(math.Round(ub.Sub(ua).Hours() / 24))
}

// reportBuckets 给出趋势的粒度与每个桶的起点（升序）。
//
// 窗口只覆盖一个自然日时按小时：从那天 0 点起每次加一个**真实**小时，直到窗口终点
// （today 的终点是此刻，于是最后一桶是当前这一小时）。夏令时那天因此是 23 或 25 个桶，
// 每个桶都恰好是一个小时，标签按墙上时间写（会出现两个 01:00 或缺一个 02:00）——
// 那正是那一天实际发生的事。
// 否则按天：每个桶是店铺时区里的一个自然日。
func reportBuckets(w ReportWindow) (string, []time.Time) {
	edges := []time.Time{}
	if civilDays(w.Current.StartDate, w.Current.EndDate) == 0 {
		for t := w.Current.Start; t.Before(w.Current.End); t = t.Add(time.Hour) {
			edges = append(edges, t)
		}
		return "hour", edges
	}
	for t := w.Current.Start; t.Before(w.Current.End); t = t.AddDate(0, 0, 1) {
		edges = append(edges, t)
	}
	return "day", edges
}

func bucketLabel(t time.Time, loc *time.Location, gran string) string {
	if gran == "hour" {
		return t.In(loc).Format("15:00")
	}
	return t.In(loc).Format("01-02")
}
