package handler

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/service"
)

// M10 的四种提案工具（docs/AI经营-M10M11设计.md §1）。都只是「提」：不执行，人在后台批准后 Keel 以 AI 员工身份执行。

type mcpProposalMetaIn struct {
	Evidence       string `json:"evidence" jsonschema:"证据（markdown）：引用工具返回的数字，能被人复现；10–8000 字"`
	ExpectedImpact string `json:"expected_impact,omitempty" jsonschema:"预计影响，≤2000 字"`
}

func (m mcpProposalMetaIn) meta() service.ProposalMeta {
	return service.ProposalMeta{Evidence: m.Evidence, ExpectedImpact: m.ExpectedImpact}
}

type mcpFlashItemIn struct {
	SKUID        int64 `json:"sku_id"`
	DiscountRate int16 `json:"discount_rate" jsonschema:"千分比折扣，850 即 85 折；500–999（最多打五折）"`
}

type mcpFlashPriceIn struct {
	Name     string           `json:"name" jsonschema:"活动名，1–30 字，买家看得到"`
	StoreID  *int64           `json:"store_id,omitempty" jsonschema:"只在这家门店生效；不给即全店"`
	Items    []mcpFlashItemIn `json:"items" jsonschema:"参加的 SKU，1–20 个"`
	StartsAt time.Time        `json:"starts_at" jsonschema:"开始时间（RFC 3339），现在到 7 天内"`
	EndsAt   time.Time        `json:"ends_at" jsonschema:"结束时间，活动至多 14 天"`
	mcpProposalMetaIn
}

type mcpCouponIn struct {
	Name             string `json:"name" jsonschema:"券名，1–30 字，买家看得到"`
	CouponType       int16  `json:"coupon_type" jsonschema:"1 满减 / 2 折扣 / 3 立减"`
	ThresholdCents   int64  `json:"threshold_cents,omitempty" jsonschema:"满多少可用（分）；立减券为 0"`
	DiscountCents    int64  `json:"discount_cents,omitempty" jsonschema:"满减 / 立减的面额（分），≤10000"`
	DiscountRate     int16  `json:"discount_rate,omitempty" jsonschema:"折扣券的千分比，500–999"`
	MaxDiscountCents int64  `json:"max_discount_cents,omitempty" jsonschema:"折扣券的封顶（分），≤10000"`
	ValidDays        int32  `json:"valid_days" jsonschema:"领取后几天内有效，1–90"`
	TotalCount       int32  `json:"total_count" jsonschema:"发行量，1–10000"`
	PerUserLimit     int32  `json:"per_user_limit" jsonschema:"每人限领，1–5"`
	Claimable        bool   `json:"claimable,omitempty" jsonschema:"是否放进领券中心让买家自己领"`
	mcpProposalMetaIn
}

type mcpProductCopyIn struct {
	ProductID int64   `json:"product_id"`
	Title     *string `json:"title,omitempty" jsonschema:"新标题，1–60 字；不改就不给"`
	Subtitle  *string `json:"subtitle,omitempty" jsonschema:"新副标题，≤120 字；不改就不给"`
	mcpProposalMetaIn
}

type mcpRefundDecisionIn struct {
	RefundNo     string  `json:"refund_no"`
	Action       string  `json:"action" jsonschema:"approve 同意 / reject 驳回"`
	RejectReason *string `json:"reject_reason,omitempty" jsonschema:"驳回理由，买家看得到；驳回时必填，≤200 字"`
	mcpProposalMetaIn
}

