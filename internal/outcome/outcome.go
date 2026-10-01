// Package outcome 回答一个失败的请求「确定没生效」还是「可能已经留下了什么」。
//
// # 为什么需要它
//
// 数据库语句被 statement_timeout 取消（SQLSTATE 57014 query_canceled）或等锁超过
// lock_timeout（55P03 lock_not_available），是一个**会自己好**的失败：行锁的持有者提交了、
// 那一阵并发过去了，原样重试就会成功。回 500 告诉客户端「服务端有 bug」，是错的；
// 503 + Retry-After 才是它在 HTTP 上的准确说法。
//
// 光换状态码还不够，客户端（以及屏幕前的人）最想知道的是**这次到底生效了没有**：
// 退款审了没有、货发了没有。能不能说「没生效」，要分两层想：
//
//  1. 出错的那一条语句所在的事务：确定回滚了。57014 / 55P03 是服务端回来的
//     ErrorResponse，带 SQLSTATE —— 语句是服务端自己取消的，事务随之进入 aborted 状态，
//     之后只能 ROLLBACK（repository 的事务包装在 fn 返回错误时就走 defer 的 Rollback）。
//     即使取消落在 COMMIT 本身上，服务端回了 SQLSTATE 也意味着提交没有发生
//     （PostgreSQL 的 COMMIT 失败即回滚，没有「提交了一半」）。
//     **真正说不清的「提交失败」不在此列**：COMMIT 发出去之后连接断了、回包丢了 ——
//     那时拿到的是网络错误，没有 SQLSTATE，IsDBBusy 认不出它，照旧走 500。
//  2. 同一个请求里**更早**的那些步骤：不一定。下单（POST /orders）先在一个事务里落草稿、
//     再提交 SAGA、最后在另一个事务里读回订单 —— 最后那一步撞上超时的话，订单其实已经建好了。
//     内网写库存（拆分形态下库存在另一个进程、另一个库）也一样：那边的事务提交了，
//     core 这边后一步才超时。
//
// 第 2 层就是这个包管的事：每个请求进来时挂一个记录器（Track），请求里凡是**确定落了地**
// 的东西都记一笔（MarkDurable）—— 本进程连接池上一个带写的事务提交成功、
// 一条自动提交的写语句成功（由挂在池上的 Tracer 自动记）、一次内网写调用发了出去
// 而结果不是「确定失败」（rpc 客户端记）。兜底处（problem.Write）只在「出错的是 57014 / 55P03，
// 且这个请求此前什么都没落地」时才说「这次没有生效」；否则维持原来的 500，不替它下结论。
//
// 没覆盖到的副作用（发出去的短信、对外的 HTTP 回调、上传到磁盘的文件）不在记录里。
// 这些路径要么在写库之后才发生，要么重试时重做一遍无害（短信多一条验证码、
// 磁盘上多一个没人引用的文件），所以没有为它们再开口子；新增「先对外动作、再写库」
// 的写路径时，在对外动作之后调一次 MarkDurable。
package outcome

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 两个 SQLSTATE。见包注释第 1 层：两者都是服务端在**提交之前**取消了语句。
const (
	// SQLStateQueryCanceled 是 statement_timeout 到点（也是被 pg_cancel_backend 取消）。
	SQLStateQueryCanceled = "57014"
	// SQLStateLockNotAvailable 是等锁超过 lock_timeout，或 NOWAIT 拿不到锁。
	SQLStateLockNotAvailable = "55P03"
)

// ErrPeerBusy 是**另一个进程**替我们判出的 busy：拆分形态下库存进程的数据库撞了 57014 / 55P03，
// 它回 503 busy，rpc 客户端把那次调用的错误挂到这个哨兵上（rpc.ErrBusy 包着它）。
// 对面的那个事务同样确定回滚了，所以它与本进程的 57014 / 55P03 同等对待。
var ErrPeerBusy = errors.New("对面的数据库繁忙，这次调用没有生效")

// IsDBBusy 回答 err 是不是「数据库此刻忙，语句被取消了」那一类（57014 / 55P03，
// 或对面进程回来的 ErrPeerBusy）。
//
// 本进程的只认服务端回来的 *pgconn.PgError：连接断了、ctx 到期（没等到服务端回话）都不是它 ——
// 那些情况下服务端那一侧发生了什么是不知道的。
func IsDBBusy(err error) bool {
	if errors.Is(err, ErrPeerBusy) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == SQLStateQueryCanceled || pgErr.Code == SQLStateLockNotAvailable
}

type ctxKey struct{}

// tracker 是一个请求的记录。并发安全：一个请求里偶尔会有几个 goroutine 各开各的事务。
type tracker struct {
	durable atomic.Bool

	mu sync.Mutex
	// dirty 记「这条连接上当前开着的事务里有过写」。提交成功时据此决定要不要记落地：
	// 只读事务提交了什么也没留下，不能因为它就放弃说「没生效」。
	dirty map[*pgx.Conn]bool
}

// Track 给 ctx 挂一个记录器。已经挂过的原样返回（中间件挂两层不会把前一层的记录丢掉）。
func Track(ctx context.Context) context.Context {
	if _, ok := ctx.Value(ctxKey{}).(*tracker); ok {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, &tracker{dirty: map[*pgx.Conn]bool{}})
}

