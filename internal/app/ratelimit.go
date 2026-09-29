package app

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/problem"
)

// 按来源 IP 的粗粒度限流。给 /search 与 /search/events 用，**两只桶各自独立**
// （后者的额度与理由在 DefaultSearchEventRatePerSec 上）。
//
// ===========================================================================
// 一、它挡的是什么：几十个并发就能让全站掉到关键词搜索
// ===========================================================================
//
// /search 在契约里是 security: []（公开无鉴权，TestSearchIsPublic 钉着），
// 而它每一次请求都要跑一次模型推理。M3 验收实测（BGE-M3 跑在 CPU 上）：
// 20 个并发的 198 字查询（**契约允许的长度**）打进去之后，同时发的 5 个正常
// 短查询全部从 `n=5 lat=55` 变成 `n=1 lat=250` —— 整站掉到纯关键词，
// 压力结束约 5 秒恢复。
//
// 也就是说单机一条 for 循环就能让一个公开 Demo 的语义搜索变成关键词搜索，
// 而访客看不出任何异常：响应是 200，strategy 照回 "rrf-v1"，只有日志里
// 多了一串降级 WARN。路线图写着 M3 结束就公开，所以这不是优化。
//
// **M4 换成 GPU 之后这件事没有消失，只是门槛抬高了。** 下面第二节把新旧两组
// 前提和两次实测都摆出来 —— 这几个默认值是从那里推出来的，不是拍的。
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
// ===========================================================================
// 二之二、这两个默认值是怎么推出来的：M4 换引擎之后重算了一遍
// ===========================================================================
//
// **前提变了，所以数字重算。** 旧的 4/s + burst 8 是按 M3 那台机器推的；
// M4 把引擎从「BGE-M3 跑在 CPU 上」换成「infero + Qwen3-Embedding-0.6B 跑在
// 一块 RTX A4000 上」，单条快了一个数量级，推导的地基整块换了。
// 两组前提都留在这里 —— 不留旧的那组，下次有人想改这两个数时就只能重新拍。
//
//	                       旧（M3，BGE-M3 / CPU）   新（M4，infero / A4000）
//	  单条 198 字              62 ms                  12.7 ms
//	  批 64                    31.3 ms/条             1.56 ms/条
//	  并发下的墙钟吞吐          ≈16 req/s（1/62ms）    ≈82 req/s（实测 20 并发）
//
// 新那一列是**在这台机器上现场量的**：直接对着 infero 打 POST /v1/embeddings
// （198 个汉字的 texts，normalize:true，尾部补池化哨兵），单条取 25 次的中位，
// 并发那一列用线程池同时发 N 条再看墙钟。重量一遍就是重跑这两件事，
// 不需要起 Keel 自己的栈。注意两件事，都会让人把这组数读得太乐观：
//
//	· 12.7 ms 比 M4 切换时记的 7.0 ms（总体架构 §1 的注）慢了近一倍，
//	  因为这块卡此刻还分给另外两个项目。**推导用慢的那个数**——
//	  一个只在卡空着时才成立的限流值，会在最需要它的时候失效。
//	· §8 给 query embedding 的预算是 15 ms，12.7 ms 是压着线过的，不是甩开的。
//
// **burst 怎么定：量出「放行几个并发，旁边的正常查询才开始降级」那条线。**
//
// 降级的判据不是感觉，是 service/search.go 里那条放弃等待线
// （DefaultQueryEmbedTimeout 250 ms + PerRuneEmbedBudget 3 ms × 字数）。
// 一条 3 个字的正常查询，那条线是 250 + 9 = **259 ms**；超过就退回纯关键词。
// 于是「N 个 198 字并发压着时，同时发的 5 条正常短查询里最慢的那条」
// 就是一个可以直接量的东西。本机实测（每档取 3 次的中位）：
//
//	  N 个 198 字并发    8      10     12     14     16     20
//	  短查询最慢          164ms  189ms  212ms  232ms  258ms  307ms
//	  259 ms 那条线       过     过     过     过     贴线    降级
//
// 拐点在 16（258 vs 259，余量为零），20 就塌了。取 **12**：实测 212 ms，
// 离线 47 ms（约 18% 余量），而这块卡是三个项目共用的，余量不能取零。
//
// 顺带一个对照，说明旧值为什么该动：CPU 那一轮里 burst=8 放行的那 8 个
// **仍然**把旁边的短查询打到 n=1 lat=260（降级了）—— 也就是说旧的 burst 8
// 在旧硬件上并没有守住这条线，它只是把爆炸半径缩小了。在 GPU 上 8 个的
// 实测是 164 ms，第一次真的守住了；12 是在守住的前提下把可用性拿回来一点。
//
// **rate 怎么定：一个 IP 能占掉引擎多少。**
//
// 旧值 4/s ÷ 16 req/s ≈ 25%，也就是 4 个 IP 就能把引擎占满。同一条口径放到
// 82 req/s 上是 20/s。取 **12/s**（约 15%，≈7 个 IP 才占得满）而不是 20/s：
// 82 req/s 是在这块卡被另外两个项目分走一部分的情况下量到的，而 rate 是
// **持续**放行量，它比 burst 更不该踩在实测值上。
//
// 12 次/秒对真人仍然是察觉不到的（人在搜索框里打字最快也就几秒一次，
// 一个页面同时发几个搜索也远到不了），对一条 for 循环仍然是立刻见底的
// —— 它能打 82/s，被限到 12/s 意味着 85% 的请求当场 429。
//
// **这道闸门仍然不是并发闸门，这一点没变。** 它是速率闸门：真正对症的是给
// 引擎调用加一个全局并发上限（信号量），让前 N 个跑满速、其余的立刻降级，
// 而不是让 burst 个一起慢下来。那一条仍然列为 defer，要改 service 层与引擎
// 客户端的形状。上面那张表就是它的依据 —— 而且现在这张表告诉我们它的收益
// 比 M3 那时候小：GPU 上 12 个并发压根没把线冲破。
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

	// DefaultSearchRatePerSec 默认每 IP 每秒 12 次。
	//
	// M3 是 4.0（按 BGE-M3 在 CPU 上单条 62 ms、引擎吞吐 ≈16 req/s 推的，
	// 一个 IP 占 25%）。M4 换成 infero + GPU 之后实测吞吐 ≈82 req/s，同一条
	// 「一个 IP 不该占掉引擎四分之一」的口径给出 20/s，这里取 12/s（约 15%）
	// 把余量留出来 —— 完整推导见文件头「二之二」。
	//
	// 12 次/秒对真人察觉不到（搜索框里打字最快也就几秒一次），对一条
	// for 循环仍然立刻见底：它能打 82/s，85% 会当场 429。
	DefaultSearchRatePerSec = 12.0

	// DefaultSearchRateBurst 默认瞬时 12 个额度。
	//
	// M3 是 8.0，而那个值在 CPU 上**没有**守住旁边正常查询的延迟
	// （8 个放行仍然把短查询打到 260 ms，越过 259 ms 的放弃等待线）。
	// GPU 上重新量了一条曲线：12 个并发 198 字压着时，同时发的短查询最慢
	// 212 ms，而拐点在 16（258 ms，贴着 259 ms 那条线）。12 是拐点留
	// 约 18% 余量之后的值 —— 这块卡是三个项目共用的，余量不能取零。
	// 那张表在文件头「二之二」。
	//
	// 它同时决定了一次性到达的并发里有多少个会被放进去：20 并发打进来时
	// 放行 12、当场 429 掉 8 个。
	DefaultSearchRateBurst = 12.0
)

