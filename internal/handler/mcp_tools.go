package handler

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// MCP 工具清单（AI 经营 M9，docs/AI经营-M9设计.md §3.1）：读工具与计算工具；提案、简报工具随任务 4–5 加进来。
//
// 每个工具都是「后台同名接口的另一个入口」：调同一个 service 函数（判权相同 —— 门店管理员身份的 AI 员工
// 只看得到它那家店），返回同一个契约类型，错误走同一个错误出口（mcpTool 的 writeErr）。
// 参数名与后台接口的 query 参数一致，agent 看后台接口文档也能用。

// 报表工具共用的时间窗口参数。
type mcpReportWindow struct {
	Period    string `json:"period,omitempty" jsonschema:"统计周期：today / yesterday / last_7_days / last_30_days / custom，默认 last_7_days"`
	StartDate string `json:"start_date,omitempty" jsonschema:"自定义周期（custom）的开始日期 YYYY-MM-DD（店铺时区，含）"`
	EndDate   string `json:"end_date,omitempty" jsonschema:"自定义周期（custom）的结束日期 YYYY-MM-DD（店铺时区，含）"`
	StoreID   *int64 `json:"store_id,omitempty" jsonschema:"只看这家门店；不传则是你管辖范围内的全部"`
	RegionID  *int64 `json:"region_id,omitempty" jsonschema:"只看这个大区"`
}

func (w mcpReportWindow) query() service.ReportQuery {
	p := w.Period
	if p == "" {
		p = service.ReportPeriodLast7Days
	}
	return service.ReportQuery{Period: p, StartDate: w.StartDate, EndDate: w.EndDate, StoreID: w.StoreID, RegionID: w.RegionID}
}

type mcpRanking struct {
	mcpReportWindow
	CategoryID *int64 `json:"category_id,omitempty" jsonschema:"只看这个类目（含子类目）"`
	SortBy     string `json:"sort_by,omitempty" jsonschema:"amount（销售额，默认）或 quantity（销量）"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Top N，默认 10"`
}

type mcpInventoryAlertsIn struct {
	StoreID  *int64 `json:"store_id,omitempty" jsonschema:"只看这家门店"`
	RegionID *int64 `json:"region_id,omitempty" jsonschema:"只看这个大区"`
	Limit    int    `json:"limit,omitempty" jsonschema:"最多返回几条，默认 20"`
}

type mcpSearchIn struct {
	Period    string `json:"period,omitempty" jsonschema:"统计周期，同 shop_overview"`
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
	Limit     int    `json:"limit,omitempty" jsonschema:"每个榜单的 Top N，默认 10"`
}

type mcpPageIn struct {
	Page     int `json:"page,omitempty" jsonschema:"页码，从 1 开始"`
	PageSize int `json:"page_size,omitempty" jsonschema:"每页条数，默认 20，最多 100"`
}

type mcpListStoresIn struct {
	mcpPageIn
	RegionID *int64 `json:"region_id,omitempty" jsonschema:"只看这个大区的门店"`
}

type mcpListProductsIn struct {
	mcpPageIn
	Status     *int16 `json:"status,omitempty" jsonschema:"0 草稿 / 1 上架 / 2 下架；不传则全部"`
	CategoryID *int64 `json:"category_id,omitempty"`
}

type mcpRestockIn struct {
	StoreID      *int64 `json:"store_id,omitempty" jsonschema:"只算这家门店；不传则是你管辖范围内全部营业中的门店"`
	CoverDays    int    `json:"cover_days,omitempty" jsonschema:"补到能卖多少天，1–90，默认 14"`
	LookbackDays int    `json:"lookback_days,omitempty" jsonschema:"按最近多少天的销量算日均，1–90，默认 14"`
	All          bool   `json:"all,omitempty" jsonschema:"true 时连建议补货量为 0 的也返回；默认只返回需要补的"`
}

type mcpGetProductIn struct {
	ProductID int64 `json:"product_id" jsonschema:"商品 id"`
}

type mcpListRefundsIn struct {
	mcpPageIn
	Status  *int16 `json:"status,omitempty" jsonschema:"售后单状态：10 待审核 / 20 待买家退货 / 30 待收货 / 40 已退款 / 50 已驳回 / 60 已撤回"`
	StoreID *int64 `json:"store_id,omitempty"`
}

// 分页结果：items 加分页元数据。
type mcpPage[T any] struct {
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
	Items    []T `json:"items"`
}

type mcpAlerts struct {
	api.ReportInventoryAlerts
}

