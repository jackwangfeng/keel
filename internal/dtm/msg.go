package dtm

/*
#include <stdlib.h>
#include "dtmrs.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"
)

// 二阶段消息（dtmrs 的 msg）。tc.go 文件头第 ① 条说过「只搬 SAGA」；这里是第二次搬，
// 理由是有了第一个真实的用户：派生数据同步（docs/电商系统-总体架构.md「派生数据同步约定」）——
// 库存跨 0 通知 core 重算有货标记、活动写入后通知库存服务同步配额。
//
// C ABI 给的形状（dtmrs.h「TCC / XA / 二阶段消息」）：
//
//	PrepareMsg(gid, 目标地址, 回查地址) → 调用方提交自己的本地事务
//	  → 成功 SubmitMsg / 明确失败 AbortMsg / 不知道就什么都别调（交给回查）
//
// 两条 ABI 自带的限制决定了上层怎么用，写在这里免得每个调用方各自再撞一次：
//
//   - **消息不带载荷。** actions_json 只是地址数组（dtmrs-server 的 msg_rows 把 payload 写成空串），
//     目标分支拿到的只有 gid。所以「通知什么」必须编进 gid —— 与 gid.go 那条「一切从 gid 推」
//     是同一个处境，也就只能带定位用的键、不带新值（接收方本来就不该信新值）。
//   - **回查地址必填。** 进程崩在「本地事务提交」与「submit」之间时，协调器在 grace 秒后调它问
//     「本地提交了没有」，分支号固定是 "00"。回查的实现是本地表屏障：本地事务里插一行屏障，
//     回查时试着插一行 rollback 标记 —— 见 repository 的 InsertMsgBarrier。

// PrepareMsg 登记一个二阶段消息。actions 是要送达的分支地址（local:// 或 http(s)://），
// queryPrepared 是回查地址，graceSecs 是 prepare 之后多久开始回查（负数用 dtmrs 的默认 10 秒）。
//
// **返回成功之后才能提交本地事务。** 反过来的话本地事务提交了、prepare 却失败，
// 那条变化就永远没有通知 —— 正是二阶段消息要消灭的那种丢失。
func (t *TC) PrepareMsg(gid string, actions []string, queryPrepared string, graceSecs int) error {
	if len(actions) == 0 {
		return errors.New("二阶段消息至少要有一个目标地址")
	}
	if queryPrepared == "" {
		return errors.New("二阶段消息必须给回查地址（dtmrs 在崩溃之后靠它决断）")
	}
	js, err := json.Marshal(actions)
	if err != nil {
		return err
	}
	t.acquireMsg()
	defer t.releaseMsg()

	g, a, q := cstr(gid), cstr(string(js)), cstr(queryPrepared)
	defer C.free(unsafe.Pointer(g))
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(q))
	if C.dtmrs_msg_prepare(t.p, g, a, q, C.int(graceSecs)) != C.DTMRS_OK {
		return fmt.Errorf("登记消息 %s 失败: %s", gid, lastError())
	}
	return nil
}

// SubmitMsg 是本地事务提交之后的那一步：交给协调器去投递。幂等。
//
// 它失败（协调器的存储一时不可用）不要紧：消息停在 prepared，grace 秒后回查会看到本地屏障那一行，
// 照样投递。所以调用方只记日志，不回错 —— 本地事务已经提交了，回错只会让调用方以为没改成。
func (t *TC) SubmitMsg(gid string) error {
	t.acquireMsg()
	defer t.releaseMsg()
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	if C.dtmrs_submit(t.p, g) != C.DTMRS_OK {
		return fmt.Errorf("提交消息 %s 失败: %s", gid, lastError())
	}
	return nil
}

// AbortMsg 在本地事务明确回滚之后作废消息。尽力而为：失败了回查也会得出同一个结论
// （本地屏障那一行不存在 → 回查插下 rollback 标记 → 作废）。
func (t *TC) AbortMsg(gid string) error {
	t.acquireMsg()
	defer t.releaseMsg()
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	if C.dtmrs_abort(t.p, g) != C.DTMRS_OK {
		return fmt.Errorf("作废消息 %s 失败: %s", gid, lastError())
	}
	return nil
}

// Status 查一个全局事务的状态（prepared / submitted / aborting / succeed / failed）。
// 只为可观察（测试、排障），业务不该按它分支。
func (t *TC) Status(gid string) (string, error) {
	t.acquireMsg()
	defer t.releaseMsg()
	g := cstr(gid)
	defer C.free(unsafe.Pointer(g))
	buf := make([]C.char, 64)
	if C.dtmrs_status(t.p, g, &buf[0], 64) != C.DTMRS_OK {
		return "", errors.New(lastError())
	}
	return C.GoString(&buf[0]), nil
}
