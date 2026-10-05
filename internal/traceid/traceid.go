// Package traceid 是一条请求在多个进程之间共用的跟踪号。
//
// 搜索归因用的 search_logs.trace_id 是另一回事：那一列只把一次检索和后来的
// 点击、加购对上。这里的号跟着这一次调用走，公网请求、内网 RPC、协调器打回来的
// 库存分支，日志里是同一个 trace_id。
//
// 号是 32 位小写十六进制（16 字节）。进来认两个头：X-Trace-Id，以及 W3C
// traceparent 里的 trace-id。形状不对就丢掉自己生成，不把调用方的字符串写进日志。
package traceid

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Header 是对外、对内都带的请求头。响应里也回它，报障时对得上日志。
const Header = "X-Trace-Id"

// 协调器打分支时不会转发我们的请求头，只会保留编排时写在 URL 上的 query。
// 所以跨进程的 SAGA 把号放在这个参数里，和分支令牌同一条路。
const queryKey = "trace"

type ctxKey struct{}

// With 把跟踪号放进 ctx。id 形状不对时原样返回。
func With(ctx context.Context, id string) context.Context {
	id = Normalize(id)
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// From 取出 ctx 里的跟踪号。没有就是空串。
func From(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// Normalize 只接受 32 位十六进制。别的（空、过长、带换行）一律空串。
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return s
}

// New 生成一个新号。熵源失败时用时间加计数顶上，请求不能没有号。
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(b[8:], fallbackSeq.Add(1))
	}
	return hex.EncodeToString(b[:])
}

var fallbackSeq atomic.Uint64

// Incoming 从调用方带来的两个头里取号，都没有或不合法就新生成一个。
// traceparent 取 W3C 的 trace-id 那一段（00-{trace}-{span}-{flags}）。
func Incoming(header, traceparent string) string {
	if id := Normalize(header); id != "" {
		return id
	}
	if id := fromTraceparent(traceparent); id != "" {
		return id
	}
	return New()
}

func fromTraceparent(h string) string {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || parts[0] != "00" {
		return ""
	}
	return Normalize(parts[1])
}

// Append 把跟踪号加到分支 URL 的 query 上。local:// 与空号不动。
// 不重新编码已有的 query：分支令牌那一段的字节要和编排时写下的一致。
func Append(raw, id string) string {
	id = Normalize(id)
	if id == "" || raw == "" || strings.HasPrefix(raw, "local://") {
		return raw
	}
	if strings.Contains(raw, "?"+queryKey+"=") || strings.Contains(raw, "&"+queryKey+"=") {
		return raw
	}
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + queryKey + "=" + id
}

// binds 让「跑在另一个 goroutine 里的分支」看见这次提交的跟踪号。
// 协调器回调不带我们的 context。号按 gid 记，分支开始时取走。
var binds sync.Map

// Hold 在提交 SAGA 之前记下 gid → 跟踪号，返回的函数在等待结束时撤掉。
// ctx 里没有号、或 gid 为空时，返回的函数什么都不做。
func Hold(ctx context.Context, gid string) func() {
	id := From(ctx)
	if id == "" || gid == "" {
		return func() {}
	}
	binds.Store(gid, id)
	return func() { unbind(gid, id) }
}

// Adopt 在一次分支调用期间挂上号。已经有人记过同一个 gid 时不动那一条，
// 返回的函数也不撤：撤的权利在先记下的那一方（通常是还在等终态的提交方）。
func Adopt(gid, id string) func() {
	id = Normalize(id)
	if gid == "" || id == "" {
		return func() {}
	}
	if _, loaded := binds.LoadOrStore(gid, id); loaded {
		return func() {}
	}
	return func() { unbind(gid, id) }
}

func unbind(gid, id string) {
	if v, ok := binds.Load(gid); ok && v == id {
		binds.CompareAndDelete(gid, id)
	}
}

// Context 若这个 gid 记着跟踪号，就放进 ctx。分支日志用它。
func Context(ctx context.Context, gid string) context.Context {
	v, ok := binds.Load(gid)
	if !ok {
		return ctx
	}
	id, _ := v.(string)
	return With(ctx, id)
}

// Branch 是分支入口用的：ctx 带上这个 gid 的跟踪号，logger 多一列 trace_id。
// 分支跑在协调器的 goroutine 里，没有当初那个 HTTP 请求的 context。
func Branch(log *slog.Logger, gid string) (context.Context, *slog.Logger) {
	ctx := Context(context.Background(), gid)
	if id := From(ctx); id != "" && log != nil {
		log = log.With("trace_id", id)
	}
	return ctx, log
}

// Install 让凡是带了跟踪号的 context 打日志都自动多一列 trace_id。
// 重复调用不会套两层。
func Install() {
	h := slog.Default().Handler()
	if _, ok := h.(*handler); ok {
		return
	}
	slog.SetDefault(slog.New(Wrap(h)))
}

// Wrap 在下一条日志上补 trace_id。已经包过的 handler 原样返回。
func Wrap(next slog.Handler) slog.Handler {
	if _, ok := next.(*handler); ok {
		return next
	}
	return &handler{next: next}
}

type handler struct{ next slog.Handler }

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if id := From(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{next: h.next.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{next: h.next.WithGroup(name)}
}
