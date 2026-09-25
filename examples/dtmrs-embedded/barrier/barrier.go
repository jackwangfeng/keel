// Package barrier 实现 dtmrs 的子事务屏障。
//
// **为什么不用 dtmrs 自带的**：它的 decide() 签名是
// decide(&mut self, tx: &mut Transaction) —— 接收调用方的事务。
// 屏障记录必须与业务变更在同一个本地事务里提交，这是屏障成立的前提。
// Go 侧事务握在 pgx 手里，没法递过 C 边界。所以这不是 dtmrs 的缺口，
// 是结构上的必然：任何非 Rust 宿主都得自己实现这三十行。
//
// 算法与 dtmrs-barrier/src/lib.rs 逐行对应，改动前请先读那边。
package barrier

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Decision 与 dtmrs 的 Decision 一一对应。
type Decision int

const (
	Execute Decision = iota
	NullCompensation
	Duplicated
)

func (d Decision) String() string {
	return [...]string{"Execute", "NullCompensation", "Duplicated"}[d]
}

// originOp 对应 dtmrs-core 的 BranchOp::origin_op：
// 补偿类操作各自对应一个正向操作，正向操作没有。
var originOp = map[string]string{
	"cancel":     "try",
	"compensate": "action",
	"rollback":   "action",
}

// Barrier 是 dtmrs BranchBarrier 的 Go 侧实现。
//
// 用法见 cmd/barrier。三种异常（重复 / 空回滚 / 悬挂）均已实测。
//
// 为什么必须自己实现：dtmrs 的 decide() 签名是 decide(&mut self, tx)，
// 它**接收调用方的事务**——屏障记录必须与业务变更在同一个本地事务里提交，
// 这就是屏障成立的前提。Go 这边事务握在 pgx 手里，没有办法把它递过 C 边界。
// 所以这不是 dtmrs 的缺口，是结构上的必然。
type Barrier struct {
	TransType string
	Gid       string
	BranchID  string
	Op        string
	Table     string
	counter   int
}

func (b *Barrier) nextBarrierID() string {
	b.counter++
	return fmt.Sprintf("%02d", b.counter)
}

func (b *Barrier) insert(ctx context.Context, tx pgx.Tx, op, bid string) (int64, error) {
	table := b.Table
	if table == "" {
		table = "barrier"
	}
	// 关键：冲突时 rows_affected 必须是 0，整个算法就靠这个返回值判定。
	sql := fmt.Sprintf(`INSERT INTO %s
        (trans_type, gid, branch_id, op, barrier_id, reason, create_time)
        VALUES ($1,$2,$3,$4,$5,$6,$7)
        ON CONFLICT DO NOTHING`, table)
	ct, err := tx.Exec(ctx, sql, b.TransType, b.Gid, b.BranchID, op, bid,
		b.Op, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// Decide 必须在调用方的事务里执行，业务 SQL 与它同事务提交。
func (b *Barrier) Decide(ctx context.Context, tx pgx.Tx) (Decision, error) {
	bid := b.nextBarrierID()

	// 第一步：补偿类操作先"假装自己是正向分支"插一行。
	// 插进去了说明正向分支从来没来过 → 空回滚。
	var originAffected int64
	origin, isCompensating := originOp[b.Op]
	if isCompensating {
		n, err := b.insert(ctx, tx, origin, bid)
		if err != nil {
			return 0, err
		}
		originAffected = n
	}

	// 第二步：以自己的身份插一行。插不进去说明这次调用已经被处理过。
	currentAffected, err := b.insert(ctx, tx, b.Op, bid)
	if err != nil {
		return 0, err
	}

	if isCompensating && originAffected > 0 {
		return NullCompensation, nil
	}
	if currentAffected == 0 {
		return Duplicated, nil
	}
	return Execute, nil
}