func registerMCPTools(srv *mcp.Server, d *MCPDeps) {
	mcpTool(srv, d, "shop_overview",
		"经营概览：销售额、单量、客单价、退款等，与上一个同长周期的对比。经营判断的起点。",
		writeAdminListError, func(ctx context.Context, in mcpReportWindow) (api.ReportOverview, error) {
			out, err := d.Reports.Overview(ctx, in.query())
			if err != nil {
				return api.ReportOverview{}, err
			}
			return api.ReportOverview{Window: apiReportWindow(out.Window), Current: apiReportMetrics(out.Current),
				Previous: apiReportMetrics(out.Previous)}, nil
		})
	mcpTool(srv, d, "sales_trend", "按天的销售趋势（销售额、单量）。用来发现某天的异常。",
		writeAdminListError, func(ctx context.Context, in mcpReportWindow) (api.ReportTrend, error) {
			out, err := d.Reports.Trend(ctx, in.query())
			if err != nil {
				return api.ReportTrend{}, err
			}
			return apiReportTrend(out), nil
		})
	mcpTool(srv, d, "product_ranking", "商品排行：按销售额或销量的 Top N。",
		writeAdminListError, func(ctx context.Context, in mcpRanking) (api.ReportProductRanking, error) {
			out, err := d.Reports.Products(ctx, in.query(), in.CategoryID, in.SortBy, in.Limit)
			if err != nil {
				return api.ReportProductRanking{}, err
			}
			return apiReportProducts(out), nil
		})
	mcpTool(srv, d, "store_comparison", "门店对比：各门店的销售额、单量、客单价。",
		writeAdminListError, func(ctx context.Context, in mcpReportWindow) (api.ReportStoreComparison, error) {
			out, err := d.Reports.Stores(ctx, in.query())
			if err != nil {
				return api.ReportStoreComparison{}, err
			}
			return apiReportStores(out), nil
		})
	mcpTool(srv, d, "inventory_alerts", "库存预警：可售数不高于预警线的（门店，SKU）。补货判断的起点。",
		writeAdminListError, func(ctx context.Context, in mcpInventoryAlertsIn) (api.ReportInventoryAlerts, error) {
			out, err := d.Reports.InventoryAlerts(ctx, in.StoreID, in.RegionID, in.Limit)
			if err != nil {
				return api.ReportInventoryAlerts{}, err
			}
			return apiReportAlerts(out), nil
		})
	mcpTool(srv, d, "search_insights", "搜索概况：高频搜索词、无结果词、低点击词。用来发现缺货、标题问题与上新机会。",
		writeAdminListError, func(ctx context.Context, in mcpSearchIn) (api.ReportSearchOverview, error) {
			w := mcpReportWindow{Period: in.Period, StartDate: in.StartDate, EndDate: in.EndDate}
			out, err := d.Reports.Search(ctx, w.query(), in.Limit)
			if err != nil {
				return api.ReportSearchOverview{}, err
			}
			return apiReportSearch(out), nil
		})
	mcpTool(srv, d, "restock_plan",
		"补货计算（确定性，不是估算）：每个（门店，SKU）的日均销量（分母去掉断货天）、可售、可售天数、预计卖断日、"+
			"建议补货量（补到能卖 cover_days 天，取到 5 的倍数）、置信。confidence=low 的样本不足 5 天，只写进简报、不要提案。"+
			"按最先卖断排序。",
		writeAdminListError, func(ctx context.Context, in mcpRestockIn) (service.RestockPlan, error) {
			return d.Restock.Plan(ctx, service.RestockQuery{StoreID: in.StoreID, CoverDays: in.CoverDays,
				LookbackDays: in.LookbackDays, OnlyNeeded: !in.All})
		})
	mcpTool(srv, d, "list_stores", "门店列表（你管辖范围内的），含营业状态、坐标、是否默认店。",
		writeStoreError, func(ctx context.Context, in mcpListStoresIn) (mcpPage[api.AdminStore], error) {
			out, err := d.Stores.ListStores(ctx, in.RegionID, false, in.Page, in.PageSize)
			if err != nil {
				return mcpPage[api.AdminStore]{}, err
			}
			items := make([]api.AdminStore, 0, len(out.Items))
			for _, s := range out.Items {
				items = append(items, apiAdminStore(s))
			}
			return mcpPage[api.AdminStore]{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total), Items: items}, nil
		})
	mcpTool(srv, d, "list_products", "商品列表：标题、状态、价格区间、总库存、销量。",
		writeCatalogError, func(ctx context.Context, in mcpListProductsIn) (mcpPage[api.AdminProduct], error) {
			out, err := d.Catalog.ListProducts(ctx, in.Page, in.PageSize,
				repository.ProductFilter{Status: in.Status, CategoryID: in.CategoryID})
			if err != nil {
				return mcpPage[api.AdminProduct]{}, err
			}
			items := make([]api.AdminProduct, 0, len(out.Items))
			for _, p := range out.Items {
				items = append(items, apiAdminProduct(p))
			}
			return mcpPage[api.AdminProduct]{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total), Items: items}, nil
		})
	mcpTool(srv, d, "get_product", "商品详情：全部 SKU（规格、价格、库存）与图片。",
		writeCatalogError, func(ctx context.Context, in mcpGetProductIn) (api.AdminProductDetail, error) {
			if in.ProductID <= 0 {
				return api.AdminProductDetail{}, fmt.Errorf("%w: product_id 必须是正整数", service.ErrCatalogBadRequest)
			}
			out, err := d.Catalog.FindProduct(ctx, in.ProductID)
			if err != nil {
				return api.AdminProductDetail{}, err
			}
			return apiAdminProductDetail(out), nil
		})
	mcpTool(srv, d, "list_refunds", "售后单列表：状态、类型、金额、原因。",
		writeAdminListError, func(ctx context.Context, in mcpListRefundsIn) (mcpPage[api.AdminRefund], error) {
			out, err := d.Orders.ListRefunds(ctx, repository.AdminRefundFilter{Status: in.Status, StoreID: in.StoreID},
				in.Page, in.PageSize)
			if err != nil {
				return mcpPage[api.AdminRefund]{}, err
			}
			items := make([]api.AdminRefund, 0, len(out.Items))
			for _, r := range out.Items {
				items = append(items, apiAdminRefund(r))
			}
			return mcpPage[api.AdminRefund]{Page: out.Page, PageSize: out.PageSize, Total: int(out.Total), Items: items}, nil
		})
}
