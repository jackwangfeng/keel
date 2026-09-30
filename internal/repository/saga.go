package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 子事务屏障（架构 §5、数据模型 §6）。算法搬自 examples/dtmrs-embedded/barrier，
// **不是它的副本**。原样搬过来的是算法本身，改了形状的有五处，逐条记在下面。
//
// # 为什么屏障必须住在这一层
//
// 屏障成立的**全部**前提是：屏障记录与业务变更在同一个本地事务里提交。而
// WithTenant 自己 Begin/Commit，交给业务层的是 Tx 接口——没有任何口子把
// pgx.Tx 交出去（理由见 product.go 里 Tx 的注释：db.New(pool) 在本包之外是
// 编译错误）。
//
// 于是只有两条路：把 pgx.Tx 导出去让分支自己开事务，或者把屏障做进这一层。
// 前者是最省事的，也会把 M1 用编译器堵住的那个缺口原样打开——那条路上没有
// set_config('app.merchant_id')，症状是线上偶发 42501，比越权更难定位到根因。
// 所以是后者：WithSagaBranch 开事务、设租户、跑屏障判定，再按判定决定跑不跑
// 业务。业务分支收到的仍然是 Tx，pgx.Tx 一次都没有离开本包。
//
// # 搬过来时改的五处
//
//	① 去掉 Barrier 结构体与它的 counter，一次调用 = 一次判定，barrier_id 恒为 "01"。
//	  counter 是**实例状态**：dtmrs 那边一个 BranchBarrier 可以在同一次分支调用里
//	  decide 多次，第二次拿 "02"。我们这条路上不存在那个用法，留着它反而是个陷阱——
//	  一个被复用的屏障实例第二次会插出一行 barrier_id="02" 的新记录，于是同一个
//	  分支的重试**永远插得进去**，重复判定当场失效，业务被执行两遍。
//	  常量没有这个状态可错。
//
//	② 去掉 Table 字段与 fmt.Sprintf 拼表名。表名由 00008_barrier.sql 固定，
//	  可配置带来的唯一新东西是一个字符串拼接进 SQL 的形状。
//
//	③ op 从「map 查不到就当正向」改成白名单。Rust 侧 op 是 enum（BranchOp::parse
//	  不认识就报错），过了 C ABI 之后只剩一个字符串——那个保证没有跟着过来。
//	  一个拼错的 "compensat" 在原实现里会被当成**正向分支**：空回滚保护失效，
//	  后到的补偿变成一次真实的回补，悬挂的正向也照样执行。这里把 enum 的保证补回来。
//
//	④ 不导出 Decide，导出的是 WithSagaBranch。一个「请你自己开事务再调我」的
//	  Decide 等于把屏障成立的前提交给调用方去遵守，而在本包之外它连事务都开不出来。
//
//	⑤ Decision 的零值改成「没有判定」。原来 Execute = iota = 0，于是出错时返回的
//	  (0, err) 读起来是「判定为 Execute」——调用方少看一眼 err 就会去执行业务。
//
// 原样搬的（改之前请先读 dtmrs-barrier/src/lib.rs，两边对同一张表操作）：
// 两次 INSERT 的顺序、`ON CONFLICT DO NOTHING` 的形状、补偿类操作到正向操作的
// 映射、reason 记的是**本分支自己的** op、create_time 是 Unix 秒。

// Decision 是屏障给出的判定，与 dtmrs 的 Decision 一一对应。
//
// **三种判定里只有一种要执行业务，但三种都是正常路径。** 后两种返回失败会让
// 协调器误判并无限重试——重试的是一件本来就不该做的事。
type Decision int

const (
	// decisionNone 是零值：没有任何判定发生（出错时返回的就是它）。
	// 刻意让零值不是一个合法判定，理由见上面第 ⑤ 条。
	decisionNone Decision = iota

	// DecisionExecute：该干活。业务 SQL 在**同一个事务**里执行。
	DecisionExecute

	// DecisionNullCompensation：空回滚——正向分支从没执行过，补偿直接空转。
	// 什么都不做，**返回成功**。
	DecisionNullCompensation

	// DecisionDuplicated：重复请求，或者悬挂（补偿先到，把这行位置占了）。
	// 什么都不做，**返回成功**。
	DecisionDuplicated
)

func (d Decision) String() string {
	switch d {
	case DecisionExecute:
		return "Execute"
	case DecisionNullCompensation:
		return "NullCompensation"
	case DecisionDuplicated:
		return "Duplicated"
	case decisionNone:
		return "None"
	default:
		// 不用数组下标（原实现是 [...]string{...}[d]）：越界会 panic，
		// 而这是个只在打日志时被调用的方法。
		return fmt.Sprintf("Decision(%d)", int(d))
	}
}

