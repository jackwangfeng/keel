package service

import (
	"sync"
	"time"
)

// 买家商品列表的总数缓存（2026-10-01，性能压测 docs/性能压测-2026-10.md 六 ①）。
//
// ### 为什么要缓存
//
// 契约里 PageMeta.total 是必填的，而它是 O(全店) 的一次计数：10 万商品时 CountProducts 41 ms、
// 「只看有货」的 CountProductsInStock 110 ms（改写加索引之后 ≈70 ms），取那一页本身只要 0.75 ms。
// 每个请求都数一遍，列表 227 RPS 就把 PG 的 16 核打满了 —— 九成以上的 CPU 花在一个
// 翻页时根本不会变的数上。
//
// ### 为什么选「进程内按键缓存 30 秒」
//
//   - 契约不动：total 仍是精确计数，只是可能晚几十秒。契约里 PageMeta.total 只是一个必填的
//     integer，没有承诺与 items 同一快照；「超过阈值返回估算值」那条路等于改它的语义
//     （客户端拿它算总页数，估算值会让最后几页翻不到或翻出空页），契约没有写「约」，不走。
//   - 「只在第 1 页算、后续页复用」不够：第 1 页恰恰是最热的那一页（默认列表前 3 页的流量里
//     大半是第 1 页），它照样每次都数。改成谁先来谁算、30 秒内大家复用，第 1 页也省了。
//   - 翻页时 total 稳定反而更好：客户端按 total 算「还有没有下一页」，每页都重数的话
//     上下架会让它在翻页途中跳来跳去。
//   - 不按「有货标记消息到达就失效」：有货标记一秒可能翻转很多次（下单高峰），按消息失效
//     等于没缓存；而 total 晚 30 秒的代价只是最后一页可能多翻一次空页或少显示几件，
//     客户端本来就要处理（items 少于 page_size 即到底）。
//
// ### 键与边界
//
// 键是（商户, 门店, 大区, 类目, 只看有货）。**商户必须在键里**：门店 id 全局唯一，看上去够用，
// 但缓存不该依赖「别家拿不到这家的门店 id」这种前提 —— 键里漏了商户，一旦哪条路径让两家
// 共用了同一组参数，就是一次跨租户的数据外泄，而且不报错、不留痕。
// 类目存的是请求里的那个 id，不是解析后的子树：子树变了（移动类目）也只是晚 30 秒。
//
// 容量有上限（productTotalCacheCap）：键里有类目，枚举类目 id 就能让表无限长。满了先扫掉过期的，
// 还满就随手扔掉一条 —— 被扔的那条下次重数一遍而已，不需要 LRU 的精确性。
//
// 没做「同一个键并发未命中只数一次」（singleflight）：计数在调用方自己的租户事务里跑，
// 让别的请求等着一个事务的结果，要处理那个事务失败、超时时等待方怎么办；而不做的代价是
// 每个键每 30 秒最多「并发数」次计数，与原来每个请求一次相比已经是两个数量级的差。
const (
	productTotalCacheTTL = 30 * time.Second
	productTotalCacheCap = 10000
)

type totalKey struct {
	merchant, store, region int64
	category                int64 // 0 表示不按类目筛（类目 id 是 IDENTITY，从 1 起）
	inStockOnly             bool
}

type totalEntry struct {
	n       int64
	expires time.Time
}

// totalCache 是一个带 TTL、有容量上限的进程内表。零值不可用，用 newTotalCache。
type totalCache struct {
	mu  sync.Mutex
	m   map[totalKey]totalEntry
	ttl time.Duration
	cap int
	now func() time.Time
}

func newTotalCache(ttl time.Duration, capacity int) *totalCache {
	return &totalCache{m: make(map[totalKey]totalEntry), ttl: ttl, cap: capacity, now: time.Now}
}

// get 返回未过期的那个数。ttl <= 0 时整个缓存关闭（恒未命中）。
func (c *totalCache) get(k totalKey) (int64, bool) {
	if c == nil || c.ttl <= 0 {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return 0, false
	}
	if !c.now().Before(e.expires) {
		delete(c.m, k)
		return 0, false
	}
	return e.n, true
}

func (c *totalCache) put(k totalKey, n int64) {
	if c == nil || c.ttl <= 0 || c.cap <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if _, exists := c.m[k]; !exists && len(c.m) >= c.cap {
		for kk, e := range c.m {
			if !now.Before(e.expires) {
				delete(c.m, kk)
			}
		}
		for kk := range c.m {
			if len(c.m) < c.cap {
				break
			}
			delete(c.m, kk)
		}
	}
	c.m[k] = totalEntry{n: n, expires: now.Add(c.ttl)}
}

func (c *totalCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
