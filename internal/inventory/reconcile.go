package inventory

// 阶段 2 的对账读口：库存行的键（StockKeys）。进程内实现、HTTP handler、客户端都在这里。
// 形状与 http.go 相同（POST + JSON，读失败归 ErrUnavailable）。

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/repository"
)

const pathStockKeys = "/inventory/stock/keys"

type stockKeyDTO struct {
	SKUID   int64 `json:"sku_id"`
	StoreID int64 `json:"store_id"`
}

type stockKeysReq struct {
	After stockKeyDTO `json:"after"`
	Limit int         `json:"limit"`
}

type stockKeysResp struct {
	Keys []stockKeyDTO `json:"keys"`
}

func keysLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultKeysPage
	case n > maxBatch:
		return maxBatch
	}
	return n
}

func (l *Local) StockKeys(ctx context.Context, q KeysQuery) ([]StockKey, error) {
	limit := keysLimit(q.Limit)
	out := []StockKey{}
	err := l.store.WithTenant(ctx, func(tx repository.InventoryStoreTx) error {
		rows, err := tx.StockKeys(ctx, repository.StockKey{SKUID: q.After.SKUID, StoreID: q.After.StoreID}, int32(limit))
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, StockKey{SKUID: r.SKUID, StoreID: r.StoreID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (h handler) stockKeys(c *gin.Context) {
	var in stockKeysReq
	if !bind(c, &in) {
		return
	}
	keys, err := h.svc.StockKeys(c.Request.Context(), KeysQuery{
		After: StockKey{SKUID: in.After.SKUID, StoreID: in.After.StoreID}, Limit: in.Limit,
	})
	if err != nil {
		fail(c, err)
		return
	}
	out := stockKeysResp{Keys: make([]stockKeyDTO, 0, len(keys))}
	for _, k := range keys {
		out.Keys = append(out.Keys, stockKeyDTO{SKUID: k.SKUID, StoreID: k.StoreID})
	}
	c.JSON(http.StatusOK, out)
}

func (r *Remote) StockKeys(ctx context.Context, q KeysQuery) ([]StockKey, error) {
	var resp stockKeysResp
	if err := r.read(ctx, pathStockKeys, stockKeysReq{
		After: stockKeyDTO{SKUID: q.After.SKUID, StoreID: q.After.StoreID}, Limit: keysLimit(q.Limit),
	}, &resp); err != nil {
		return nil, err
	}
	out := make([]StockKey, 0, len(resp.Keys))
	for _, k := range resp.Keys {
		out = append(out, StockKey{SKUID: k.SKUID, StoreID: k.StoreID})
	}
	return out, nil
}
