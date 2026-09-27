package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/keel/keel/internal/repository/internal/db"
)

// 经营报表（GET /admin/reports/*，契约 Report tag）在 repository 边界上的那一面。
//
// 全部是只读聚合（db/queries/reports.sql）。这一层不懂口径也不懂时区：窗口的起止、
// 趋势的桶边界都由 service 算好传进来，这里只负责把它们与范围过滤原样放进 SQL。
// 租户仍然只由 RLS 管（check_query_tenancy.py）。

// ReportFilter 是报表的时间窗口与筛选：半开区间 [Start, End)，StoreID / RegionID
// 为 nil 即不筛。Only 是同一租户内的范围（service/authz.go 的 orderListScope）。
type ReportFilter struct {
	Start, End time.Time
	StoreID    *int64
	RegionID   *int64
	Only       ScopeFilter
}

// ReportOrderTotals 是窗口内已支付订单的合计。
type ReportOrderTotals struct {
	OrderCount int64
	BuyerCount int64
	PaidCents  int64
}

// ReportRefundTotals 是窗口内到账退款的合计。
type ReportRefundTotals struct {
	RefundCount int64
	RefundCents int64
}

// ReportBucket 是趋势里的一个桶（Index 从 0 开始，对应 service 给的第几个边界）。
type ReportBucket struct {
	Index       int
	OrderCount  int64
	PaidCents   int64
	RefundCents int64
}

// ReportProductRow 是商品排行的一行。
type ReportProductRow struct {
	ProductID           int64
	Title               string
	CategoryID          int64
	Quantity            int64
	AmountCents         int64
	OrderCount          int64
	RefundedQuantity    int64
	RefundedAmountCents int64
}

// ReportStoreRow 是门店对比的一行。
type ReportStoreRow struct {
	StoreID     int64
	StoreName   string
	StoreCode   string
	RegionID    int64
	RegionName  string
	Deleted     bool
	OrderCount  int64
	PaidCents   int64
	RefundCents int64
}

// ReportInventoryAlert 是库存预警的一行。
type ReportInventoryAlert struct {
	StoreID      int64
	StoreName    string
	RegionID     int64
	SKUID        int64
	SKUCode      string
	SpecValues   map[string]string
	ProductID    int64
	ProductTitle string
	AvailableQty int
	WarningQty   int
}

// ReportAlertStore 是库存预警门店范围里的一家店。
type ReportAlertStore struct {
	ID       int64
	Name     string
	RegionID int64
}

// ReportAlertSKU 是给库存预警补名字用的 SKU 信息。
type ReportAlertSKU struct {
	SKUCode      string
	SpecValues   map[string]string
	ProductID    int64
	ProductTitle string
}

// ReportSearchTotals 是窗口内检索日志的合计。
type ReportSearchTotals struct {
	SearchCount     int64
	ZeroResultCount int64
	ClickCount      int64
}

// ReportSearchTerm 是一个归并后的搜索词。
type ReportSearchTerm struct {
	Term            string
	SearchCount     int64
	ZeroResultCount int64
}

// ReportTx 是经营报表的读这一面。
type ReportTx interface {
	ReportOrderTotals(ctx context.Context, f ReportFilter) (ReportOrderTotals, error)
	ReportRefundTotals(ctx context.Context, f ReportFilter) (ReportRefundTotals, error)
	// ReportBuckets 按 edges 分桶（edges[i] 是第 i 桶的起点，升序；最后一桶止于 f.End）。
	// 返回的切片与 edges 等长，没有数据的桶是零值。
	ReportBuckets(ctx context.Context, f ReportFilter, edges []time.Time) ([]ReportBucket, error)
	// ReportProductRanking sortBy 是 "amount" 或 "quantity"。
	ReportProductRanking(ctx context.Context, f ReportFilter, categoryID *int64, sortBy string, limit int) ([]ReportProductRow, error)
	// ReportStoreComparison 不看 f.StoreID（门店对比本来就是按门店展开的）。
	ReportStoreComparison(ctx context.Context, f ReportFilter) ([]ReportStoreRow, error)
	// 库存预警的 core 那一半（微服务拆分阶段 1a）：预警行本身由库存服务给
	// （inventory.Service.LowStock），这三条回答库存服务回答不了的事 ——
	// 门店范围、要排除的 SKU、补名字。三条都不看 f.Start / f.End（库存是现状）。
	//
	// ReportAlertStores 是未软删、落在 f.StoreID / f.RegionID / f.Only 里的门店。
	ReportAlertStores(ctx context.Context, f ReportFilter) ([]ReportAlertStore, error)
	// ReportAlertExcludedSKUs 是软删 SKU 与软删商品下的 SKU。
	ReportAlertExcludedSKUs(ctx context.Context) ([]int64, error)
	// ReportAlertSKUInfo 按 id 取货号、规格、商品与商品名。查不到的 id 不在结果里。
	ReportAlertSKUInfo(ctx context.Context, skuIDs []int64) (map[int64]ReportAlertSKU, error)
	// ReportSearchTotals / ReportSearchTerms 只看 f.Start / f.End（检索日志没有门店维度）。
	ReportSearchTotals(ctx context.Context, f ReportFilter) (ReportSearchTotals, error)
	ReportSearchTerms(ctx context.Context, f ReportFilter, onlyZero bool, limit int) ([]ReportSearchTerm, error)
}

func checkLimit(limit int) error {
	if limit < 1 || limit > math.MaxInt32 {
		return fmt.Errorf("limit %d 超出范围 [1, %d]", limit, math.MaxInt32)
	}
	return nil
}

