package traceid

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/tenant"
)

// ===========================================================================
// 请求访问日志：这个进程里「一条请求发生了什么」的唯一入口
// ===========================================================================
//
// 包本身早就把号铺好了（HTTP 头、traceparent、SAGA query 传参、gid 绑定），
// 缺的不是号，是**业务代码里没人用它**。挂上这一道之后，每条请求都有一条
// 结构化记录，而「要不要全量记」由运行时的采样开关决定 —— 不必重启。
//
// ### 为什么默认不采样
//
// 这个进程实测峰值约 500 RPS（压测记录 docs/压测与破坏性测试-2026-10-08.md）。
// 全量记是每天几千万行，而排障真正要看的是「慢的那几条」和「错的那几条」。
// 采样率 0 不代表什么都不记 —— 出错的与慢的**始终**记（见 shouldLog），
// 采样率控制的是「剩下的正常请求记不记」。
//
// ### 三个开关的分工
//
//	采样率 0（默认）  只记慢的、错的、被点名的
//	采样率 >0        再按比例带上正常请求。100 = 全量
//	白名单            无论采样率多少，这些 trace_id 一律全量记，且立刻生效
//
// 白名单才是排障的常用姿势：「客人给了个 trace_id，我只想看这一条」。
// 它不需要动全局采样率，也就不会把噪声一起打开。

// 慢请求的门槛（毫秒）。超过就算「不正常的慢」，无条件记。
//
// 15ms 不是拍出来的：inference 客户端给查询侧的预算是 15ms（同client.go 的
// timeout 注释），一个超过这个数的请求已经比它该有的样子慢了一个量级。
const slowMillis = 200

// 这几个路径无条件不记，不管采样率多少。
//
// 健康检查是探针在打（每几秒一次），记它等于把真正的日志淹掉；就绪检查同理。
// 它们的 status 已经由探针自己看过了。
var alwaysSilent = map[string]bool{
	"/healthz": true,
	"/readyz":  true,
	"/livez":   true,
}

// accessState 是这一道中间件的全部可变状态。
//
// 三个字段各自一种并发形状：采样率要无锁读、且能在运行时被改（后台接口改它，
// 中间件在每个请求上读它）；白名单是无界的 sync.Map（删不掉的东西放在一个可
// 枚举的数组里迟早出事）；开关计数只用于观察，容忍一点丢失。
type accessState struct {
	// rate 是百分之一为单位：25 表示 25%。用百分之一而不是 float64 是为了
	// 读它不落数据竞争（atomic 只保证整数/指针这类无锁读）。
	rate atomic.Int64

	// forced 是「这些 trace_id 全量记」。存的是空结构体 —— 不关心值，只问在不在。
	forced sync.Map

	// 三个计数器，供 GetStats 读。都是「本进程启动以来」的累计值。
	kept    atomic.Int64
	dropped atomic.Int64
	// forcedHit 单独数，是为了看白名单到底有没有被用过 ——
	// 一个从没被用过的功能和一个坏掉的功能看起来一模一样。
	forcedHit atomic.Int64
}

var access = &accessState{}

// ===========================================================================
// 开关
// ===========================================================================

// SetSampleRate 设全局采样率（百分之一为单位：25 = 25%，100 = 全量）。
//
// 出范围的值当作 0（只记慢的与错的），而不是报错 —— 这是个运维开关，
// 传错一个 250 的后果应该是「没有生效」而不是「接口 500」。
//
// 生效是即时的：下一次请求读到的就是新值，没有需要重建的东西。
func SetSampleRate(pct int64) {
	if pct < 0 || pct > 100 {
		pct = 0
	}
	access.rate.Store(pct)
}

// SampleRate 读当前采样率（百分之一为单位）。
func SampleRate() int64 { return access.rate.Load() }

// ForceOn 把一个 trace_id 加进白名单：无论采样率多少，它的每次请求都记全量。
//
// 形状不对就返回 false 而不静默接受 —— 白名单是用来查特定请求的，
// 存进去一个不存在的号等于给运维一个「开了但没用」的假象。
func ForceOn(id string) bool {
	id = Normalize(id)
	if id == "" {
		return false
	}
	access.forced.Store(id, struct{}{})
	return true
}

// ForceOff 把一个号移出白名单。
func ForceOff(id string) bool {
	id = Normalize(id)
	if id == "" {
		return false
	}
	if _, loaded := access.forced.LoadAndDelete(id); !loaded {
		return false
	}
	return true
}

// Forced 问一个号在不在白名单里。
func Forced(id string) bool {
	id = Normalize(id)
	if id == "" {
		return false
	}
	_, ok := access.forced.Load(id)
	return ok
}

// ForcedList 列白名单（后台接口要看）。
func ForcedList() []string {
	out := make([]string, 0, 8)
	access.forced.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
		return true
	})
	return out
}

// Stats 是开关的当前状态，GET 接口原样返回。
type Stats struct {
	// SampleRate 是百分之一为单位的采样率。
	SampleRate int64 `json:"sample_rate"`
	// Kept / Dropped 是本次进程启动以来记下与丢掉的请求数。
	Kept    int64 `json:"kept"`
	Dropped int64 `json:"dropped"`
	// ForcedHit 是白名单命中的次数。看白名单有没有被用过。
	ForcedHit  int64    `json:"forced_hit"`
	Forced     []string `json:"forced"`
	SlowMillis int      `json:"slow_millis"`
}

