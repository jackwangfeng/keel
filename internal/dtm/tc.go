// Package dtm 把 dtmrs 的嵌入式事务协调器接进本进程。
//
// 它是 examples/dtmrs-embedded/dtmrs 的产品化版本，**不是它的副本**。
// 搬过来时做了三处取舍，每一处都有理由：
//
//	① 只搬 SAGA，没搬 TCC 与拉取式分支。
//	  例子里那两套是「0.11 的 C ABI 够用」的证据（架构 ADR #1 那条悬了很久的
//	  前提），证据的归宿是例子与它的 workflow，不是 internal/。架构 §7 写明
//	  TCC 要到「预售 / 多仓调拨」才引入，而 internal/ 里一个没人调的导出 API
//	  没有任何闸门守着 —— 上游改了 C ABI，它会安静地烂掉，而例子那边会红。
//	  真要用的那天从例子里搬第二次，成本是十几行。
//
//	② 加了有界信号量。
//	  架构「ADR #1 补注」定下的那条工程规矩：**所有进入 dtmrs 的调用都要过一个
//	  有界信号量**。它是实测出来的（数据记在 examples 的 README 里）：每个阻塞的
//	  cgo 调用占一个 OS 线程，而超出 Go 的线程上限不是返回错误，是**进程直接死**
//	  （fatal error: thread exhaustion）。例子里没有实现它 —— 例子不需要，
//	  它自己控制并发度。
//	  服务需要：并发度是外面的人决定的。实测限流不花吞吐（2000 saga：不限并发
//	  1213 线程 246.8/s，限到 32 是 70 线程 243.7/s）。
//
//	③ Close 做成幂等的。
//	  Run 里既有 defer 也可能有显式收尾，两次 dtmrs_close 是 double free。
//	  例子是一次性进程，碰不到这件事。
//
// 已知边界（实测，不是推断）：
//   - 引入 cgo 意味着失去 CGO_ENABLED=0 静态编译，最终镜像也不能是 scratch。
//     见 docker/Dockerfile。
//   - 子事务屏障不在 C ABI 里，也不可能在：dtmrs 的 decide() 接收调用方的事务，
//     而屏障记录必须与业务变更同事务提交。Go 侧要自己实现同一套算法，
//     算法见 examples/dtmrs-embedded/barrier，产品化归 M2 任务 2。
//   - **协调器自己的三张表（trans_global / trans_branch_op / auth_token）由
//     dtmrs 在 Start() 里建**，需要建表权限。`barrier` 不在其中 —— 它由
//     dtmrs-barrier 建，而 C ABI 根本不依赖那个 crate。详见 Open 的注释。
package dtm

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/dtmrs/include
#cgo LDFLAGS: -L${SRCDIR}/../../third_party/dtmrs/lib -ldtmrs -Wl,-rpath,${SRCDIR}/../../third_party/dtmrs/lib
#include <stdlib.h>
#include <stdint.h>
#include "dtmrs.h"

// 声明 Go 侧导出的符号，以便取它的地址传给注册函数。
extern int goBranchHandler(const char*, const char*, const char*, void*);

static int reg_go(DtmrsTc *tc, const char *name, void *ud) {
    return dtmrs_register(tc, name, goBranchHandler, ud);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"
)

// 分支返回值。**返回 Failure 会立刻触发全局补偿**，所以「我不知道业务做没做」
// 必须返回 Unknown 而不是 Failure —— 前者让协调器重试，后者让它回滚。
const (
	Success = int(C.DTMRS_SUCCESS)
	Failure = int(C.DTMRS_FAILURE)
	Unknown = int(C.DTMRS_UNKNOWN)
)

// DefaultMaxInflight 是同时进入 dtmrs 的阻塞调用数上限。
//
// 32 不是随手挑的：examples 的 README 记了实测三组数据，提交并发 2000 / 256 / 32
// 的吞吐分别是 246.8 / 247.2 / 243.7 saga/s（几乎相同），而峰值 OS 线程是
// 1213 / 290 / 70。也就是说限流几乎不花钱，却省掉一千多个线程；而线程用尽时
// Go 不是返回错误，是 `fatal error: thread exhaustion`，进程直接死。
const DefaultMaxInflight = 32

