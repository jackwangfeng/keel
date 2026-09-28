package handler

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/service"
)

// AI 经营 M10 计算工具（docs/AI经营-M10M11设计.md §2）：slow_movers、promotion_review。
//
// 与 mcp_tools.go 里那批工具同一套规矩（同一个 service 函数、同一个判权、同一个错误出口），
// 只是单独开一个文件登记——mcp_tools.go 那时候有另一路在并发改，这里只从 registerMCPTools
// 借一行调用（见那个文件末尾）。

type mcpSlowMoversIn struct {
	StoreID      *int64 `json:"store_id,omitempty" jsonschema:"只算这家门店；不传则是你管辖范围内全部营业中的门店"`
	LookbackDays int    `json:"lookback_days,omitempty" jsonschema:"按最近多少天的销量算周转，1–90，默认 30"`
	MinAvailable int32  `json:"min_available,omitempty" jsonschema:"只看可售数不低于它的（门店，SKU），默认 10"`
	Limit        int    `json:"limit,omitempty" jsonschema:"最多返回几条，默认 50，最多 200"`
}

type mcpPromotionReviewIn struct {
	PromotionID      *int64 `json:"promotion_id,omitempty" jsonschema:"活动 id；与 coupon_template_id 二选一"`
	CouponTemplateID *int64 `json:"coupon_template_id,omitempty" jsonschema:"券模板 id；与 promotion_id 二选一"`
}

func registerMCPComputeTools(srv *mcp.Server, d *MCPDeps) {
	mcpTool(srv, d, "slow_movers",
		"滞销清仓：每个（门店，SKU）的库存周转天数（可售 ÷ 日均，日均口径与 restock_plan 相同——"+
			"分母去掉断货天）。周转天数为 null 的是回看期内一件没卖出去的，排最前，比任何数字都更该清。"+
			"只给「哪些该清」的名单，不算折扣或清货量。",
		writeAdminListError, func(ctx context.Context, in mcpSlowMoversIn) (service.SlowMoversResult, error) {
			return d.SlowMovers.Plan(ctx, service.SlowMoversQuery{StoreID: in.StoreID, LookbackDays: in.LookbackDays,
				MinAvailable: in.MinAvailable, Limit: in.Limit})
		})
	mcpTool(srv, d, "promotion_review",
		"活动 / 券复盘：promotion_id 或 coupon_template_id 二选一。活动给窗口内与紧邻的前一个"+
			"等长窗口的销售额、单量、客单价对比，以及参与 SKU 的销量（限时折扣 / 秒杀才有；满减满折按"+
			"全店口径算）；券给发出数、核销数、核销率，以及这张券带来的已支付订单与优惠合计。",
		writePromotionError, func(ctx context.Context, in mcpPromotionReviewIn) (service.PromotionReviewOut, error) {
			return d.PromotionReview.Review(ctx, service.PromotionReviewQuery{PromotionID: in.PromotionID,
				CouponTemplateID: in.CouponTemplateID})
		})
}