// GetStats 读开关状态。
func GetStats() Stats {
	return Stats{
		SampleRate:  access.rate.Load(),
		Kept:        access.kept.Load(),
		Dropped:     access.dropped.Load(),
		ForcedHit:   access.forcedHit.Load(),
		Forced:      ForcedList(),
		SlowMillis:  slowMillis,
	}
}

// resetForTest 把开关归零。只有测试用。
func resetForTest() {
	access = &accessState{}
}

// ===========================================================================
// 中间件
// ===========================================================================

// AccessLog 给每个请求记一条结构化日志（默认按采样规则）。
//
// 刻意不记请求体也不记响应体：webhook 报文里有金额与流水号，登录报文里有口令。
// 需要报文的那一处（payments.notify_payload）已经落了库，那里有 RLS 管着。
// 这条约束照抄 logHandlerErrors 的注释，它已经踩过一次「日志记了但没人在意」的坑。
//
// 刻意不记 query 全文：token 与密码在 query 里是常态。
//
// **挂在 traceid.Middleware() 之后** —— 号必须已经在 ctx 里，否则这条日志
// 少了最关键的那一列。它自己会取号而不依赖 gin 的 c.Set。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.FullPath()
		if path == "" {
			// 没匹配上路由的请求：FullPath() 是空的。用 URL.Path 顶上，
			// 否则「有人在扫不存在的路径」在日志里就是一片空白。
			path = c.Request.URL.Path
		}

		c.Next()

		status := c.Writer.Status()
		dur := time.Since(start)

		if !shouldLog(path, status, dur, From(c.Request.Context())) {
			access.dropped.Add(1)
			return
		}

		// 过了闸才取租户：这是一次不该记的请求时，不该为它查任何东西。
		ctx := c.Request.Context()
		attrs := []slog.Attr{
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", status),
			slog.Duration("duration", dur),
		}
		if id, err := tenant.FromContext(ctx); err == nil {
			attrs = append(attrs, slog.Int64("merchant_id", id))
		}

		// 分三级：5xx 是服务端的错，4xx 是「有人得知道」但不是我们错
		//（404 在扫子域名、409 是同一把幂等键还在飞），其余是正常流量。
		log := logFor(ctx)
		access.kept.Add(1)
		switch {
		case status >= 500:
			log.ErrorContext(ctx, "请求", toAny(attrs)...)
		case status >= 400:
			log.WarnContext(ctx, "请求", toAny(attrs)...)
		default:
			log.InfoContext(ctx, "请求", toAny(attrs)...)
		}
	}
}

// logFor 拿一个**保证会补trace_id** 的 logger。
//
// 为什么不能直接 slog.Default()：trace_id 那一列是包里的 handler 补上的，
// 而 Install() 只在app.Run 里调一次。谁在这之后替换了默认 handler
// （测试换一个、或运维接了别的日志库），这一列就静默消失 ——
// 而访问日志的全部价值就在这一列上，它悄悄没了没人会知道。
//
// 这一步顺带保证了 Install() 没被调过时仍然是对的：trace_id 来自 ctx，
// 而号是 Middleware() 放进去的，与 Install 无关。已经包过就直接拿现成的，
// 不重复包（包两层会让同一条日志出现两个 trace_id）。
func logFor(ctx context.Context) *slog.Logger {
	h := slog.Default().Handler()
	if _, ok := h.(*handler); ok {
		return slog.Default()
	}
	return slog.New(Wrap(h))
}

// shouldLog 是这一道闸的全部判据。四条独立成立即为记：
//
//	① 白名单里有这个 trace_id —— 有人点名要看的，一律记，**不看采样率**
//	② status >= 400 —— 出错的。4xx 与 5xx 的差别在打日志时分级，不在要不要记
//	③ 耗时 >= slowMillis —— 不正常的慢。慢请求往往比报错更难查
//	④ 随机数落在采样率里 —— 运维手动开的那部分
//
// 健康检查在最前面无条件放行（见 alwaysSilent）：它一条都不该记，
// 否则采样率开着的时候日志会被探针刷满。
func shouldLog(path string, status int, dur time.Duration, id string) bool {
	if alwaysSilent[path] {
		return false
	}
	if id != "" {
		if _, ok := access.forced.Load(id); ok {
			access.forcedHit.Add(1)
			return true
		}
	}
	if status >= 400 {
		return true
	}
	if dur >= slowMillis*time.Millisecond {
		return true
	}
	r := access.rate.Load()
	if r <= 0 {
		return false
	}
	if r >= 100 {
		return true
	}
	return int64(rand.IntN(100))+1 <= r
}

// toAny 把 []slog.Attr 摊成 []any，供 slog 的 ...any 形式用。
// 这道转换每请求一次，代价是一次分配与一个循环；AccessLog 里还有网络 I/O
// 与 JSON 编码，比这贵得多。
func toAny(attrs []slog.Attr) []any {
	out := make([]any, 0, len(attrs)*2)
	for _, a := range attrs {
		out = append(out, a.Key, a.Value)
	}
	return out
}