var (
	// ErrUnknownBranchOp：op 不在 dtmrs 的 BranchOp 里。见上面第 ③ 条。
	ErrUnknownBranchOp = errors.New("未知的分支 op")

	// ErrBarrierDenied：屏障 INSERT 被数据库以权限拒绝（42501）。
	//
	// 它几乎只有一个成因，所以值得一个自己的 sentinel：**屏障的 SQL 带上了
	// 冲突目标**。keel_app 在 barrier 上只有 INSERT（00008_barrier.sql 的
	// GRANT 面），而 `ON CONFLICT (gid, ...) DO NOTHING` 要额外的 SELECT 权，
	// 不带冲突目标的 `ON CONFLICT DO NOTHING` 不要。
	//
	// 这条权限面因此同时是一道形状约束：写错了会当场被拒，而不是让一个错误的
	// 幂等语义悄悄生效。有了这个 sentinel，测试能把「屏障 SQL 被改坏了」和
	// 「附近别的东西坏了」分开。
	ErrBarrierDenied = errors.New("屏障 INSERT 被拒绝")
)

// barrierTransType 写进 barrier.trans_type。
//
// 分支屏障只有 SAGA 一种事务类型（架构 §7：TCC 要到预售 / 多仓调拨才引入）；二阶段消息的
// 回查屏障另走 msg_barrier.go，写的是 "msg"。它不在主键里，只是一行记录的自述。
const barrierTransType = "saga"

// barrierID 是每次判定的 barrier_id。恒为 "01"，理由见文件头第 ① 条。
const barrierID = "01"

// originOp 对应 dtmrs-core 的 BranchOp::origin_op：补偿类操作各自对应一个正向
// 操作，正向操作没有。map 里没有的 op 是正向操作——**前提是它先过了 knownOps**。
var originOp = map[string]string{
	"cancel":     "try",
	"compensate": "action",
	"rollback":   "action",
}

// knownOps 是 dtmrs-core BranchOp 的七个取值（v0.11.0）。
//
// 这个白名单是第 ③ 条那个保证的载体：不认识的 op 必须报错，**不能**沿用
// 「map 里查不到就是正向」那条默认路径。两者的差别是一个拼写错误到底会不会
// 把空回滚保护整个关掉。
var knownOps = map[string]bool{
	"action": true, "compensate": true,
	"try": true, "confirm": true, "cancel": true,
	"commit": true, "rollback": true,
}

// 拆分前这里还有一个 SagaTx 接口（DeductInventory / RestoreInventory），嵌在 core 的 Tx 里。
// 微服务拆分阶段 1b 起扣减与回补归库存服务：屏障照旧由这一个文件实现，但有两个入口 ——
// core 的分支走 Repo.WithSagaBranch（业务池，屏障在 core 库），库存的分支走
// InventoryStore.WithSagaBranch（库存池，屏障在库存库，inventory_svc.go）。两个入口共用
// 下面同一个 decideBarrier：屏障记录必须与业务变更同事务提交，所以它住在业务变更所在的那个库里。

// WithSagaBranch 在一个「设好租户 + 过了屏障」的事务里执行一个 SAGA 分支。
//
//	r.pool.Begin → set_config('app.merchant_id') → 屏障判定（同一个 tx）
//	  → 判定是 Execute 才调 fn → Commit
//
// 屏障记录与 fn 写的每一行同生共死：fn 返回错误时整个事务回滚，**屏障那行也
// 跟着没有**。这一条是重试能不能继续的前提——屏障若先落地，协调器的下一次重试
// 会被判成 Duplicated 而直接跳过业务，正向阶段扣的库存再没人回补，架构 §5 的
// 「少卖」从可恢复变成永久漏账。
//
// 租户从 ctx 取，与 WithTenant 同一个规矩：调用方没有那个参数可以传错。分支手里
// 只有 (gid, branchID, op) 三个字符串，租户由 dtm.TenantContextFromGID 从 gid 解出来
// 放进 ctx —— gid 是协调器重放时唯一还在的东西（见 internal/dtm/gid.go）。
//
// **本包刻意不解析 gid**：那样 repository 就得认得 gid 的文法，而产生这个 ctx 的
// 地方只有一处，租户与 gid 在那里按构造就是一致的。多一份解析等于多一份会漂移的
// 真相。
//
// 返回的 Decision 只为可观察（日志、测试）。三种判定都是正常路径，调用方不必
// 按它分支——**要是按它返回失败，协调器会去补偿一件从没发生过的事**。
func (r *Repo) WithSagaBranch(ctx context.Context, gid, branchID, op string, fn func(Tx) error) (Decision, error) {
	if gid == "" || branchID == "" {
		return decisionNone, fmt.Errorf("屏障需要非空的 gid 与 branch_id，实得 gid=%q branch_id=%q",
			gid, branchID)
	}
	if !knownOps[op] {
		// 在开事务之前拒绝。往下走的代价见文件头第 ③ 条。
		return decisionNone, fmt.Errorf("%w: %q（dtmrs 的 BranchOp 只有 action/compensate/try/confirm/cancel/commit/rollback）",
			ErrUnknownBranchOp, op)
	}
	if fn == nil {
		return decisionNone, errors.New("屏障分支的业务函数为空")
	}

	decision := decisionNone
	err := r.withTenantTx(ctx, func(tx pgx.Tx, q Tx) error {
		d, err := decideBarrier(ctx, tx, gid, branchID, op)
		if err != nil {
			return err
		}
		decision = d
		if d != DecisionExecute {
			// 空回滚与重复都是正常路径：什么都不做，让事务正常提交。
			// 提交的是屏障那一行——它记的正是「这个位置已经被占了」。
			return nil
		}
		return fn(q)
	})
	if err != nil {
		// 失败时不报判定：事务已经回滚，屏障那行不存在，这次调用等于没发生过。
		return decisionNone, err
	}
	return decision, nil
}

