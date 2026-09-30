package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// 二阶段消息的回查屏障（internal/dtm/msg.go）。
//
// dtmrs 的 msg 要求一个回查地址：进程崩在「本地事务提交」与「submit」之间时，协调器过 grace 秒
// 来问「本地事务提交了没有」。回答必须与本地事务**原子地**对得上，于是用的是 DTM 的「本地表屏障」：
//
//	本地事务里：插 (gid, "00", "msg", "01", reason="msg")        —— MarkMsgPrepared
//	回查时：    插同一个位置，reason="rollback"                    —— QueryPreparedMsg
//	            插进去了 → 本地事务没提交（而且从此再也提交不了这一行）→ 回 FAILURE 作废
//	            插不进去 → 位置已被占 → 回 SUCCESS 继续投递
//
// 「从此再也提交不了」是这套算法成立的那一半：回查与本地事务抢同一个主键，谁先提交谁赢，
// 不存在「回查说没提交、本地事务随后又提交了」的第三种结局 —— 输掉的本地事务在 MarkMsgPrepared
// 那里拿到 false，由调用方决定怎么办（见各调用方的注释）。
//
// 与 DTM 原版差的一处：keel_app 在 barrier 上**只有 INSERT**（00008_barrier.sql 的 GRANT 面，
// 那段注释讲了为什么不多给），读不回 reason。于是「插不进去」分不出是本地事务占的位，还是
// 上一次回查已经插下的 rollback 标记（上一次回查的答复丢了，协调器又问了一次）。后一种会被
// 答成 SUCCESS —— 一条本地事务其实没提交的消息被投递出去。
//
// 这对今天的两个用户是无害的，而且是刻意接受的：它们的接收方都**不信消息内容**，只把消息当作
// 「去查一下」（docs/电商系统-总体架构.md「派生数据同步约定」第 1 条），多投一次等于多重算一次。
// 将来谁要用二阶段消息做「收到即执行」的业务（扣款、发券），必须先给回查补上 SELECT，不能照搬这里。
//
// 分支号 "00"、op "msg" 与 dtmrs 回查调用的分支号一致（driver.rs 用 "00" 调 query_prepared），
// 与 SAGA 分支的屏障（"01" 起的分支号，op 是 action/compensate）不会互相占位。

const (
	msgBarrierTransType = "msg"
	msgBarrierBranchID  = "00"
	msgBarrierOp        = "msg"
)

// errEmptyMsgGID 挡住空 gid：空串会让所有消息共用一个屏障位置，第二条起全部被判成「回查赢了」。
var errEmptyMsgGID = errors.New("二阶段消息的屏障需要非空的 gid")

func insertMsgBarrier(ctx context.Context, tx pgx.Tx, gid, reason string) (bool, error) {
	if gid == "" {
		return false, errEmptyMsgGID
	}
	n, err := insertBarrier(ctx, tx, msgBarrierTransType, gid, msgBarrierBranchID, msgBarrierOp, reason)
	return n > 0, err
}

// MarkMsgPrepared 见 InventoryStoreTx。
func (t invTx) MarkMsgPrepared(ctx context.Context, gid string) (bool, error) {
	return insertMsgBarrier(ctx, t.tx, gid, "msg")
}

// QueryPreparedMsg 是库存服务那一侧的回查：本地事务提交了没有。ctx 必须带租户（从 gid 解出来）。
func (s *InventoryStore) QueryPreparedMsg(ctx context.Context, gid string) (committed bool, err error) {
	return queryPreparedMsg(ctx, s.r, gid)
}

// QueryPreparedMsg 是 core 那一侧的回查，同一个算法，屏障记在业务库。
func (r *Repo) QueryPreparedMsg(ctx context.Context, gid string) (committed bool, err error) {
	return queryPreparedMsg(ctx, r, gid)
}

func queryPreparedMsg(ctx context.Context, r *Repo, gid string) (bool, error) {
	var inserted bool
	err := r.withTenantTx(ctx, func(tx pgx.Tx, _ Tx) error {
		var e error
		inserted, e = insertMsgBarrier(ctx, tx, gid, "rollback")
		return e
	})
	if err != nil {
		return false, err
	}
	return !inserted, nil
}