// BranchFunc 是业务分支的签名。它就是一个普通 Go 函数 —— 这正是嵌入式形态的价值：
// 单机跑的时候分支不必是 HTTP handler。
//
// **它只拿到这三个字符串。** 分支跑在任何 HTTP 请求之外，崩溃重启后由协调器重放，
// 进程对原请求毫无记忆。所以分支需要的一切（包括租户）都必须能从 gid 推出来 ——
// 这就是 gid.go 存在的全部理由。
type BranchFunc func(gid, branchID, op string) int

// TC 是嵌入在本进程里的事务协调器。
type TC struct {
	p       *C.DtmrsTc
	boxes   []unsafe.Pointer // C 侧持有的 handle 盒子，close 时释放
	handles []cgo.Handle

	// sem 给每一个**阻塞的 cgo 调用**发一张通行证。见 DefaultMaxInflight。
	sem chan struct{}

	// 下面两个只为测试可观察：闸门守的是「真的有上限」，而不是「代码里有个
	// channel」。把 acquire 删掉时 peak 会冲过 cap，测试才红。
	inflight atomic.Int64
	peak     atomic.Int64

	closeOnce sync.Once
}

func cstr(s string) *C.char { return C.CString(s) }

func lastError() string { return C.GoString(C.dtmrs_last_error()) }

// acquire / release 是所有阻塞 cgo 调用的入口与出口。
//
// 刻意不区分「提交」与「等待」两类调用用两个信号量：要命的那件事是**总线程数**，
// 而它只认「同时卡在 cgo 里的调用数」这一个量。代价说清楚：一批长 WaitFinal
// 会占满额度，把 SubmitSaga 挡在外面。所以 WaitFinal 必须带一个有界的 timeout，
// 而不是「等到天荒地老」—— 那样的调用一旦有 32 个，协调器就再也收不到新事务。
func (t *TC) acquire() {
	t.sem <- struct{}{}
	n := t.inflight.Add(1)
	for {
		old := t.peak.Load()
		if n <= old || t.peak.CompareAndSwap(old, n) {
			break
		}
	}
}

func (t *TC) release() {
	t.inflight.Add(-1)
	<-t.sem
}

// Open 打开协调器。dsn 形如 "sqlite:/var/lib/keel/dtm.db" 或 "postgres://..."。
//
// **Open 不碰数据库**，Start 才碰 —— 这一点实测过，也决定了启动顺序：
// 连不上、没权限这两类错误都从 Start 出来。
//
// 关于 dsn 指向哪里，有一条实测出来的事实要写在这里，因为它不符合直觉：
//
//	Start() 会跑 dtmrs 自己的 migrate()，建出 trans_global / trans_branch_op /
//	auth_token 三张表。它需要 CREATE 权限，还会对已有表发 ALTER TABLE ADD COLUMN
//	（补列，靠吞掉「列已存在」来幂等）。**keel_app 做不到**：实测用 keel_app 的
//	DSN 调 Start，报的是 `permission denied for schema public`。
//
//	而 barrier **不在这三张表里**。它由 dtmrs-barrier 那个 crate 建，
//	而 dtmrs-ffi 根本不依赖它（Cargo.toml 里没有，符号表里也没有）。所以
//	「barrier 随 dtmrs 一起建出来」这句话是错的，它得由我们自己的迁移建。
//
// 推论：协调器的存储与业务库**不是同一个连接**，也不该是同一份凭据。
func Open(dsn string, maxInflight int) (*TC, error) {
	if maxInflight <= 0 {
		maxInflight = DefaultMaxInflight
	}
	c := cstr(dsn)
	defer C.free(unsafe.Pointer(c))
	p := C.dtmrs_open(c)
	if p == nil {
		return nil, fmt.Errorf("dtmrs_open: %s", lastError())
	}
	return &TC{p: p, sem: make(chan struct{}, maxInflight)}, nil
}

