package app

import (
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
)

// 按来源 IP 的粗粒度限流。**只给 /search 用。**
//
// ===========================================================================
// 一、它挡的是什么：20 个并发就能让全站掉到关键词搜索
// ===========================================================================
//
// /search 在契约里是 security: []（公开无鉴权，TestSearchIsPublic 钉着），
// 而它每一次请求都要在 CPU 上跑一次模型推理。验收实测：20 个并发的 198 字
// 查询（**契约允许的长度**）打进去之后，同时发的 5 个正常短查询全部从
// `n=5 lat=55` 变成 `n=1 lat=250` —— 整站掉到纯关键词，压力结束约 5 秒恢复。
//
// 也就是说单机一条 for 循环就能让一个公开 Demo 的语义搜索变成关键词搜索，
// 而访客看不出任何异常：响应是 200，strategy 照回 "rrf-v1"，只有日志里
// 多了一串降级 WARN。路线图写着 M3 结束就公开，所以这不是优化。
//
// ===========================================================================
// 二、为什么是进程内的 token bucket，以及它**不**是什么
// ===========================================================================
//
// 不引入 Redis、也不引入 golang.org/x/time/rate。前者是一个新的运行时依赖
// **和一个新的部署组件**；后者虽然小，但这个仓库主模块的运行时依赖只有一个
// （x/crypto，而且它本来就在依赖图里），为一个三十行的桶再加一棵树不划算。
//
// **说清楚它不是什么，免得它读起来像一个分布式限流：**
//
//	· 计数在**进程内存**里。多实例部署时每个实例各有一份配额，
//	  实际放行量是 实例数 × 这里配的数。要精确就得把计数挪到共享存储，
//	  那是另一个量级的东西（一次网络往返 / 一个新组件），本轮不做。
//	· 按**来源 IP** 分桶，而 IP 是可以换的。它挡的是「一台机器一条 for 循环」
//	  这种最廉价的打法，挡不住分布式的。
//	· 它是**速率**闸门，不是**并发**闸门。20 个真正同时到达的请求里，
//	  前 burst 个会一起放进去，它们仍然会一起压在引擎上。真正对症的是给
//	  引擎调用加一个全局并发上限（信号量），那一条列为 defer ——
//	  它要改的是 service 层与引擎客户端的形状，不该夹在这一轮里。
//
// 即便如此这道闸门是值得的，但**它没有把这个问题消灭掉**，本机对着真引擎
// （compose.inference.yaml，BGE-M3 跑在 CPU 上）复量了一次，两个数都记在这里：
//
//	· 20 个 198 字的并发打进来：8 个放行、**12 个当场 429**（默认 burst 8）。
//	  攻击者要维持压力就得持续被拒，成本从「一条 for 循环」变成「持续被拒」。
//	· 但那 8 个仍然一起压在引擎上，同时发的 5 个正常短查询照样
//	  **n=1 latency_ms=260**（纯关键词）。也就是说爆炸半径小了，没有归零。
//
// 把 burst 调到 1、2 能进一步压住它，代价是真人「连点两下搜索」就被拒 ——
// 那是拿可用性换的，不划算。真正对症的是给引擎调用加一个**全局并发上限**
// （信号量）：让前 N 个跑满速、其余的立刻降级，而不是让 8 个一起慢下来、
// 全都降级。那一条要改 service 层与引擎客户端的形状，列为 defer，
// 上面这两个数就是它的依据。
//
// ===========================================================================
// 三、内存不能被来源 IP 撑爆
// ===========================================================================
//
// 一个「map[IP]桶」的限流器本身就是一个内存放大器：攻击者每换一个源 IP
// 就让我们多留一条记录。所以桶有闲置回收（sweepEvery / idleTTL），
// 而且回收是在加锁的写路径上顺带做的，不另起 goroutine —— 一个后台
// goroutine 要有人负责停掉它，而 Router 没有生命周期。

