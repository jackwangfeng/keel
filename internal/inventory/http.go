package inventory

// 库存服务的 HTTP 传输层：服务端 handler（挂到 rpc.Routes.Tenant）与客户端（Remote）。
//
// ===========================================================================
// 形状上的三个决定
// ===========================================================================
//
// 一、**全部是 POST + JSON**，读也是。批量读的入参是 id 列表（一页商品的 SKU、一辆车），
//     塞进查询串会碰到长度上限，而签名覆盖的正文没有这个问题（rpc.maxBody = 1 MiB）。
//     这是内网接口，不对外承诺 REST 语义。
//
// 二、**业务结果一律 200，差别写在正文的 outcome 里**；非 2xx 只留给传输与调用方 bug。
//     「前提不成立」「扣完会变负」「缺行」是库存服务**给出了答案**，core 要拿着答案里的
//     当前值回给后台（契约的 InventoryConflict.current）；而 rpc.Client 对非 2xx 只保留
//     Problem 的 type / title，拿不到正文。更要紧的是分类：rpc 把 5xx 与传输失败归为
//     「结果未知」，4xx 归为「确定失败」—— 业务结论走 200 之后，core 只需要看一眼 rpc 的
//     分类就知道该回 503 还是 500，不必在两套错误语义之间来回翻译。
//
// 三、**客户端不重试**（与 rpc.Client 同一条规矩）。写的重试由 core 的调用方决定：
//     公网请求回 503 让客户端带同一把幂等键重试；相对调整的幂等在库存服务这一侧（biz_id）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/rpc"
)

// 路径（接在 rpc.Prefix = /internal/v1 之后）。客户端与服务端共用这几个常量。
const (
	pathStoreStock = "/inventory/stock/by-store"
	pathSKUTotals  = "/inventory/stock/totals"
	pathHealthy    = "/inventory/stock/healthy"
	pathLowStock   = "/inventory/alerts"
	pathSet        = "/inventory/stock/set"
	pathAdjust     = "/inventory/stock/adjust"
	pathInit       = "/inventory/stock/init"
)

// 写操作的 outcome 取值。
const (
	outcomeOK           = "ok"
	outcomeNotFound     = "not_found"
	outcomeConflict     = "conflict"
	outcomeInsufficient = "insufficient"
	outcomeBizIDReused  = "biz_id_reused"
)

// ---------------------------------------------------------------------------
// 线上的形状
// ---------------------------------------------------------------------------

type storeStockReq struct {
	StoreID int64   `json:"store_id"`
	SKUIDs  []int64 `json:"sku_ids"`
}