func from(ctx context.Context) *tracker {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(ctxKey{}).(*tracker)
	return t
}

// Untracked 返回一个**看不见**记录器的 ctx：在它上面落的地不记进这个请求。
//
// 给「每个请求都会顺手写一笔、而那一笔不是业务效果」的地方用 —— 典型是鉴权中间件里
// 刷新后台会话的 last_seen_at（service.StaffService.LoadStaffSession）、AI 员工密钥的
// last_used_at。不剔掉它们的话，每个后台请求在进 handler 之前就已经「落过地」了，
// 兜底永远说不出「这次没有生效」。剔掉是安全的：重试时它们会被再写一遍，结果相同。
func Untracked(ctx context.Context) context.Context {
	if from(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, (*tracker)(nil))
}

// MarkDurable 记下「这个请求已经确定（或可能）留下了东西」。ctx 上没有记录器时什么也不做。
func MarkDurable(ctx context.Context) {
	if t := from(ctx); t != nil {
		t.durable.Store(true)
	}
}

// MaybeDurable 回答「这个请求是不是可能已经留下了东西」。
//
// **没有记录器时回 true**：不知道就当作可能有 —— 这个函数的唯一用途是决定能不能说「没生效」，
// 说错的代价（客户端以为没退款、又让人去点一次）远大于少说一次的代价（多一个 500）。
func MaybeDurable(ctx context.Context) bool {
	t := from(ctx)
	return t == nil || t.durable.Load()
}

// Tracer 挂在连接池上（db.NewPoolFromDSN），按语句的结果自动记落地。
//
// 判据只看两样东西：语句的 CommandTag（是不是写）与语句结束后连接的事务状态
// （'I' 空闲 = 刚才那条是自动提交的、或者它就是 COMMIT；'T' / 'E' 在事务里）。
//
//	自动提交的写成功            → 落地
//	事务里的写成功              → 这条连接记一个 dirty，还没落地
//	COMMIT 成功且这条连接 dirty → 落地
//	ROLLBACK / 出错后回到空闲   → 清掉 dirty
//
// 请求之外的连接用法（后台任务、建池时的 Guard）ctx 上没有记录器，整段直接跳过。
type Tracer struct{}

var (
	_ pgx.QueryTracer    = Tracer{}
	_ pgx.BatchTracer    = Tracer{}
	_ pgx.CopyFromTracer = Tracer{}
)

// isWrite：INSERT / UPDATE / DELETE / MERGE / COPY。SELECT 不算 —— 本仓库没有在 SELECT 里
// 调写库函数的查询（迁移里的函数只有读的与触发器），SELECT ... FOR UPDATE 拿的锁随事务结束消失。
func isWrite(tag pgconn.CommandTag) bool {
	if tag.Insert() || tag.Update() || tag.Delete() {
		return true
	}
	s := tag.String()
	return strings.HasPrefix(s, "MERGE") || strings.HasPrefix(s, "COPY")
}

func (t *tracker) end(conn *pgx.Conn, tag pgconn.CommandTag, err error, write bool) {
	idle := conn.PgConn().TxStatus() == 'I'
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		if idle {
			delete(t.dirty, conn)
		}
		return
	}
	if !idle {
		if write {
			t.dirty[conn] = true
		}
		return
	}
	if write || (tag.String() == "COMMIT" && t.dirty[conn]) {
		t.durable.Store(true)
	}
	delete(t.dirty, conn)
}

// start：语句开始时连接是空闲的，说明上一个事务早就结束了 —— 留着的 dirty 只可能是
// 一条事务中途断掉的连接留下的残迹，清掉，免得同一请求再拿到这条连接时误判。
func (t *tracker) start(conn *pgx.Conn) {
	if conn.PgConn().TxStatus() != 'I' {
		return
	}
	t.mu.Lock()
	delete(t.dirty, conn)
	t.mu.Unlock()
}

func (Tracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if t := from(ctx); t != nil {
		t.start(conn)
	}
	return ctx
}

func (Tracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if t := from(ctx); t != nil {
		t.end(conn, data.CommandTag, data.Err, isWrite(data.CommandTag))
	}
}

func (Tracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	if t := from(ctx); t != nil {
		t.start(conn)
	}
	return ctx
}

// TraceBatchQuery：批里每一条写都先记 dirty（批在管道里跑，中途的事务状态不可靠），
// 批结束时连接若已空闲（没有包在显式事务里，批自己就是那个隐式事务）再一起结算。
func (Tracer) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	t := from(ctx)
	if t == nil || data.Err != nil || !isWrite(data.CommandTag) {
		return
	}
	t.mu.Lock()
	t.dirty[conn] = true
	t.mu.Unlock()
}

func (Tracer) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	if t := from(ctx); t != nil {
		t.mu.Lock()
		dirty := t.dirty[conn]
		t.mu.Unlock()
		t.end(conn, pgconn.CommandTag{}, data.Err, dirty)
	}
}

func (Tracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	if t := from(ctx); t != nil {
		t.start(conn)
	}
	return ctx
}

func (Tracer) TraceCopyFromEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromEndData) {
	if t := from(ctx); t != nil {
		t.end(conn, data.CommandTag, data.Err, true)
	}
}
