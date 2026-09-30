package rpc

import (
	"sync"
	"time"
)

// 熔断器：库存服务卡住（不是挂掉 —— 挂掉的话连接当场被拒，立刻就失败）时，
// 每个读请求都要等满超时才降级。商品列表、试算、购物车、检索每页都要读一次库存，
// 于是库存服务一卡，这几个页面每个请求都多等一个超时，公网那一侧的连接与 goroutine
// 也跟着一起堆。熔断器在连续失败 N 次之后直接判「不可用」，不再去等。
//
// 状态机就三个状态：
//
//	关闭   正常放行。连续失败到 threshold 次 → 打开。任何一次成功把计数清零。
//	打开   一律不发，直接返回 ErrCircuitOpen。cooldown 过后 → 半开。
//	半开   只放**一个**探测请求过去，其余照样直接失败：
//	       探测成功 → 关闭；失败 → 回到打开，重新计冷却。
//
// 「失败」只算结果未知（连不上、超时、5xx）：4xx 是对面回答了（签名不对、入参不合法），
// 那是配置或代码的错，不是对面不在 —— 把它算进来的话，一个带着坏参数的调用方能把
// 所有人的库存读一起熔断掉。调用方自己取消的（公网请求断了）也不算：那不说明对面怎样。
//
// 只给读用（Client.ReadJSON）。写与 SAGA 分支不走它：写要的是「结果未知时由调用方决定
// 重试」那一套语义（见 client.go 的错误分类），多一种「没发出去」的失败只会让每个写的
// 调用方多学一种错误；而写都在后台与 outbox 里，慢一点不拖公网页面。
type breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu       sync.Mutex
	failures int       // 关闭状态下的连续失败数
	openedAt time.Time // 非零 = 打开（或半开）
	probing  bool      // 半开状态下已经放了一个探测出去
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// allow 决定这一次放不放。放的话调用方**必须**用 done 报告结果（成功 / 失败 / 不算）。
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true
	}
	if b.now().Sub(b.openedAt) < b.cooldown || b.probing {
		return false
	}
	b.probing = true // 半开：就放这一个
	return true
}

// outcome 是一次放行之后的结果。
type outcome int

const (
	outcomeSuccess outcome = iota // 对面回答了（2xx 或 4xx）
	outcomeFailure                // 结果未知：连不上、超时、5xx
	outcomeIgnored                // 调用方自己取消：不说明对面怎样
)

func (b *breaker) done(o outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	halfOpen := b.probing
	b.probing = false
	switch o {
	case outcomeSuccess:
		b.failures = 0
		b.openedAt = time.Time{}
	case outcomeFailure:
		if halfOpen {
			b.openedAt = b.now() // 探测失败：重新打开，重新计冷却
			return
		}
		b.failures++
		if b.failures >= b.threshold && b.openedAt.IsZero() {
			b.openedAt = b.now()
		}
	case outcomeIgnored:
		// 半开的探测被调用方取消了：什么都没学到，下一个请求再探。
	}
}

// state 只给测试与日志：closed / open / half-open。
func (b *breaker) state() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.openedAt.IsZero():
		return "closed"
	case b.now().Sub(b.openedAt) < b.cooldown:
		return "open"
	default:
		return "half-open"
	}
}