type levelDTO struct {
	SKUID     int64     `json:"sku_id"`
	Available int32     `json:"available"`
	Warning   int32     `json:"warning"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storeStockResp struct {
	// Levels 只含**有行**的 SKU；没回来的即缺行（可售 0）。
	Levels []levelDTO `json:"levels"`
}

type skuTotalsReq struct {
	SKUIDs []int64 `json:"sku_ids"`
}

type totalDTO struct {
	SKUID     int64 `json:"sku_id"`
	Available int32 `json:"available"`
	Warning   int32 `json:"warning"`
}

type skuTotalsResp struct {
	Totals []totalDTO `json:"totals"`
}

type healthyReq struct {
	StoreID int64 `json:"store_id"`
}

type healthyResp struct {
	SKUIDs []int64 `json:"sku_ids"`
}

type lowStockReq struct {
	StoreIDs      []int64 `json:"store_ids"`
	ExcludeSKUIDs []int64 `json:"exclude_sku_ids"`
	Limit         int     `json:"limit"`
}

type lowStockRowDTO struct {
	StoreID   int64 `json:"store_id"`
	SKUID     int64 `json:"sku_id"`
	Available int32 `json:"available"`
	Warning   int32 `json:"warning"`
}

type lowStockResp struct {
	Items []lowStockRowDTO `json:"items"`
	Total int64            `json:"total"`
}

type stockDTO struct {
	SKUID     int64     `json:"sku_id"`
	StoreID   int64     `json:"store_id"`
	Available int32     `json:"available"`
	Warning   int32     `json:"warning"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toStockDTO(s Stock) stockDTO {
	return stockDTO{SKUID: s.SKUID, StoreID: s.StoreID, Available: s.Available, Warning: s.Warning, UpdatedAt: s.UpdatedAt}
}

func (d stockDTO) stock() Stock {
	return Stock{SKUID: d.SKUID, StoreID: d.StoreID, Available: d.Available, Warning: d.Warning, UpdatedAt: d.UpdatedAt}
}

type setReq struct {
	SKUID       int64  `json:"sku_id"`
	StoreID     int64  `json:"store_id"`
	Available   int32  `json:"available"`
	Expected    int32  `json:"expected"`
	Warning     *int32 `json:"warning,omitempty"`
	AllowInsert bool   `json:"allow_insert"`
	BizID       string `json:"biz_id"`
}

type adjustReq struct {
	SKUID   int64   `json:"sku_id"`
	StoreID int64   `json:"store_id"`
	Delta   int32   `json:"delta"`
	Reason  *string `json:"reason,omitempty"`
	BizID   string  `json:"biz_id"`
}

// writeResp 是两个写操作共用的响应。Stock 在 ok 时是写后的值，在 conflict / insufficient
// 时是当前值；not_found / biz_id_reused 时是零值。
type writeResp struct {
	Outcome  string   `json:"outcome"`
	Stock    stockDTO `json:"stock"`
	Replayed bool     `json:"replayed,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

type initReq struct {
	Rows []initRowDTO `json:"rows"`
}

type initRowDTO struct {
	SKUID     int64 `json:"sku_id"`
	StoreID   int64 `json:"store_id"`
	Available int32 `json:"available"`
	Warning   int32 `json:"warning"`
}

// ---------------------------------------------------------------------------
// 服务端
// ---------------------------------------------------------------------------

// Mount 把库存接口挂到 g 上。g 必须是 rpc.Routes.Tenant（验签 + 租户头）：
// handler 直接用请求 ctx 调 svc，租户已经在 ctx 里，RLS 照旧工作。
func Mount(g *gin.RouterGroup, svc Service) {
	h := handler{svc: svc}
	g.POST(pathStoreStock, h.storeStock)
	g.POST(pathSKUTotals, h.skuTotals)
	g.POST(pathHealthy, h.healthy)
	g.POST(pathLowStock, h.lowStock)
	g.POST(pathSet, h.set)
	g.POST(pathAdjust, h.adjust)
	g.POST(pathInit, h.init)
	mountOrders(g, h)
	g.POST(pathStockKeys, h.stockKeys)
}

type handler struct{ svc Service }

// fail 把 svc 的非业务错误写成 Problem：ErrInvalid 是调用方的 bug（400，rpc 归为确定失败），
// 其余（数据库错误）是 500 —— rpc 归为「结果未知」，写操作据此不会被 core 当成失败。
func fail(c *gin.Context, err error) {
	if errors.Is(err, ErrInvalid) {
		problem.Write(c, http.StatusBadRequest, problem.TypeInvalidRequest, err.Error())
		return
	}
	slog.ErrorContext(c.Request.Context(), "库存服务内部错误", "path", c.Request.URL.Path, "err", err)
	problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "库存服务内部错误")
}

func bind(c *gin.Context, v any) bool {
	if err := c.ShouldBindJSON(v); err != nil {
		problem.Write(c, http.StatusBadRequest, problem.TypeInvalidRequest, "请求体不合法: "+err.Error())
		return false
	}
	return true
}

func (h handler) storeStock(c *gin.Context) {
	var in storeStockReq
	if !bind(c, &in) {
		return
	}
	m, err := h.svc.StoreStock(c.Request.Context(), in.StoreID, in.SKUIDs)
	if err != nil {
		fail(c, err)
		return
	}
	out := storeStockResp{Levels: make([]levelDTO, 0, len(m))}
	for id, l := range m {
		if l.Exists {
			out.Levels = append(out.Levels, levelDTO{SKUID: id, Available: l.Available, Warning: l.Warning, UpdatedAt: l.UpdatedAt})
		}
	}
	c.JSON(http.StatusOK, out)
}

func (h handler) skuTotals(c *gin.Context) {
	var in skuTotalsReq
	if !bind(c, &in) {
		return
	}
	m, err := h.svc.SKUTotals(c.Request.Context(), in.SKUIDs)
	if err != nil {
		fail(c, err)
		return
	}
	out := skuTotalsResp{Totals: make([]totalDTO, 0, len(m))}
	for id, t := range m {
		out.Totals = append(out.Totals, totalDTO{SKUID: id, Available: t.Available, Warning: t.Warning})
	}
	c.JSON(http.StatusOK, out)
}

func (h handler) healthy(c *gin.Context) {
	var in healthyReq
	if !bind(c, &in) {
		return
	}
	ids, err := h.svc.HealthySKUs(c.Request.Context(), in.StoreID)
	if err != nil {
		fail(c, err)
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	c.JSON(http.StatusOK, healthyResp{SKUIDs: ids})
}

func (h handler) lowStock(c *gin.Context) {
	var in lowStockReq
	if !bind(c, &in) {
		return
	}
	p, err := h.svc.LowStock(c.Request.Context(), LowStockQuery{
		StoreIDs: in.StoreIDs, ExcludeSKUIDs: in.ExcludeSKUIDs, Limit: in.Limit,
	})
	if err != nil {
		fail(c, err)
		return
	}
	out := lowStockResp{Total: p.Total, Items: make([]lowStockRowDTO, 0, len(p.Items))}
	for _, r := range p.Items {
		out.Items = append(out.Items, lowStockRowDTO{StoreID: r.StoreID, SKUID: r.SKUID, Available: r.Available, Warning: r.Warning})
	}
	c.JSON(http.StatusOK, out)
}

func (h handler) set(c *gin.Context) {
	var in setReq
	if !bind(c, &in) {
		return
	}
	s, err := h.svc.Set(c.Request.Context(), SetRequest{
		SKUID: in.SKUID, StoreID: in.StoreID, Available: in.Available, Expected: in.Expected,
		Warning: in.Warning, AllowInsert: in.AllowInsert, BizID: in.BizID,
	})
	var conflict *ConflictError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeOK, Stock: toStockDTO(s)})
	case errors.As(err, &conflict):
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeConflict, Stock: toStockDTO(conflict.Current)})
	case errors.Is(err, ErrNotFound):
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeNotFound, Detail: err.Error()})
	default:
		fail(c, err)
	}
}

func (h handler) adjust(c *gin.Context) {
	var in adjustReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.Adjust(c.Request.Context(), AdjustRequest{
		SKUID: in.SKUID, StoreID: in.StoreID, Delta: in.Delta, Reason: in.Reason, BizID: in.BizID,
	})
	var short *InsufficientError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeOK, Stock: toStockDTO(res.Stock), Replayed: res.Replayed})
	case errors.As(err, &short):
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeInsufficient, Stock: toStockDTO(short.Current)})
	case errors.Is(err, ErrBizIDReused):
		c.JSON(http.StatusOK, writeResp{Outcome: outcomeBizIDReused, Detail: err.Error()})
	default:
		fail(c, err)
	}
}

func (h handler) init(c *gin.Context) {
	var in initReq
	if !bind(c, &in) {
		return
	}
	rows := make([]InitRow, 0, len(in.Rows))
	for _, r := range in.Rows {
		rows = append(rows, InitRow{SKUID: r.SKUID, StoreID: r.StoreID, Available: r.Available, Warning: r.Warning})
	}
	if err := h.svc.InitSKUs(c.Request.Context(), rows); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, writeResp{Outcome: outcomeOK})
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

// Remote 是 HTTP 实现。KEEL_ROLE=core 用它。
type Remote struct{ c *rpc.Client }

// NewRemote 建 HTTP 实现。c 由 rpc.NewClient(KEEL_INVENTORY_URL, KEEL_INTERNAL_SECRET, 0) 建。
func NewRemote(c *rpc.Client) *Remote { return &Remote{c: c} }

var _ Service = (*Remote)(nil)

// read 发一个读请求。结果未知（连不上、超时、5xx）→ ErrUnavailable，读页面据此降级；
// 确定失败（4xx：签名不对、入参不合法）不是「对面暂时不在」，是配置或代码的错，原样上浮成 500。
func (r *Remote) read(ctx context.Context, path string, in, out any) error {
	err := r.c.PostJSON(ctx, rpc.Prefix+path, in, out)
	if err == nil {
		return nil
	}
	if rpc.IsUnknown(err) {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return fmt.Errorf("库存服务拒绝了读请求: %w", err)
}

// write 发一个写请求。结果未知 → ErrOutcomeUnknown：**不能当成失败**，
// 见 inventory.go 文件头「两种没拿到答案」。
func (r *Remote) write(ctx context.Context, path string, in, out any) error {
	err := r.c.PostJSON(ctx, rpc.Prefix+path, in, out)
	if err == nil {
		return nil
	}
	if rpc.IsUnknown(err) {
		return fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
	}
	if errors.Is(err, rpc.ErrBadRequest) {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return fmt.Errorf("库存服务拒绝了写请求: %w", err)
}

func (r *Remote) StoreStock(ctx context.Context, storeID int64, skuIDs []int64) (map[int64]Level, error) {
	ids := dedup(skuIDs)
	out := make(map[int64]Level, len(ids))
	if len(ids) == 0 {
		// 与 Local 同一个短路：空批不发请求。否则「一辆空车」也要跨一次网络，
		// 而且库存服务不在时空车也会被降级 —— 那是一个没有任何理由的 503。
		return out, nil
	}
	var resp storeStockResp
	if err := r.read(ctx, pathStoreStock, storeStockReq{StoreID: storeID, SKUIDs: ids}, &resp); err != nil {
		return nil, err
	}
	for _, l := range resp.Levels {
		out[l.SKUID] = Level{Available: l.Available, Warning: l.Warning, Exists: true, UpdatedAt: l.UpdatedAt}
	}
	return out, nil
}

func (r *Remote) SKUTotals(ctx context.Context, skuIDs []int64) (map[int64]Total, error) {
	ids := dedup(skuIDs)
	out := make(map[int64]Total, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var resp skuTotalsResp
	if err := r.read(ctx, pathSKUTotals, skuTotalsReq{SKUIDs: ids}, &resp); err != nil {
		return nil, err
	}
	for _, t := range resp.Totals {
		out[t.SKUID] = Total{Available: t.Available, Warning: t.Warning}
	}
	return out, nil
}

func (r *Remote) HealthySKUs(ctx context.Context, storeID int64) ([]int64, error) {
	var resp healthyResp
	if err := r.read(ctx, pathHealthy, healthyReq{StoreID: storeID}, &resp); err != nil {
		return nil, err
	}
	return resp.SKUIDs, nil
}

func (r *Remote) LowStock(ctx context.Context, q LowStockQuery) (LowStockPage, error) {
	if len(q.StoreIDs) == 0 && q.Limit > 0 {
		return LowStockPage{Items: []LowStockRow{}}, nil
	}
	var resp lowStockResp
	if err := r.read(ctx, pathLowStock, lowStockReq{
		StoreIDs: q.StoreIDs, ExcludeSKUIDs: q.ExcludeSKUIDs, Limit: q.Limit,
	}, &resp); err != nil {
		return LowStockPage{}, err
	}
	out := LowStockPage{Total: resp.Total, Items: make([]LowStockRow, 0, len(resp.Items))}
	for _, it := range resp.Items {
		out.Items = append(out.Items, LowStockRow{StoreID: it.StoreID, SKUID: it.SKUID, Available: it.Available, Warning: it.Warning})
	}
	return out, nil
}

func (r *Remote) Set(ctx context.Context, req SetRequest) (Stock, error) {
	var resp writeResp
	if err := r.write(ctx, pathSet, setReq{
		SKUID: req.SKUID, StoreID: req.StoreID, Available: req.Available, Expected: req.Expected,
		Warning: req.Warning, AllowInsert: req.AllowInsert, BizID: req.BizID,
	}, &resp); err != nil {
		return Stock{}, err
	}
	switch resp.Outcome {
	case outcomeOK:
		return resp.Stock.stock(), nil
	case outcomeConflict:
		return Stock{}, &ConflictError{Current: resp.Stock.stock()}
	case outcomeNotFound:
		return Stock{}, fmt.Errorf("%w: %s", ErrNotFound, resp.Detail)
	default:
		return Stock{}, unknownOutcome(pathSet, resp.Outcome)
	}
}

func (r *Remote) Adjust(ctx context.Context, req AdjustRequest) (AdjustResult, error) {
	var resp writeResp
	if err := r.write(ctx, pathAdjust, adjustReq{
		SKUID: req.SKUID, StoreID: req.StoreID, Delta: req.Delta, Reason: req.Reason, BizID: req.BizID,
	}, &resp); err != nil {
		return AdjustResult{}, err
	}
	switch resp.Outcome {
	case outcomeOK:
		return AdjustResult{Stock: resp.Stock.stock(), Replayed: resp.Replayed}, nil
	case outcomeInsufficient:
		return AdjustResult{}, &InsufficientError{Delta: req.Delta, Current: resp.Stock.stock()}
	case outcomeBizIDReused:
		return AdjustResult{}, fmt.Errorf("%w: %s", ErrBizIDReused, resp.Detail)
	default:
		return AdjustResult{}, unknownOutcome(pathAdjust, resp.Outcome)
	}
}

func (r *Remote) InitSKUs(ctx context.Context, rows []InitRow) error {
	if len(rows) == 0 {
		return nil
	}
	in := initReq{Rows: make([]initRowDTO, 0, len(rows))}
	for _, x := range rows {
		in.Rows = append(in.Rows, initRowDTO{SKUID: x.SKUID, StoreID: x.StoreID, Available: x.Available, Warning: x.Warning})
	}
	var resp writeResp
	return r.write(ctx, pathInit, in, &resp)
}

// unknownOutcome：对面说 200，outcome 却不认识 —— 多半是两边版本不一致。
// 写可能已经发生了，所以按「结果未知」处置，而不是当成失败。
func unknownOutcome(path, outcome string) error {
	return fmt.Errorf("%w: %s 回了不认识的 outcome %q（core 与库存服务版本不一致？）",
		ErrOutcomeUnknown, path, outcome)
}
