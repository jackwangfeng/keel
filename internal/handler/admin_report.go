package handler

import (
	"strconv"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// 经营报表（GET /api/v1/admin/reports/*，契约 Report tag）。
//
// 六条接口分在五个文件里（admin_report_*.go），这个文件只放它们共用的东西：
// handler 结构体与「service 结果 → 契约类型」的翻译。分文件的理由与 admin_order_list.go
// 相同 —— contract_test.go 的 query 参数对账按文件解析 c.Query 的字面量，
// 参数集合不同的接口放进同一个文件，每一条都会被判成「读了契约里没有的参数」。
// overview 与 trend 的参数一模一样，所以共用一个文件。
//
// 这里没有口径、没有判权、没有 SQL：口径在 service/report.go，判权在 service/authz.go
// （orderListScope / requireMerchantWide），SQL 在 db/queries/reports.sql。

// AdminReportHandler 是经营报表的六条接口。
type AdminReportHandler struct {
	svc *service.ReportService
}

// NewAdminReportHandler 建经营报表 handler。
func NewAdminReportHandler(s *service.ReportService) *AdminReportHandler {
	return &AdminReportHandler{svc: s}
}

// reportQuery 把公共的五个参数收成 service 的形状。参数必须由各文件以字面量读出来
// 再传进来（contract_test.go 的对账读的是字面量）。
//
// store_id / region_id 读不动时当没传，与后台订单列表一致（adminListInt64 的注释）；
// period / 日期写错则是 422，由 service 判 —— 窗口写错时回一份今天的数字会被当真。
func reportQuery(period, startDate, endDate, storeID, regionID string) service.ReportQuery {
	return service.ReportQuery{
		Period: period, StartDate: startDate, EndDate: endDate,
		StoreID: adminListInt64(storeID), RegionID: adminListInt64(regionID),
	}
}

// reportLimit 解析 Top N；读不动时给 0，由 service 钳到默认值。
func reportLimit(raw string) int {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

func apiReportRange(r service.ReportRange) api.ReportRange {
	return api.ReportRange{
		StartAt:   r.Start.UTC(),
		EndAt:     r.End.UTC(),
		StartDate: openapi_types.Date{Time: r.StartDate},
		EndDate:   openapi_types.Date{Time: r.EndDate},
	}
}

func apiReportWindow(w service.ReportWindow) api.ReportWindow {
	return api.ReportWindow{
		Period:   api.ReportWindowPeriod(w.Period),
		Timezone: w.Timezone,
		Current:  apiReportRange(w.Current),
		Previous: apiReportRange(w.Previous),
	}
}

func apiReportMetrics(m service.ReportMetrics) api.ReportMetrics {
	return api.ReportMetrics{
		PaidAmountCents:    m.PaidCents,
		RefundAmountCents:  m.RefundCents,
		NetSalesCents:      m.NetSalesCents,
		OrderCount:         m.OrderCount,
		BuyerCount:         m.BuyerCount,
		AvgOrderValueCents: m.AvgOrderValueCents,
		RefundCount:        m.RefundCount,
		RefundRate:         m.RefundRate,
	}
}

func apiReportTrend(t service.ReportTrend) api.ReportTrend {
	points := make([]api.ReportTrendPoint, 0, len(t.Points))
	for _, p := range t.Points {
		points = append(points, api.ReportTrendPoint{
			BucketStartAt:     p.BucketStart.UTC(),
			Label:             p.Label,
			PaidAmountCents:   p.PaidCents,
			RefundAmountCents: p.RefundCents,
			NetSalesCents:     p.NetSalesCents,
			OrderCount:        p.OrderCount,
		})
	}
	return api.ReportTrend{
		Window:      apiReportWindow(t.Window),
		Granularity: api.ReportTrendGranularity(t.Granularity),
		Points:      points,
	}
}

func apiReportProducts(r service.ReportProductRanking) api.ReportProductRanking {
	items := make([]api.ReportProductRankItem, 0, len(r.Items))
	for i, p := range r.Items {
		items = append(items, api.ReportProductRankItem{
			Rank:                i + 1,
			ProductId:           p.ProductID,
			Title:               p.Title,
			CategoryId:          p.CategoryID,
			Quantity:            p.Quantity,
			AmountCents:         p.AmountCents,
			OrderCount:          p.OrderCount,
			RefundedQuantity:    p.RefundedQuantity,
			RefundedAmountCents: p.RefundedAmountCents,
		})
	}
	return api.ReportProductRanking{
		Window: apiReportWindow(r.Window),
		SortBy: api.ReportProductRankingSortBy(r.SortBy),
		Items:  items,
	}
}

func apiReportStores(r service.ReportStoreComparison) api.ReportStoreComparison {
	stores := make([]api.ReportStoreRow, 0, len(r.Stores))
	for _, s := range r.Stores {
		stores = append(stores, api.ReportStoreRow{
			StoreId: s.StoreID, StoreName: s.StoreName, StoreCode: s.StoreCode,
			RegionId: s.RegionID, RegionName: s.RegionName, Deleted: s.Deleted,
			PaidAmountCents: s.PaidCents, RefundAmountCents: s.RefundCents,
			NetSalesCents: s.PaidCents - s.RefundCents, OrderCount: s.OrderCount,
		})
	}
	regions := make([]api.ReportRegionRow, 0, len(r.Regions))
	for _, g := range r.Regions {
		regions = append(regions, api.ReportRegionRow{
			RegionId: g.RegionID, RegionName: g.RegionName, StoreCount: g.StoreCount,
			PaidAmountCents: g.PaidCents, RefundAmountCents: g.RefundCents,
			NetSalesCents: g.PaidCents - g.RefundCents, OrderCount: g.OrderCount,
		})
	}
	return api.ReportStoreComparison{Window: apiReportWindow(r.Window), Stores: stores, Regions: regions}
}

func apiReportAlerts(r service.ReportInventoryAlerts) api.ReportInventoryAlerts {
	items := make([]api.ReportInventoryAlert, 0, len(r.Items))
	for _, a := range r.Items {
		items = append(items, api.ReportInventoryAlert{
			StoreId: a.StoreID, StoreName: a.StoreName, RegionId: a.RegionID,
			SkuId: a.SKUID, SkuCode: a.SKUCode, SpecValues: a.SpecValues,
			ProductId: a.ProductID, ProductTitle: a.ProductTitle,
			AvailableQty: a.AvailableQty, WarningQty: a.WarningQty,
		})
	}
	return api.ReportInventoryAlerts{Total: r.Total, Items: items}
}

func apiReportTerms(ts []repository.ReportSearchTerm) []api.ReportSearchTerm {
	out := make([]api.ReportSearchTerm, 0, len(ts))
	for _, t := range ts {
		out = append(out, api.ReportSearchTerm{Query: t.Term, SearchCount: t.SearchCount, ZeroResultCount: t.ZeroResultCount})
	}
	return out
}

func apiReportSearch(r service.ReportSearchOverview) api.ReportSearchOverview {
	return api.ReportSearchOverview{
		Window:            apiReportWindow(r.Window),
		SearchCount:       r.Totals.SearchCount,
		ZeroResultCount:   r.Totals.ZeroResultCount,
		ZeroResultRate:    r.ZeroResultRate,
		ClickCount:        r.Totals.ClickCount,
		TopQueries:        apiReportTerms(r.TopQueries),
		ZeroResultQueries: apiReportTerms(r.ZeroQueries),
	}
}
