package inventory

// 阶段 1b 的 HTTP 传输层：活动配额、关单释放、退款回补、流水查询，以及两个 SAGA 分支的挂载。
// 形状上的三个决定与 http.go 相同（全部 POST + JSON、业务结论一律 200 + outcome、客户端不重试）。

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/dtm"
)

const (
	pathActivityQuery = "/inventory/activity/query"
	pathActivitySet   = "/inventory/activity/set"
	pathRelease       = "/inventory/orders/release"
	pathRestock       = "/inventory/refunds/restock"
	pathTrail         = "/inventory/trail"
)

const outcomeActivityRule = "activity_rule"

type activityQueryReq struct {
	SKUIDs       []int64 `json:"sku_ids,omitempty"`
	PromotionIDs []int64 `json:"promotion_ids,omitempty"`
}

type activityDTO struct {
	PromotionID int64 `json:"promotion_id"`
	SKUID       int64 `json:"sku_id"`
	Quota       int32 `json:"quota"`
	Sold        int32 `json:"sold"`
}

type activityQueryResp struct {
	Items []activityDTO `json:"items"`
}

type activitySetReq struct {
	PromotionID int64              `json:"promotion_id"`
	Items       []activityQuotaDTO `json:"items"`
}

type activityQuotaDTO struct {
	SKUID int64 `json:"sku_id"`
	Quota int32 `json:"quota"`
}

type activityRuleDTO struct {
	SKUID   int64 `json:"sku_id"`
	Sold    int32 `json:"sold"`
	Quota   int32 `json:"quota"`
	Removed bool  `json:"removed"`
}

type activitySetResp struct {
	Outcome string           `json:"outcome"`
	Rule    *activityRuleDTO `json:"rule,omitempty"`
}

type releaseReq struct {
	OrderNo string      `json:"order_no"`
	StoreID int64       `json:"store_id"`
	BizType int16       `json:"biz_type"`
	Lines   []OrderLine `json:"lines"`
}

type restockReq struct {
	RefundNo string      `json:"refund_no"`
	StoreID  int64       `json:"store_id"`
	Lines    []OrderLine `json:"lines"`
}

type releaseResp struct {
	Outcome  string `json:"outcome"`
	Qty      int32  `json:"qty"`
	Replayed bool   `json:"replayed,omitempty"`
}

type trailReq struct {
	BizID string `json:"biz_id"`
}

type trailDTO struct {
	SKUID   int64  `json:"sku_id"`
	StoreID int64  `json:"store_id"`
	BizType int16  `json:"biz_type"`
	Change  int32  `json:"change"`
	Before  int32  `json:"before"`
	After   int32  `json:"after"`
	Warning int32  `json:"warning"`
	Reason  string `json:"reason,omitempty"`
}

type trailResp struct {
	Items []trailDTO `json:"items"`
}

// mountOrders 挂阶段 1b 的接口（Mount 调它）。
func mountOrders(g *gin.RouterGroup, h handler) {
	g.POST(pathActivityQuery, h.activityQuery)
	g.POST(pathActivitySet, h.activitySet)
	g.POST(pathRelease, h.release)
	g.POST(pathRestock, h.restock)
	g.POST(pathTrail, h.trail)
}

// MountSaga 把库存服务的 SAGA 分支挂到 g 上。g 必须是 rpc.Routes.Saga（分支令牌准入）。
// 分支体是 Local 上的同一个函数 —— 单体里它注册在进程内协调器上（local://）。
func MountSaga(g *gin.RouterGroup, l *Local) {
	dtm.MountBranches(g, l.SagaBranches())
}

func (h handler) activityQuery(c *gin.Context) {
	var in activityQueryReq
	if !bind(c, &in) {
		return
	}
	m, err := h.svc.ActivityStock(c.Request.Context(), ActivityQuery{SKUIDs: in.SKUIDs, PromotionIDs: in.PromotionIDs})
	if err != nil {
		fail(c, err)
		return
	}
	out := activityQueryResp{Items: make([]activityDTO, 0, len(m))}
	for k, a := range m {
		out.Items = append(out.Items, activityDTO{PromotionID: k.PromotionID, SKUID: k.SKUID, Quota: a.Quota, Sold: a.Sold})
	}
	c.JSON(http.StatusOK, out)
}

func (h handler) activitySet(c *gin.Context) {
	var in activitySetReq
	if !bind(c, &in) {
		return
	}
	items := make([]ActivityQuota, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, ActivityQuota{SKUID: it.SKUID, Quota: it.Quota})
	}
	err := h.svc.SetActivityQuotas(c.Request.Context(), in.PromotionID, items)
	var rule *ActivityRuleError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, activitySetResp{Outcome: outcomeOK})
	case errors.As(err, &rule):
		c.JSON(http.StatusOK, activitySetResp{Outcome: outcomeActivityRule, Rule: &activityRuleDTO{
			SKUID: rule.SKUID, Sold: rule.Sold, Quota: rule.Quota, Removed: rule.Removed}})
	default:
		fail(c, err)
	}
}

