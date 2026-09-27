package dtm

/*
#include <stdint.h>
#include "dtmrs.h"
*/
import "C"

import (
	"log/slog"
	"runtime/cgo"
	"unsafe"
)

// goBranchHandler 是 C 侧回调的唯一入口。
//
// 注意这个文件里只有 //export 和声明，没有 C 定义 —— cgo 规定带 //export 的文件
// 其 preamble 不得包含定义，只能包含声明。C 侧的 trampoline 在 tc.go 里。
//
// **panic 在这里被拦下来，返回 Unknown。**
//
// 两件事叠在一起：Go 的 panic 不能穿过 C 栈（会直接搞死进程），而「业务做没做」
// 这个问题在 panic 之后是没有答案的 —— panic 可能发生在业务 SQL 提交之后。
// 返回 Failure 是在断言「肯定没做」，协调器会据此立刻触发全局补偿，
// 而那可能是在补偿一件从未发生的事，或者更糟：在一件已经发生的事上补两次。
// Unknown 让协调器重试，重试由子事务屏障挡住重复执行。
//
//export goBranchHandler
func goBranchHandler(gid, branchID, op *C.char, ud unsafe.Pointer) (ret C.int) {
	g, b, o := C.GoString(gid), C.GoString(branchID), C.GoString(op)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("事务分支 panic，按 Unknown 上报以便协调器重试",
				"gid", g, "branch_id", b, "op", o, "panic", r)
			ret = C.int(Unknown)
		}
	}()
	h := cgo.Handle(*(*C.uintptr_t)(ud))
	fn := h.Value().(BranchFunc)
	return C.int(fn(g, b, o))
}

// goBranchHandlerEx 是 dtmrs_register_ex 那条回调的入口，多一个 payload。
// panic 的处置与 goBranchHandler 相同，理由也相同。
//
//export goBranchHandlerEx
func goBranchHandlerEx(gid, branchID, op, payload *C.char, ud unsafe.Pointer) (ret C.int) {
	g, b, o := C.GoString(gid), C.GoString(branchID), C.GoString(op)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("事务分支 panic，按 Unknown 上报以便协调器重试",
				"gid", g, "branch_id", b, "op", o, "panic", r)
			ret = C.int(Unknown)
		}
	}()
	h := cgo.Handle(*(*C.uintptr_t)(ud))
	fn := h.Value().(BranchFuncEx)
	return C.int(fn(g, b, o, normalizePayload(C.GoString(payload))))
}