const (
	// EnvSearchRateLimit 每个 IP 每秒可以打几次 /search。<= 0 关闭限流。
	EnvSearchRateLimit = "KEEL_SEARCH_RATE_PER_SEC"

	// EnvSearchRateBurst 瞬时可以攒多少个额度。
	EnvSearchRateBurst = "KEEL_SEARCH_RATE_BURST"

	// DefaultSearchRatePerSec 默认每 IP 每秒 4 次。
	//
	// 一个人在搜索框里打字，最快也就是几秒一次；4 次/秒对真人是察觉不到的，
	// 对一条 for 循环是立刻见底的。
	DefaultSearchRatePerSec = 4.0

	// DefaultSearchRateBurst 默认瞬时 8 个额度。
	//
	// 留给「用户连点了几下」和「一个页面同时发了几个搜索」。它同时决定了
	// 一次性到达的并发里有多少个会被放进去 —— 验收那次 20 并发会被挡掉 12 个。
	DefaultSearchRateBurst = 8.0
)

// ipRateLimiter 是一组按来源 IP 分的 token bucket。
type ipRateLimiter struct {
	ratePerSec float64
	burst      float64
	idleTTL    time.Duration

	mu      sync.Mutex
	buckets map[string]*tokenBucket
	// nextSweep 到点才扫一遍闲置桶。扫描是 O(桶数)，不能每个请求都做。
	nextSweep  time.Time
	sweepEvery time.Duration

	// now 可替换，测试用。真实时间由 time.Now 提供。
	now func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(ratePerSec, burst float64) *ipRateLimiter {
	return &ipRateLimiter{
		ratePerSec: ratePerSec,
		burst:      burst,
		// 闲置多久就把这条记录丢掉。取「攒满一桶所需时间」的十倍，
		// 至少一分钟：比这更短的话，一个每分钟搜一次的真人每次都从满桶开始，
		// 限流对他等于不存在。
		idleTTL:    maxDuration(time.Minute, time.Duration(burst/math.Max(ratePerSec, 0.001)*10)*time.Second),
		buckets:    map[string]*tokenBucket{},
		sweepEvery: time.Minute,
		now:        time.Now,
	}
}

// allow 取一个额度。返回 false 时第二个值是建议的退避秒数（Retry-After）。
func (l *ipRateLimiter) allow(key string) (bool, int) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if d := now.Sub(b.last); d > 0 {
		b.tokens = math.Min(l.burst, b.tokens+d.Seconds()*l.ratePerSec)
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// 还差多久能攒够一个额度。至少回 1 —— Retry-After: 0 等于「立刻重试」，
	// 那正好是限流要挡的行为。
	wait := int(math.Ceil((1 - b.tokens) / l.ratePerSec))
	if wait < 1 {
		wait = 1
	}
	return false, wait
}

// sweepLocked 丢掉闲置太久的桶。调用方必须持锁。
func (l *ipRateLimiter) sweepLocked(now time.Time) {
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(l.sweepEvery)
	for k, b := range l.buckets {
		// 只丢「已经攒满」的：没攒满说明它最近还在被限，丢掉等于给它清零重来。
		if now.Sub(b.last) > l.idleTTL && b.tokens+now.Sub(b.last).Seconds()*l.ratePerSec >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// rateLimitByIP 是那道中间件。lim 为 nil 时它什么都不做。
func rateLimitByIP(lim *ipRateLimiter) gin.HandlerFunc {
	if lim == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		// gin 的 ClientIP 会读 X-Forwarded-For / X-Real-IP，而那两个头是
		// 客户端可以随便写的 —— 除非前面确实有一个会覆写它们的反向代理。
		// gin 默认的 TrustedProxies 是「全部信任」，也就是说 ClientIP 在
		// 裸跑时等于「攻击者自己说他是谁」，限流形同虚设。
		//
		// 所以这里用 RemoteIP：它只认 TCP 连接的对端地址。代价是部署在
		// 反向代理后面时全部流量会算到代理那一个 IP 上 —— 那种部署要显式
		// 配 TrustedProxies 并改用 ClientIP，是一次有意识的动作，
		// 不该是默认行为。这笔账写在这里。
		if ok, retry := lim.allow(c.RemoteIP()); !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			problem.Write(c, http.StatusTooManyRequests, problem.TypeRateLimited,
				"请求过于频繁，请稍后再试")
			return
		}
		c.Next()
	}
}

// searchRateLimiterFromEnv 按环境变量造限流器。配 0 或负数就是关掉。
func searchRateLimiterFromEnv() *ipRateLimiter {
	rate := envFloat(EnvSearchRateLimit, DefaultSearchRatePerSec)
	burst := envFloat(EnvSearchRateBurst, DefaultSearchRateBurst)
	if rate <= 0 || burst < 1 {
		return nil
	}
	return newIPRateLimiter(rate, burst)
}

func envFloat(key string, def float64) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
