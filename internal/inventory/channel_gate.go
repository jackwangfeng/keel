package inventory

// 渠道层的库存变化通知（stock.changed）的闸门：只有「开了渠道的商家」才发。
// 设计见 docs/superpowers/specs/2026-10-02-channel-adapter-design.md §8。
//
// 两级：
//
//   - 进程开关（KEEL_CHANNELS，默认关）：关着时 allows 一律 false，**一次库都不查** ——
//     没有渠道的部署里，库存写路径与没有渠道层时逐条语句相同（不变量「不配渠道零开销」）；
//   - 开着时按商家：查库存库的 channel_merchants（00300），进程内缓存 channelGateTTL。
//     本进程收到 core 的「开 / 关渠道」消息时直接 Remember，不等 TTL；多实例部署里别的实例最多晚 TTL 生效。
//
// 查询在改库存的那个事务里做（同一条连接，不多要连接），只在缓存未命中时一次。

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const channelGateTTL = 30 * time.Second

// channelMerchantLookup 是 allows 要的那一个查询（repository.InventoryStoreTx 满足它）。
type channelMerchantLookup interface {
	ChannelMerchantEnabled(ctx context.Context) (bool, error)
}

type ChannelGate struct {
	enabled bool
	now     func() time.Time

	mu    sync.Mutex
	cache map[int64]gateEntry

	queries atomic.Int64
}

type gateEntry struct {
	on bool
	at time.Time
}

// NewChannelGate 建闸门。enabled 是进程开关（KEEL_CHANNELS）。nil 闸门等于关。
func NewChannelGate(enabled bool) *ChannelGate {
	return &ChannelGate{enabled: enabled, now: time.Now, cache: map[int64]gateEntry{}}
}

// Enabled：进程开关开着。
func (g *ChannelGate) Enabled() bool { return g != nil && g.enabled }

// Queries 是查过库的次数，只为可观察（不变量测试断言开关关闭时为 0）。
func (g *ChannelGate) Queries() int64 {
	if g == nil {
		return 0
	}
	return g.queries.Load()
}

// allows：这家商家要不要发 stock.changed。查询失败原样返回（不缓存）。
func (g *ChannelGate) allows(ctx context.Context, q channelMerchantLookup, merchantID int64) (bool, error) {
	if !g.Enabled() {
		return false, nil
	}
	now := g.now()
	g.mu.Lock()
	e, ok := g.cache[merchantID]
	g.mu.Unlock()
	if ok && now.Sub(e.at) < channelGateTTL {
		return e.on, nil
	}
	g.queries.Add(1)
	on, err := q.ChannelMerchantEnabled(ctx)
	if err != nil {
		return false, err
	}
	g.mu.Lock()
	g.cache[merchantID] = gateEntry{on: on, at: now}
	g.mu.Unlock()
	return on, nil
}

// Remember 记下一家商家的最新状态（本进程刚应用了 core 的消息）。
func (g *ChannelGate) Remember(merchantID int64, on bool) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	g.cache[merchantID] = gateEntry{on: on, at: g.now()}
	g.mu.Unlock()
}

// Forget 让下一次判定重查库。
func (g *ChannelGate) Forget(merchantID int64) {
	if !g.Enabled() {
		return
	}
	g.mu.Lock()
	delete(g.cache, merchantID)
	g.mu.Unlock()
}
