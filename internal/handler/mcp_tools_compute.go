package handler

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/service"
)

// AI 经营 M10 计算工具（docs/AI经营-M10M11设计.md §2）：slow_movers、promotion_review；
// 渠道分配：channel_allocation_review（docs/superpowers/specs/2026-10-03-ai-channel-allocation-design.md §4.1）。
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

type mcpChannelAllocationIn struct {
	StoreID int64   `json:"store_id" jsonschema:"门店 id（必填）"`
	SKUIDs  []int64 `json:"sku_ids,omitempty" jsonschema:"只看这些 SKU，至多 50 个；不传则自动挑这家门店窗口内在任一渠道卖过或挂零过的"`
	Days    int     `json:"days,omitempty" jsonschema:"窗口天数，7–30，默认 14"`
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
	mcpTool(srv, d, "channel_allocation_review",
		"渠道库存分配复盘（只读）：一家门店每个 SKU 在每个销售渠道（自营 + 接入的外卖 / 电商渠道）上的当前分配规则"+
			"（比例 ratio_bp、安全库存、上限，及来自哪一级）、对外可售数、窗口内卖出件数、有货时的日均销量"+
			"（分母去掉挂零的时间）、挂零小时数（held_zero_hours = keel 有货但规则算成 0；empty_zero_hours = keel 自己没货）、"+
			"缺货拒单件数、单件净收入（扣佣金与成本价；cost_missing 表示没填成本价、净收入没减成本）。"+
			"suggestions 是 keel 按固定规则算的基线建议，可以照用、修改或不用；keel 自己可售为 0 的 SKU 不给建议——缺的是货，不是分配。",
		writeAdminListError, func(ctx context.Context, in mcpChannelAllocationIn) (service.AllocationReview, error) {
			return d.Channels.AllocationReview(ctx, service.AllocationReviewInput{StoreID: in.StoreID, SKUIDs: in.SKUIDs,
				Days: in.Days})
		})
}
