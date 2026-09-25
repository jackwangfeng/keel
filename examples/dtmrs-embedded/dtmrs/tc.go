// Package dtmrs 把 dtmrs 的 C ABI 包成 Go 可用的形状。
//
// 它存在的意义不是「又一个 binding」，而是证明一件事：**事务编排代码在
// 进程内与跨服务两种形态下完全一致**，迁移只改分支地址那一个字符串。
// 见 cmd/topology。
//
// 已知边界（实测，不是推断）：
//   - 引入 cgo 意味着失去 CGO_ENABLED=0 静态编译。
//   - 子事务屏障不在 C ABI 里，也不可能在：dtmrs 的 decide() 接收调用方的事务，
//     而屏障记录必须与业务变更同事务提交。Go 侧用自己的事务实现同一套算法，
//     见 barrier.go。
//
// dtmrs 0.11 起 C ABI 导出 22 个符号（0.8 只有 11 个），TCC / XA / 二阶段消息 /
// workflow 都可用了。本包目前封装 SAGA 与 TCC。
package dtmrs

/*
#cgo CFLAGS: -I${SRCDIR}/../include
#cgo LDFLAGS: -L${SRCDIR}/../lib -ldtmrs -Wl,-rpath,${SRCDIR}/../lib
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
	"unsafe"
)

// 分支返回值。**返回 Failure 会立刻触发全局补偿**，所以「我不知道业务做没做」
// 必须返回 Unknown 而不是 Failure —— 前者让协调器重试，后者让它回滚。
const (
	Success = int(C.DTMRS_SUCCESS)
	Failure = int(C.DTMRS_FAILURE)
	Unknown = int(C.DTMRS_UNKNOWN)
)

// BranchFunc 是业务分支的签名。它就是一个普通 Go 函数 —— 这正是嵌入式形态的价值：
// 单机跑的时候分支不必是 HTTP handler。
type BranchFunc func(gid, branchID, op string) int

// TC 是嵌入在本进程里的事务协调器。
type TC struct {
	p       *C.DtmrsTc
	boxes   []unsafe.Pointer // C 侧持有的 handle 盒子，close 时释放
	handles []cgo.Handle
}

func cstr(s string) *C.char { return C.CString(s) }

func lastError() string { return C.GoString(C.dtmrs_last_error()) }

// Open 打开协调器。dsn 形如 "sqlite:/tmp/x.db" 或 "postgres://..."。
func Open(dsn string) (*TC, error) {
	c := cstr(dsn)
	defer C.free(unsafe.Pointer(c))
	p := C.dtmrs_open(c)
	if p == nil {
		return nil, fmt.Errorf("dtmrs_open: %s", lastError())
	}
	return &TC{p: p}, nil
}

// Register 注册一个进程内分支，对应编排里的 "local://<name>"。
func (t *TC) Register(name string, fn BranchFunc) error {
	h := cgo.NewHandle(fn)
	// 把 handle 放进 C 分配的内存，而不是 unsafe.Pointer(uintptr(h))。
	// 后者在 -race 下必然 fatal error: checkptr —— 这个坑 100% 会在 CI 上命中。
	box := C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
	*(*C.uintptr_t)(box) = C.uintptr_t(h)
	t.boxes = append(t.boxes, box)
	t.handles = append(t.handles, h)

	c := cstr(name)
	defer C.free(unsafe.Pointer(c))
	if C.reg_go(t.p, c, box) != C.DTMRS_OK {
		return fmt.Errorf("register %q: %s", name, lastError())
	}
	return nil
}

// RegisterPull 注册一个拉取式分支：协调器把任务放进队列，由 Go 侧自己取。
//
// 这条路径绕开了 C→Go 回调，任务可以随便丢进 goroutine 异步处理，
// 也不会让 tokio 的 block_on 占住 OS 线程。
func (t *TC) RegisterPull(name string) error {
	c := cstr(name)
	defer C.free(unsafe.Pointer(c))
	if C.dtmrs_register_pull(t.p, c) != C.DTMRS_OK {
		return fmt.Errorf("register_pull %q: %s", name, lastError())
	}
	return nil
}

func (t *TC) Start() error {
	if C.dtmrs_start(t.p) != C.DTMRS_OK {
		return fmt.Errorf("start: %s", lastError())
	}
	return nil
}

// SubmitSaga 提交一个 SAGA 全局事务。steps 是 JSON 数组，每项
// {"action": "...", "compensate": "..."}，地址可以是 local:// 也可以是 http://。
//
// **这里没有 SubmitTCC —— C ABI 根本没导出它。**
func (t *TC) SubmitSaga(gid, stepsJSON string) error {
	g, s := cstr(gid), cstr(stepsJSON)
	defer C.free(unsafe.Pointer(g))
	defer C.free(unsafe.Pointer(s))
	if C.dtmrs_submit_saga(t.p, g, s) != C.DTMRS_OK {
		return fmt.Errorf("submit %s: %s", gid, lastError())
	}
	return nil
}

// WaitFinal 阻塞到该全局事务到达终态，返回状态字符串。
func (t *TC) WaitFinal(gid string, timeoutMS int) (string, error) {
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	buf := make([]C.char, 64)
	if C.dtmrs_wait_final(t.p, g, C.int(timeoutMS), &buf[0], 64) != C.DTMRS_OK {
		return "", errors.New(lastError())
	}
	return C.GoString(&buf[0]), nil
}

// NextTask 非阻塞地取一个拉取式任务，返回其 JSON 描述。没有任务时 ok 为 false。
func (t *TC) NextTask() (task string, ok bool) {
	buf := make([]C.char, 512)
	if C.dtmrs_next_task(t.p, 0, &buf[0], 512) == 1 {
		return C.GoString(&buf[0]), true
	}
	return "", false
}

// Reply 回填拉取式任务的结果。
func (t *TC) Reply(taskID uint64, result int) {
	C.dtmrs_reply(t.p, C.ulonglong(taskID), C.int(result))
}

// ---- TCC ----
//
// TCC 的一阶段是**调用方自己做**的，所以它是按 gid 的一串调用，不是一次提交：
//
//	TccBegin(gid)
//	  → 每个分支：TccRegister(gid, "01", confirm, cancel) 然后自己跑 Try
//	  → 全成功 Submit(gid)，任一失败或超时 Abort(gid)
//
// 两条纪律，来自 dtmrs 的头文件且都有代价：
//
//   - **先 register 再跑 Try**。反过来的话，Try 冻结的资源 TC 不知道，
//     回滚时没人收尾。
//   - **Submit 之后不能 Abort**。方向已定，confirm 失败只会无限重试，
//     绝不会转成 cancel。
func (t *TC) TccBegin(gid string) error {
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	if C.dtmrs_tcc_begin(t.p, g) != C.DTMRS_OK {
		return fmt.Errorf("tcc_begin %s: %s", gid, lastError())
	}
	return nil
}

// TccRegister 登记一个 TCC 分支。branch_id 由调用方给，从 "01" 起两位补零。
// 原样重试是幂等的；同一个号配不同地址会报错。
func (t *TC) TccRegister(gid, branchID, confirm, cancel string) error {
	g, b := cstr(gid), cstr(branchID)
	cf, cn := cstr(confirm), cstr(cancel)
	defer func() {
		C.free(unsafe.Pointer(g))
		C.free(unsafe.Pointer(b))
		C.free(unsafe.Pointer(cf))
		C.free(unsafe.Pointer(cn))
	}()
	if C.dtmrs_tcc_register(t.p, g, b, cf, cn) != C.DTMRS_OK {
		return fmt.Errorf("tcc_register %s/%s: %s", gid, branchID, lastError())
	}
	return nil
}

// Submit 提交已登记的分支，进入二阶段（confirm / commit）。
func (t *TC) Submit(gid string) error {
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	if C.dtmrs_submit(t.p, g) != C.DTMRS_OK {
		return fmt.Errorf("submit %s: %s", gid, lastError())
	}
	return nil
}

// Abort 逆序撤销所有已登记分支。已 Submit 的会返回错误。
func (t *TC) Abort(gid string) error {
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	if C.dtmrs_abort(t.p, g) != C.DTMRS_OK {
		return fmt.Errorf("abort %s: %s", gid, lastError())
	}
	return nil
}

func (t *TC) Close() {
	C.dtmrs_close(t.p)
	for _, b := range t.boxes {
		C.free(b)
	}
	for _, h := range t.handles {
		h.Delete()
	}
	t.boxes, t.handles = nil, nil
}
