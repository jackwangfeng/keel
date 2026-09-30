package repository

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 事务里又去池上拿第二条连接的机械防线。
//
// # 这是什么问题
//
// 一个请求在 WithTenant（或任何开事务的入口）里攥着一条连接，回调里又调了一个直接走
// r.pool 的 Repo 方法（或者嵌套开了第二个事务）—— 于是一个请求同时要两条连接。
// 池只有 max(4, CPU) 条：4 个这样的请求同时走到第二步，每个都拿着一条、等着另一条，
// 谁也不放，直到请求超时。退款审核里的沙箱渠道（读回调密钥、验签）就是这么踩的
// （TestConcurrentRefundAuditsDoNotExhaustATinyPool）。它在单测、低并发的环境里
// 完全看不出来，只在线上高峰时以「一批请求一起超时」的样子出现。
//
// # 怎么挡
//
// 开事务的入口在回调期间把「本 goroutine 正在 pool P 上开着事务」记下来；每一处直接用
// r.pool 的地方先经过 poolFor(ctx, 方法名)，发现本 goroutine 在同一个池上还开着事务：
//
//   - 测试构建（testing.Testing()）里直接 panic，信息带方法名 —— 写出这种代码的那次
//     提交，测试当场红；
//   - 生产里只记一条 ERROR（带方法名与堆栈），不影响请求：它今天能跑，只是在高并发下
//     会自锁，把它变成 500 不会让事情变好。
//
// 为什么按 goroutine 而不是按 ctx 记：Tx 回调的签名是 func(Tx) error，不收 ctx ——
// 回调里用的是外面闭包捕获的那个 ctx，开事务的入口没有机会往里放标记。而回调就跑在
// 开事务的那个 goroutine 上，所以「这个 goroutine 正开着事务」是准确的判据
// （回调里另起 goroutine 再去用池的情形抓不到，那种写法本身就少见）。
//
// 按池区分：拆分形态下库存池与业务池是两个池，在业务事务里读库存池不会让业务池自锁。
//
// # 逃生口
//
// 真有「事务里有意另开连接」的场景（比如刻意不想被事务回滚带走的审计写入），用
// OutsideTx(ctx) 包一下 ctx 再调，并在调用处写明理由。眼下仓库里没有这样的调用。

// OutsideTx 标记「这次对池的使用是有意在事务之外的」，跳过 poolFor 的检查。
// 调用处必须写明为什么需要第二条连接（以及并发下为什么不会自锁）。
func OutsideTx(ctx context.Context) context.Context {
	return context.WithValue(ctx, outsideTxKey{}, true)
}

type outsideTxKey struct{}

type txKey struct {
	pool *pgxpool.Pool
	gid  uint64
}

var (
	txMu    sync.Mutex
	txDepth = map[txKey]int{}
)

// enterTx 记下「本 goroutine 在 r.pool 上开着一个事务」，返回的函数撤销它。
// 开事务的入口在 Begin 成功之后、调回调之前调它，defer 撤销。
func (r *Repo) enterTx() func() {
	k := txKey{r.pool, goid()}
	txMu.Lock()
	txDepth[k]++
	txMu.Unlock()
	return func() {
		txMu.Lock()
		if txDepth[k]--; txDepth[k] <= 0 {
			delete(txDepth, k)
		}
		txMu.Unlock()
	}
}

// poolFor 是 Repo 里**每一处**直接用 r.pool 的地方的入口：检查之后原样返回 r.pool。
// method 只进报错信息，让人一眼知道是哪个方法被从事务里调到了。
func (r *Repo) poolFor(ctx context.Context, method string) *pgxpool.Pool {
	if ctx.Value(outsideTxKey{}) != nil {
		return r.pool
	}
	k := txKey{r.pool, goid()}
	txMu.Lock()
	inTx := txDepth[k] > 0
	txMu.Unlock()
	if inTx {
		reportSecondConn(ctx, method)
	}
	return r.pool
}

func reportSecondConn(ctx context.Context, method string) {
	msg := fmt.Sprintf("repository.%s 在一个已经开着事务的 goroutine 里又从同一个连接池拿连接："+
		"一个请求同时占两条连接，并发一上来池会自锁（见 repository/txguard.go）。"+
		"改用事务上的同名方法（Tx），或在开事务之前读好；确属有意时用 repository.OutsideTx 并写明理由", method)
	if testing.Testing() {
		panic(msg)
	}
	slog.ErrorContext(ctx, msg, "method", method, "stack", string(debug.Stack()))
}

// goid 取当前 goroutine 的 id（runtime.Stack 第一行 "goroutine 123 [...]"）。
// Go 刻意不暴露它；这里只拿它当 map 的键，不拿它做任何调度决定。
func goid() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i > 0 {
		b = b[:i]
	}
	n, _ := strconv.ParseUint(string(b), 10, 64)
	return n
}