// decideBarrier 是屏障算法本身，必须在调用方的事务里执行。
//
// 两步，逐条对应 dtmrs-barrier 的 decide()：
//
//	① 补偿类操作先「假装自己是正向分支」插一行。插进去了说明正向分支从来没来过
//	  → 空回滚。
//	② 以自己的身份插一行。插不进去说明这次调用已经被处理过 → 重复（或悬挂：
//	  补偿在 ① 里把正向的位置占了，迟到的正向就插不进去）。
func decideBarrier(ctx context.Context, tx pgx.Tx, gid, branchID, op string) (Decision, error) {
	var originAffected int64
	origin, isCompensating := originOp[op]
	if isCompensating {
		n, err := insertBarrier(ctx, tx, barrierTransType, gid, branchID, origin, op)
		if err != nil {
			return decisionNone, err
		}
		originAffected = n
	}

	currentAffected, err := insertBarrier(ctx, tx, barrierTransType, gid, branchID, op, op)
	if err != nil {
		return decisionNone, err
	}

	if isCompensating && originAffected > 0 {
		return DecisionNullCompensation, nil
	}
	if currentAffected == 0 {
		return DecisionDuplicated, nil
	}
	return DecisionExecute, nil
}

// barrierInsert 是 keel_app 在 barrier 上发的**唯一**一条语句。
//
// `ON CONFLICT` 后面不许写冲突目标。这不是风格：keel_app 在这张表上只有 INSERT
// （00008_barrier.sql），带冲突目标要额外的 SELECT 权，写上去会当场 42501。
// 实测两个返回值正是算法要的：首次 INSERT 0 1，重复 INSERT 0 0。
//
// 表名写死在字面量里，不经 fmt.Sprintf——理由见文件头第 ② 条。
const barrierInsert = `INSERT INTO barrier
    (trans_type, gid, branch_id, op, barrier_id, reason, create_time)
    VALUES ($1,$2,$3,$4,$5,$6,$7)
    ON CONFLICT DO NOTHING`

// insertBarrier 插一行屏障记录，返回受影响行数（0 = 这个位置已经被占了）。
//
// op 与 reason 是两个不同的东西，不要合并：op 是**这一行占的位置**（① 里插的是
// 正向操作的位置），reason 是**谁插的这行**。空回滚留下的那一行 op='action'
// reason='compensate'，一眼能看出它是补偿抢先占的位，而不是正向真的执行过。
//
// transType 只是一行记录的自述（不在主键里）：SAGA 分支写 "saga"，二阶段消息的屏障写 "msg"（msg_barrier.go）。
func insertBarrier(ctx context.Context, tx pgx.Tx, transType, gid, branchID, op, reason string) (int64, error) {
	ct, err := tx.Exec(ctx, barrierInsert,
		transType, gid, branchID, op, barrierID, reason, time.Now().Unix())
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			return 0, fmt.Errorf("%w: %v —— keel_app 在 barrier 上只有 INSERT，"+
				"而带冲突目标的 ON CONFLICT 还要 SELECT 权。屏障的 SQL 是不是被加上冲突列了？",
				ErrBarrierDenied, pgErr.Message)
		}
		return 0, err
	}
	return ct.RowsAffected(), nil
}