const (
	// EnvSearchEventRateLimit / EnvSearchEventRateBurst 是 /search/events 那只桶。
	// <= 0 关闭限流。
	EnvSearchEventRateLimit = "KEEL_SEARCH_EVENT_RATE_PER_SEC"
	EnvSearchEventRateBurst = "KEEL_SEARCH_EVENT_RATE_BURST"

	// DefaultSearchEventRatePerSec 默认每 IP 每秒 36 次，是 /search 的三倍。
	//
	// **为什么单独一只桶，而不是和 /search 共用**：共用的话，一个刚搜完、
	// 连着点了几件商品的真人会把自己下一次搜索的额度吃掉 —— 回传是辅助数据，
	// 它不该让主业务 429。
	//
	// **为什么是三倍**：一次检索最多有三次**有效**回传（click / add_cart / order
	// 各填一列，首次为准，之后的同类回传是空操作）。一个没超 /search 配额的
	// 客户端，有效回传就不会超 3 × 12/s；超出的那部分全是重复上报或刷量。
	//
	// **为什么这里的限流不是防刷指标的那道闸门**：刷指标挡在 service 里 ——
	// product_id 必须在这次检索返回的 ranked_ids 里、每列首次为准，于是一个
	// trace_id 最多贡献三次写入，发多少次都一样。限流挡的是另一件事：每次回传
	// 是一次点查加一次单行 UPDATE，不限的话一条 for 循环可以把写压力无上限地
	// 推给数据库。它比 /search 便宜得多（不碰推理引擎），所以额度可以宽。
	DefaultSearchEventRatePerSec = 3 * DefaultSearchRatePerSec

	// DefaultSearchEventRateBurst 默认瞬时 36 个额度，同样是 /search 的三倍：
	// 一页结果里连点几件、加购、再下单，都落在一个瞬时窗口里。
	DefaultSearchEventRateBurst = 3 * DefaultSearchRateBurst
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
		// ClientIP 只在连接对端属于 KEEL_TRUSTED_PROXIES 时才读 X-Forwarded-For /
		// X-Real-IP，否则就是 RemoteIP —— 见 trustProxies。那两个头是客户端可以
		// 随便写的，gin 默认却「全部信任」，所以 Router 总会先调 trustProxies
		// 把默认值换掉：不配就一个都不信，配了只信名单里的代理。
		if ok, retry := lim.allow(c.ClientIP()); !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			problem.Write(c, http.StatusTooManyRequests, problem.TypeRateLimited,
				"请求过于频繁，请稍后再试")
			return
		}
		c.Next()
	}
}

