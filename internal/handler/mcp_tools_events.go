package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 事件的拉取工具（AI 经营 M10 §3）：list_events / ack_events。
//
// 游标由 Keel 记（每名 AI 员工一个）：agent 每次醒来 list_events()（不带 after_id 就从上次 ack 的位置接着读），
// 处理完这一页 ack_events(next_after_id)。没 ack 的下次还会读到 —— 至少一次，agent 按事件 id 去重。
// 事件按 AI 员工的管辖范围过滤：门店级事件只给能操作那家店的；全店事件（store_id 为空）只给全店范围的。

type mcpListEventsIn struct {
	AfterID *int64 `json:"after_id,omitempty" jsonschema:"从这个事件 id 之后读；不传则从你上次 ack_events 确认的位置接着读"`
	Limit   int    `json:"limit,omitempty" jsonschema:"最多返回几条，默认 50，最多 100"`
}

type mcpAckEventsIn struct {
	UpToID int64 `json:"up_to_id" jsonschema:"确认到这个事件 id（含）为止都处理完了；一般传 list_events 返回的 next_after_id"`
}

// mcpEvent 是一条事件。payload 的字段随 type 变（docs/AI接口.md「事件」）。
type mcpEvent struct {
	ID        int64          `json:"id"`
	Type      string         `json:"type" jsonschema:"stock_low / refund_created / search_zero_spike / proposal_decided"`
	StoreID   *int64         `json:"store_id,omitempty" jsonschema:"事件属于哪家门店；没有即全店口径"`
	Payload   map[string]any `json:"payload"`
	CreatedAt time.Time      `json:"created_at"`
}

type mcpEventsOut struct {
	Items       []mcpEvent `json:"items"`
	Cursor      int64      `json:"cursor" jsonschema:"你上次 ack_events 确认到的事件 id（从没确认过是 0）"`
	NextAfterID int64      `json:"next_after_id" jsonschema:"这一页最后一条的 id；处理完就 ack_events(next_after_id)"`
	HasMore     bool       `json:"has_more" jsonschema:"后面还有，接着 list_events(after_id=next_after_id)"`
}

type mcpAckOut struct {
	Cursor int64 `json:"cursor" jsonschema:"确认之后的游标（只进不退）"`
}

func writeAgentEventError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrAgentEventBadRequest) {
		writeProblemDetail(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求参数不合法", err)
		return
	}
	writeAdminListError(c, err)
}

func registerMCPEventTools(srv *mcp.Server, d *MCPDeps) {
	mcpTool(srv, d, "list_events",
		"拉取你管辖范围内的新事件（库存跌破预警线、买家申请售后、无结果词突增、提案有了结果），按 id 升序。"+
			"不传 after_id 就从上次 ack_events 的位置接着读；处理完一页用 ack_events(next_after_id) 确认。",
		writeAgentEventError, func(ctx context.Context, in mcpListEventsIn) (mcpEventsOut, error) {
			page, err := d.Events.ListForAgent(ctx, in.AfterID, in.Limit)
			if err != nil {
				return mcpEventsOut{}, err
			}
			out := mcpEventsOut{Items: make([]mcpEvent, 0, len(page.Items)), Cursor: page.Cursor,
				NextAfterID: page.NextAfterID, HasMore: page.HasMore}
			for _, e := range page.Items {
				p := map[string]any{}
				_ = json.Unmarshal(e.Payload, &p)
				out.Items = append(out.Items, mcpEvent{ID: e.ID, Type: e.Type, StoreID: e.StoreID, Payload: p,
					CreatedAt: e.CreatedAt})
			}
			return out, nil
		})
	mcpTool(srv, d, "ack_events", "确认事件处理到哪一条（含）为止：之后 list_events 不带 after_id 就从这里之后读。游标只进不退。",
		writeAgentEventError, func(ctx context.Context, in mcpAckEventsIn) (mcpAckOut, error) {
			cur, err := d.Events.Ack(ctx, in.UpToID)
			if err != nil {
				return mcpAckOut{}, err
			}
			return mcpAckOut{Cursor: cur}, nil
		})
}