func registerMCPProposalTools(srv *mcp.Server, d *MCPDeps) {
	mcpTool(srv, d, "my_scorecard",
		"你自己的成绩单（近 30 天）：按提案种类的提 / 批 / 驳回 / 过期数，以及执行后复盘的 positive / neutral / negative "+
			"分布与最近的明细。驳回多、negative 多的种类要收着提。",
		writeProposalError, func(ctx context.Context, _ struct{}) (api.AgentScorecard, error) {
			sc, err := d.Proposals.MyScorecard(ctx)
			if err != nil {
				return api.AgentScorecard{}, err
			}
			return apiScorecard(sc), nil
		})
	mcpTool(srv, d, "propose_flash_price",
		"提一条限时折扣提案（清仓 / 促销）：若干 SKU 在一段时间内打折。不会立即执行：人批准后 Keel 建活动并上线。"+
			"需要全店范围的 AI 员工。同一组 SKU 已有待处理提案时会被拒。",
		writeProposalError, func(ctx context.Context, in mcpFlashPriceIn) (api.AgentProposal, error) {
			items := make([]service.FlashPriceItem, 0, len(in.Items))
			for _, it := range in.Items {
				items = append(items, service.FlashPriceItem{SKUID: it.SKUID, DiscountRate: it.DiscountRate})
			}
			p, err := d.Proposals.ProposeFlashPrice(ctx, service.FlashPricePayload{Name: in.Name, StoreID: in.StoreID,
				Items: items, StartsAt: in.StartsAt, EndsAt: in.EndsAt}, in.meta())
			if err != nil {
				return api.AgentProposal{}, err
			}
			return apiAgentProposal(p), nil
		})
	mcpTool(srv, d, "propose_coupon",
		"提一条发券提案：建一张券（满减 / 折扣 / 立减），可放进领券中心。不会立即执行：人批准后 Keel 建券。"+
			"需要全店范围的 AI 员工。面额至多 100 元、发行量至多 10000。",
		writeProposalError, func(ctx context.Context, in mcpCouponIn) (api.AgentProposal, error) {
			p, err := d.Proposals.ProposeCoupon(ctx, service.CouponPayload{Name: in.Name, CouponType: in.CouponType,
				ThresholdCents: in.ThresholdCents, DiscountCents: in.DiscountCents, DiscountRate: in.DiscountRate,
				MaxDiscountCents: in.MaxDiscountCents, ValidDays: in.ValidDays, TotalCount: in.TotalCount,
				PerUserLimit: in.PerUserLimit, Claimable: in.Claimable}, in.meta())
			if err != nil {
				return api.AgentProposal{}, err
			}
			return apiAgentProposal(p), nil
		})
	mcpTool(srv, d, "propose_product_copy",
		"提一条改商品标题 / 副标题的提案（搜索缺口：让搜得到、点得进）。不会立即执行：人批准后 Keel 改，并过广告法违禁词检查。"+
			"需要全店范围的 AI 员工。",
		writeProposalError, func(ctx context.Context, in mcpProductCopyIn) (api.AgentProposal, error) {
			p, err := d.Proposals.ProposeProductCopy(ctx, in.ProductID, in.Title, in.Subtitle, in.meta())
			if err != nil {
				return api.AgentProposal{}, err
			}
			return apiAgentProposal(p), nil
		})
	mcpTool(srv, d, "propose_refund_decision",
		"对一张待审核的售后单提审核意见（同意 / 驳回，驳回要写给买家看的理由）。不会立即执行：人批准后 Keel 审核。"+
			"按订单的履约门店判权。",
		writeProposalError, func(ctx context.Context, in mcpRefundDecisionIn) (api.AgentProposal, error) {
			p, err := d.Proposals.ProposeRefundDecision(ctx, in.RefundNo, in.Action, in.RejectReason, in.meta())
			if err != nil {
				return api.AgentProposal{}, err
			}
			return apiAgentProposal(p), nil
		})
}

type mcpQuerySQLIn struct {
	SQL string `json:"sql" jsonschema:"一条 SELECT / WITH 查询，只能读 agent_ro 里的视图（不写 schema 前缀即可）：orders order_items products skus categories stores regions refunds search_logs coupon_templates user_coupons promotions promotion_skus。至多 500 行、3 秒"`
}

func registerMCPSQLTool(srv *mcp.Server, d *MCPDeps) {
	mcpTool(srv, d, "query_sql",
		"只读 SQL：现有工具答不了的经营问题（「上周复购的买家占比」「某类目的客单分布」）用它自己查。只能读一组脱敏视图"+
			"（没有手机号、地址、买家原话），只接受一条 SELECT / WITH，至多 500 行、3 秒超时。金额是分。"+
			"需要全店范围的 AI 员工。能用专门工具的（报表、补货、滞销）优先用专门工具。",
		writeProposalError, func(ctx context.Context, in mcpQuerySQLIn) (service.AgentSQLResult, error) {
			return d.Proposals.QuerySQL(ctx, in.SQL)
		})
}