// EnvTrustedProxies 是反向代理的地址名单（逗号分隔的 IP 或 CIDR）。
//
// 部署在反向代理后面时，TCP 对端永远是代理，按它限流等于全部访客共用一个桶：
// 一个人刷搜索，所有人一起 429。名单里的来源发来的 X-Forwarded-For / X-Real-IP
// 才会被采信（gin 从右往左跳过受信代理，取第一个不受信的地址）；不在名单里的
// 来源自报什么都不信。**不配就是一个都不信**，和裸跑的安全默认一致。
//
// 只写真正会覆写 / 追加这两个头的代理，写宽了（比如 0.0.0.0/0）等于把
// 「我是谁」交给客户端自己说。
const EnvTrustedProxies = "KEEL_TRUSTED_PROXIES"

// trustProxies 按 raw（EnvTrustedProxies 的值）配 r 的受信代理。
// 空串即不信任任何代理；有一项不是合法的 IP / CIDR 就报错，Run 据此拒绝启动 ——
// 名单写错时静默退回「一个都不信」会让限流又退化成共用一个桶，而且没人发现。
func trustProxies(r *gin.Engine, raw string) error {
	var list []string
	if strings.TrimSpace(raw) != "" {
		for _, item := range strings.Split(raw, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				return fmt.Errorf("%s 里有空项：%q", EnvTrustedProxies, raw)
			}
			if _, _, err := net.ParseCIDR(item); err != nil && net.ParseIP(item) == nil {
				return fmt.Errorf("%s 里的 %q 不是合法的 IP 或 CIDR", EnvTrustedProxies, item)
			}
			list = append(list, item)
		}
	}
	// nil 在 gin 里就是「一个都不信」，ClientIP 退回 RemoteIP。
	return r.SetTrustedProxies(list)
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

// searchEventRateLimiterFromEnv 是 /search/events 那只桶。每次调用造一只**新**桶，
// 与 searchRateLimiterFromEnv 那只互不相干。
func searchEventRateLimiterFromEnv() *ipRateLimiter {
	rate := envFloat(EnvSearchEventRateLimit, DefaultSearchEventRatePerSec)
	burst := envFloat(EnvSearchEventRateBurst, DefaultSearchEventRateBurst)
	if rate <= 0 || burst < 1 {
		return nil
	}
	return newIPRateLimiter(rate, burst)
}

const (
	// EnvTileRateLimit / EnvTileRateBurst 是 /geo/tiles 那只桶。<= 0 关闭限流。
	EnvTileRateLimit = "KEEL_TILE_RATE_PER_SEC"
	EnvTileRateBurst = "KEEL_TILE_RATE_BURST"

	// DefaultTileRatePerSec / DefaultTileRateBurst：一屏地图（手机约 4×6、后台约 6×5 张）两层叠就是
	// 四五十张，拖动、缩放一下又是一屏 —— 瞬时额度按两屏给，持续额度按每秒一屏给。
	// 挡的是一条 for 循环把整个中国的瓦片刷一遍、耗光天地图 key 的日调用量。
	DefaultTileRatePerSec = 60.0
	DefaultTileRateBurst  = 120.0

	// EnvTileUpstreamPerSec / EnvTileUpstreamBurst 是瓦片的**总闸**：全站（不分 IP）每秒最多向瓦片服务商
	// 取多少张（缓存没命中的那些），默认值与理由在 geo.DefaultTileUpstreamPerSec。<= 0 关闭。
	EnvTileUpstreamPerSec = "KEEL_TILE_UPSTREAM_PER_SEC"
	EnvTileUpstreamBurst  = "KEEL_TILE_UPSTREAM_BURST"
)

func tileRateLimiterFromEnv() *ipRateLimiter {
	rate := envFloat(EnvTileRateLimit, DefaultTileRatePerSec)
	burst := envFloat(EnvTileRateBurst, DefaultTileRateBurst)
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