// Register 注册一个进程内分支，对应编排里的 "local://<name>"。必须在 Start 之前调。
func (t *TC) Register(name string, fn BranchFunc) error {
	h := cgo.NewHandle(fn)
	// 把 handle 放进 C 分配的内存，而不是 unsafe.Pointer(uintptr(h))。
	// 后者在 -race 下必然 fatal error: checkptr —— 这个坑 100% 会在 CI 上命中，
	// 而本机不开 race 跑起来一切正常。
	box := C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
	*(*C.uintptr_t)(box) = C.uintptr_t(h)
	t.boxes = append(t.boxes, box)
	t.handles = append(t.handles, h)

	c := cstr(name)
	defer C.free(unsafe.Pointer(c))
	if C.reg_go(t.p, c, box) != C.DTMRS_OK {
		return fmt.Errorf("注册分支 %q 失败: %s", name, lastError())
	}
	return nil
}

// Start 建表（见 Open 的注释）并启动推进器。
func (t *TC) Start() error {
	t.acquire()
	defer t.release()
	if C.dtmrs_start(t.p) != C.DTMRS_OK {
		return fmt.Errorf("启动协调器失败: %s", lastError())
	}
	return nil
}

// SubmitSaga 提交一个 SAGA 全局事务。steps 是 JSON 数组，每项
// {"action": "...", "compensate": "..."}，地址可以是 local:// 也可以是 http://。
//
// steps 里**只有地址，没有业务载荷** —— 分支要的一切从 gid 推。见 gid.go。
func (t *TC) SubmitSaga(gid, stepsJSON string) error {
	t.acquire()
	defer t.release()

	g, s := cstr(gid), cstr(stepsJSON)
	defer C.free(unsafe.Pointer(g))
	defer C.free(unsafe.Pointer(s))
	if C.dtmrs_submit_saga(t.p, g, s) != C.DTMRS_OK {
		return fmt.Errorf("提交 %s 失败: %s", gid, lastError())
	}
	return nil
}

// WaitFinal 阻塞到该全局事务到达终态，返回状态字符串。
//
// timeoutMS 必须是有界的：这是个阻塞 cgo 调用，它整段时间都占着一张信号量
// 通行证和一个 OS 线程。
func (t *TC) WaitFinal(gid string, timeoutMS int) (string, error) {
	t.acquire()
	defer t.release()

	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	buf := make([]C.char, 64)
	if C.dtmrs_wait_final(t.p, g, C.int(timeoutMS), &buf[0], 64) != C.DTMRS_OK {
		return "", errors.New(lastError())
	}
	return C.GoString(&buf[0]), nil
}

// Close 收尾。幂等：第二次调用什么都不做。
//
// 不幂等的话，`defer tc.Close()` 加上任何一条显式收尾路径就是一次 double free，
// 症状是进程在退出的最后一刻崩掉 —— 而那时日志已经打完了，看起来像「正常退出」。
func (t *TC) Close() {
	t.closeOnce.Do(func() {
		C.dtmrs_close(t.p)
		for _, b := range t.boxes {
			C.free(b)
		}
		for _, h := range t.handles {
			h.Delete()
		}
		t.boxes, t.handles = nil, nil
	})
}

// Start 是「建协调器 → 注册全部分支 → 启动」这一串的唯一入口。
//
// 合成一个函数而不是让调用方自己串，是因为顺序有硬约束且踩错不报错：
// dtmrs 要求**先 Register 再 Start**，反过来的话分支注册不上，而症状要等到
// 第一次提交时才出现（「未注册的 local:// 名字」在提交期被拒），
// 那时错误指向的是提交的那段业务代码。
//
// 出错时会把已经建出来的协调器关掉再返回 —— 否则一次启动失败会漏掉一个
// tokio 运行时和两个线程，而进程还要继续活着去报这个错。
func Start(dsn string, maxInflight int, branches map[string]BranchFunc) (*TC, error) {
	tc, err := Open(dsn, maxInflight)
	if err != nil {
		return nil, err
	}
	for name, fn := range branches {
		if err := tc.Register(name, fn); err != nil {
			tc.Close()
			return nil, err
		}
	}
	if err := tc.Start(); err != nil {
		tc.Close()
		return nil, err
	}
	return tc, nil
}
