package dtmrs

/*
#include <stdint.h>
#include "dtmrs.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// goBranchHandler 是 C 侧回调的唯一入口。
//
// 注意这个文件里只有 //export 和声明，没有 C 定义 —— cgo 规定带 //export 的文件
// 其 preamble 不得包含定义，只能包含声明。C 侧的 trampoline 在 tc.go 里。
//
//export goBranchHandler
func goBranchHandler(gid, branchID, op *C.char, ud unsafe.Pointer) C.int {
	h := cgo.Handle(*(*C.uintptr_t)(ud))
	fn := h.Value().(BranchFunc)
	return C.int(fn(C.GoString(gid), C.GoString(branchID), C.GoString(op)))
}