func (t tenantTx) ReportOrderTotals(ctx context.Context, f ReportFilter) (ReportOrderTotals, error) {
	r, err := t.q.ReportOrderTotals(ctx, db.ReportOrderTotalsParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return ReportOrderTotals{}, err
	}
	return ReportOrderTotals(r), nil
}

func (t tenantTx) ReportRefundTotals(ctx context.Context, f ReportFilter) (ReportRefundTotals, error) {
	r, err := t.q.ReportRefundTotals(ctx, db.ReportRefundTotalsParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return ReportRefundTotals{}, err
	}
	return ReportRefundTotals(r), nil
}

func (t tenantTx) ReportBuckets(ctx context.Context, f ReportFilter, edges []time.Time) ([]ReportBucket, error) {
	if len(edges) == 0 {
		return []ReportBucket{}, nil
	}
	pgEdges := make([]pgtype.Timestamptz, len(edges))
	for i, e := range edges {
		pgEdges[i] = ts(e)
	}
	out := make([]ReportBucket, len(edges))
	for i := range out {
		out[i].Index = i
	}
	// width_bucket 返回 1..n（n = len(edges)）；窗口谓词保证不会落到 0 或 n+1 以外，
	// 真落出去了说明 service 给的 edges 与窗口对不上 —— 报出来，不要悄悄丢数据。
	slot := func(b int32) (int, error) {
		i := int(b) - 1
		if i < 0 || i >= len(out) {
			return 0, fmt.Errorf("报表分桶越界：第 %d 桶（共 %d 桶），edges 与窗口 [%s, %s) 不一致",
				b, len(out), f.Start.Format(time.RFC3339), f.End.Format(time.RFC3339))
		}
		return i, nil
	}
	orders, err := t.q.ReportOrderBuckets(ctx, db.ReportOrderBucketsParams{
		Edges: pgEdges, WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range orders {
		i, err := slot(r.Bucket)
		if err != nil {
			return nil, err
		}
		out[i].OrderCount, out[i].PaidCents = r.OrderCount, r.PaidCents
	}
	refunds, err := t.q.ReportRefundBuckets(ctx, db.ReportRefundBucketsParams{
		Edges: pgEdges, WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range refunds {
		i, err := slot(r.Bucket)
		if err != nil {
			return nil, err
		}
		out[i].RefundCents = r.RefundCents
	}
	return out, nil
}

func (t tenantTx) ReportProductRanking(ctx context.Context, f ReportFilter, categoryID *int64,
	sortBy string, limit int) ([]ReportProductRow, error) {
	if err := checkLimit(limit); err != nil {
		return nil, err
	}
	rows, err := t.q.ReportProductRanking(ctx, db.ReportProductRankingParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
		CategoryID: categoryID, SortBy: sortBy, RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReportProductRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReportProductRow(r))
	}
	return out, nil
}

func (t tenantTx) ReportStoreComparison(ctx context.Context, f ReportFilter) ([]ReportStoreRow, error) {
	rows, err := t.q.ReportStoreComparison(ctx, db.ReportStoreComparisonParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		RegionID:      f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReportStoreRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReportStoreRow(r))
	}
	return out, nil
}

func (t tenantTx) ReportAlertStores(ctx context.Context, f ReportFilter) ([]ReportAlertStore, error) {
	rows, err := t.q.ReportAlertStores(ctx, db.ReportAlertStoresParams{
		StoreID: f.StoreID, RegionID: f.RegionID,
		OnlyRegionIds: f.Only.RegionIDs, OnlyStoreIds: f.Only.StoreIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReportAlertStore, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReportAlertStore{ID: r.ID, Name: r.Name, RegionID: r.RegionID})
	}
	return out, nil
}

func (t tenantTx) ReportAlertExcludedSKUs(ctx context.Context) ([]int64, error) {
	ids, err := t.q.ReportAlertExcludedSKUs(ctx)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}

func (t tenantTx) ReportAlertSKUInfo(ctx context.Context, skuIDs []int64) (map[int64]ReportAlertSKU, error) {
	out := make(map[int64]ReportAlertSKU, len(skuIDs))
	if len(skuIDs) == 0 {
		return out, nil
	}
	rows, err := t.q.ReportAlertSKUInfo(ctx, skuIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		spec := map[string]string{}
		if len(r.SpecValues) > 0 {
			if err := json.Unmarshal(r.SpecValues, &spec); err != nil {
				return nil, fmt.Errorf("sku %d 的 spec_values 解不开: %w", r.ID, err)
			}
		}
		out[r.ID] = ReportAlertSKU{SKUCode: r.SkuCode, SpecValues: spec,
			ProductID: r.ProductID, ProductTitle: r.ProductTitle}
	}
	return out, nil
}

func (t tenantTx) ReportSearchTotals(ctx context.Context, f ReportFilter) (ReportSearchTotals, error) {
	r, err := t.q.ReportSearchTotals(ctx, db.ReportSearchTotalsParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
	})
	if err != nil {
		return ReportSearchTotals{}, err
	}
	return ReportSearchTotals(r), nil
}

func (t tenantTx) ReportSearchTerms(ctx context.Context, f ReportFilter, onlyZero bool, limit int) ([]ReportSearchTerm, error) {
	if err := checkLimit(limit); err != nil {
		return nil, err
	}
	rows, err := t.q.ReportSearchTerms(ctx, db.ReportSearchTermsParams{
		WindowStart: ts(f.Start), WindowEnd: ts(f.End),
		OnlyZero: onlyZero, RowLimit: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]ReportSearchTerm, 0, len(rows))
	for _, r := range rows {
		out = append(out, ReportSearchTerm(r))
	}
	return out, nil
}