func (h handler) release(c *gin.Context) {
	var in releaseReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.ReleaseForOrder(c.Request.Context(), ReleaseRequest{
		OrderNo: in.OrderNo, StoreID: in.StoreID, BizType: in.BizType, Lines: in.Lines,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, releaseResp{Outcome: outcomeOK, Qty: res.Qty, Replayed: res.Replayed})
}

func (h handler) restock(c *gin.Context) {
	var in restockReq
	if !bind(c, &in) {
		return
	}
	res, err := h.svc.RestockForRefund(c.Request.Context(), RestockRequest{
		RefundNo: in.RefundNo, StoreID: in.StoreID, Lines: in.Lines,
	})
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, releaseResp{Outcome: outcomeOK, Qty: res.Qty, Replayed: res.Replayed})
}

func (h handler) trail(c *gin.Context) {
	var in trailReq
	if !bind(c, &in) {
		return
	}
	rows, err := h.svc.OrderTrail(c.Request.Context(), in.BizID)
	if err != nil {
		fail(c, err)
		return
	}
	out := trailResp{Items: make([]trailDTO, 0, len(rows))}
	for _, e := range rows {
		out.Items = append(out.Items, trailDTO{SKUID: e.SKUID, StoreID: e.StoreID, BizType: e.BizType,
			Change: e.Change, Before: e.Before, After: e.After, Warning: e.Warning, Reason: e.Reason})
	}
	c.JSON(http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 客户端
// ---------------------------------------------------------------------------

func (r *Remote) ActivityStock(ctx context.Context, q ActivityQuery) (map[ActivityKey]Activity, error) {
	out := map[ActivityKey]Activity{}
	if len(q.SKUIDs) == 0 && len(q.PromotionIDs) == 0 {
		// 与 Local 同一个短路：空批不发请求（没有任何报价的列表页不为活动多跨一次网络）。
		return out, nil
	}
	var resp activityQueryResp
	if err := r.read(ctx, pathActivityQuery, activityQueryReq{
		SKUIDs: dedup(q.SKUIDs), PromotionIDs: dedup(q.PromotionIDs),
	}, &resp); err != nil {
		return nil, err
	}
	for _, it := range resp.Items {
		out[ActivityKey{PromotionID: it.PromotionID, SKUID: it.SKUID}] = Activity{Quota: it.Quota, Sold: it.Sold}
	}
	return out, nil
}

func (r *Remote) SetActivityQuotas(ctx context.Context, promotionID int64, items []ActivityQuota) error {
	in := activitySetReq{PromotionID: promotionID, Items: make([]activityQuotaDTO, 0, len(items))}
	for _, it := range items {
		in.Items = append(in.Items, activityQuotaDTO{SKUID: it.SKUID, Quota: it.Quota})
	}
	var resp activitySetResp
	if err := r.write(ctx, pathActivitySet, in, &resp); err != nil {
		return err
	}
	switch resp.Outcome {
	case outcomeOK:
		return nil
	case outcomeActivityRule:
		if resp.Rule == nil {
			return unknownOutcome(pathActivitySet, resp.Outcome+"（缺 rule）")
		}
		return &ActivityRuleError{SKUID: resp.Rule.SKUID, Sold: resp.Rule.Sold, Quota: resp.Rule.Quota,
			Removed: resp.Rule.Removed}
	default:
		return unknownOutcome(pathActivitySet, resp.Outcome)
	}
}

func (r *Remote) ReleaseForOrder(ctx context.Context, req ReleaseRequest) (ReleaseResult, error) {
	var resp releaseResp
	if err := r.write(ctx, pathRelease, releaseReq{
		OrderNo: req.OrderNo, StoreID: req.StoreID, BizType: req.BizType, Lines: req.Lines,
	}, &resp); err != nil {
		return ReleaseResult{}, err
	}
	if resp.Outcome != outcomeOK {
		return ReleaseResult{}, unknownOutcome(pathRelease, resp.Outcome)
	}
	return ReleaseResult{Qty: resp.Qty, Replayed: resp.Replayed}, nil
}

func (r *Remote) RestockForRefund(ctx context.Context, req RestockRequest) (ReleaseResult, error) {
	var resp releaseResp
	if err := r.write(ctx, pathRestock, restockReq{
		RefundNo: req.RefundNo, StoreID: req.StoreID, Lines: req.Lines,
	}, &resp); err != nil {
		return ReleaseResult{}, err
	}
	if resp.Outcome != outcomeOK {
		return ReleaseResult{}, unknownOutcome(pathRestock, resp.Outcome)
	}
	return ReleaseResult{Qty: resp.Qty, Replayed: resp.Replayed}, nil
}

func (r *Remote) OrderTrail(ctx context.Context, bizID string) ([]TrailEntry, error) {
	var resp trailResp
	if err := r.read(ctx, pathTrail, trailReq{BizID: bizID}, &resp); err != nil {
		return nil, err
	}
	out := make([]TrailEntry, 0, len(resp.Items))
	for _, it := range resp.Items {
		out = append(out, TrailEntry{SKUID: it.SKUID, StoreID: it.StoreID, BizType: it.BizType,
			Change: it.Change, Before: it.Before, After: it.After, Warning: it.Warning, Reason: it.Reason})
	}
	return out, nil
}
