package shopify

import (
	"context"

	"github.com/keel/keel/internal/channel"
)

// LatestOrderID 取店里最近一张订单的 gid（联调用，只读）；没有订单回空串。
func LatestOrderID(ctx context.Context, a *Adapter, b channel.Binding) (string, error) {
	var out struct {
		Orders struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"orders"`
	}
	const q = `query LatestOrder{ orders(first:1, sortKey:CREATED_AT, reverse:true){ nodes{ id } } }`
	if err := a.gql(ctx, b, "LatestOrder", q, map[string]any{}, &out); err != nil {
		return "", err
	}
	if len(out.Orders.Nodes) == 0 {
		return "", nil
	}
	return out.Orders.Nodes[0].ID, nil
}
