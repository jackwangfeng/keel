// Package repository 是数据访问层，也是业务代码进入数据库的唯一入口。
//
// sqlc 产物在 internal/db 之下，Go 的 internal 规则让 internal/repository/ 之外的
// 包 import 不到它。于是 handler 与 service 只能经由 WithTenant 拿到 Tx，
// 而 WithTenant 保证每一次访问都发生在一个设过 app.merchant_id 的事务里。
//
// 这条约束必须靠编译器而不是靠自觉：sqlc 的 DBTX 接口同时被 *pgxpool.Pool 和
// pgx.Tx 满足，所以 db.New(pool).ListProducts(ctx, ...) 是能编译过的一句话，
// 而它不在事务里，也就没有 SET LOCAL —— 真实症状不是「越权读到别家数据」
// （RLS 会挡住），而是线上偶发 42501，比越权更难定位到根因。
package repository

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// Repo 持有连接池。池请用 db.NewPool 建：它把 RLS 自检挂在每条物理连接上。
type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// WithTenant 在一个设好租户上下文的事务里执行 fn。
//
// 租户从 ctx 取，不从参数传 —— 调用方没有那个参数可以传错。
//
// fn 收到的是 Tx（接口），不是 *db.Queries。理由见 product.go 里 Tx 的注释：
// 生成代码上的导出方法 WithTx(pgx.Tx) 会让「自己 Begin 一个没设租户的事务」
// 重新变成一句能编译的话，而接口让那个方法在业务层根本不存在。
func (r *Repo) WithTenant(ctx context.Context, fn func(Tx) error) error {
	return r.withTenantTx(ctx, func(_ pgx.Tx, q Tx) error { return fn(q) })
}

// pgvector 的三个会话级 GUC，与 app.merchant_id 设在同一个地方（M3 Task 4）。
//
// ===========================================================================
// 为什么它们在**这里**，而不是在检索那条查询旁边
// ===========================================================================
//
// 因为它们补的是**这一层自己制造的**那个问题。上面那句 set_config 打开了
// RLS 的租户谓词 `merchant_id = current_merchant()`，而那个谓词不是应用写在
// SQL 里的 WHERE —— 它由数据库注入到**每一条**查询上，包括将来任何一条还没
// 写出来的向量查询。补偿放在查询旁边，就意味着第二条向量查询的作者必须知道
// 要抄这三行；他不抄的话，症状是搜索安静地返回零条（见下面实测）。
// 放在这里，那个前提根本不必存在。
//
// ===========================================================================
// 实测：不设它们时，检索在真实规模下是**真的坏的**（M3 计划第一条的实验）
// ===========================================================================
//
// 库：40 家商家、30,340 条 1024 维向量，idx_ptv_hnsw（m=16 / ef_construction=64），
// pgvector 0.8.1 / PostgreSQL 16（pgvector/pgvector:pg16），默认服务器参数。
// 查询：一条随机单位向量，`ORDER BY embedding <=> $1 LIMIT 20`，
// 以 keel_app 的身份、带着某一家商家的 app.merchant_id 发出。
//
//	被查商家             向量数   占全库   iterative_scan=off   relaxed_order
//	-------------------  -------  -------  -------------------  -------------
//	exp-small              400     1.3%    0 / 1 / 0 条（3 次）  20 / 20 / 20
//	exp-1                  760     2.5%    3 条                  20
//
// off 那一列的 EXPLAIN ANALYZE 每次都是同一句话：
//
//	Index Scan using idx_ptv_hnsw ... (actual rows=0)
//	  Rows Removed by Filter: 40
//
// **40 就是 hnsw.ef_search 的默认值。** 索引扫描一共只吐 40 条候选，吐完就
// 结束；这 40 条里属于本商家的期望值是 40 × 1.3% ≈ 0.5 条。返回零条不是异常，
// 是这个机制的正常输出。
//
// 顺带验掉两条「看上去更简单」的对策，都是实测：
//
//   - **只调大 ef_search 不行。** 同一条查询，off + ef_search=100 → 0 条；
//     off + ef_search=400 → 3 条。它只是把 40 条候选变成 400 条，
//     命中率还是那 1.3%。
//   - **放大 LIMIT 更不行，而且它「看起来有效」。** off + `LIMIT 500` 实测
//     返回 400 条（= 这家店的全部）—— 但 EXPLAIN 显示计划已经不是 HNSW 了，
//     规划器改走了**全表顺序扫描 + top-N 排序**。固定计划下再看一次：
//     `enable_seqscan=off` + `LIMIT 500` → 仍然是 0 条，`Rows Removed by
//     Filter: 40` 一个字都没变。也就是说 00016 文件头那句「LIMIT 限不住索引
//     愿意吐几行」是对的，只是它在端到端观察下会被计划切换掩盖成「好像有用」。
//
// ===========================================================================
// 这个陷阱由**规划器的选择**触发，而不是由数据触发 —— 所以必须无条件设
// ===========================================================================
//
// 同一个库、同一条查询，300 件商品的那家商家反而**没有**踩到：规划器给它选
// 的是顺序扫描 + top-N 排序（精确结果，13 ms）。原因是 embedding 列超过
// TOAST 阈值被存到行外，于是「先按 merchant_id 过滤、只给命中的行取向量算
// 距离」非常便宜。两种计划的估算代价（本机实测）：
//
//	顺序扫描      ≈ 0.275 × N（N = 全库向量数）
//	HNSW + LIMIT  ≈ 349 + 4.31 × N × size / M（M = 本商家向量数）
//
// 令两者相等，得到翻转点 **M ≈ 15.7 × size，与 N 无关**（size=20 时约 314 件）。
// 实测吻合：300 件走顺序扫描、400 件走 HNSW。检索那条查询还 JOIN 了 products，
// 多一条「先用 idx_products_listing 取本店商品再逐行算距离」的路，翻转点变成
// 约 sqrt(22 × N)（本机 N=30,340 时实测约 870 件）。
//
// 结论有两层，第二层才是重点：
//
//	① 一家店的商品数越过那条线，它的搜索就从「精确」跳成「零结果」，
//	   而那条线随全库向量数移动 —— 也就是说**别家店上新品会让这家店搜不到东西**。
//	② 翻转点是规划器按代价估算算出来的，它还随统计信息新鲜度、
//	   random_page_cost（SSD 上常见的 1.1 实测就把 400 件那家从顺序扫描推进了
//	   HNSW）、work_mem 一起动 —— 这些是应用看不见也管不着的东西。
//
// 所以正确的姿势不是「判断哪些查询需要」，而是**让 HNSW 计划在任何时候被选中
// 时都是对的**。这三行就是那个保证。
const (
	// hnswIterativeScan 让 HNSW 扫描在候选不够时继续往下走，而不是吐完
	// ef_search 条就收工（pgvector 0.8+，默认 off）。
	//
	// 取 relaxed_order 而不是 strict_order，两个理由都是实测的：
	//
	//   - **召回更好。** 以关掉索引跑出来的精确 top-20 为基准真值，
	//     同一批查询的 overlap@20：relaxed 13/9/11，strict 8/2/5（各 3 次）。
	//     strict_order 要先攒够能保证顺序的一段才敢往外吐，同样的
	//     max_scan_tuples 预算下它拿到的集合更差。
	//   - **略快。** 中位 34.4 ms vs 35.6 ms（各 10 次）。
	//
	// 「顺序可能略乱」这件事在这条链路上不要紧：这一段的产出是喂给 RRF 的
	// **排名列表**（语义检索层 §4），RRF 只用名次，之后还要和关键词那一路重排
	// 一次。需要精确顺序的是精排（§5），而那一层本轮不存在。
	//
	// 注：上面那些 overlap 数字是在**均匀随机向量**上测的，那是 ANN 的最坏
	// 情形（1024 维随机单位向量两两几乎正交，top-20 本来就没有区分度）。
	// 真实 embedding 有聚簇结构，召回会显著好于这个数。它们能说明的是
	// relaxed 与 strict 的**相对**关系，不是绝对召回率。
	hnswIterativeScan = "relaxed_order"

	// hnswMaxScanTuples 是迭代扫描的硬预算：扫到这么多条还凑不够就收工，
	// 返回不完整的结果而不是一直扫下去。20000 是 pgvector 的默认值，
	// 这里**显式写出来**，理由是它决定搜索的行为，而默认值可以被
	// postgresql.conf 改掉 —— 一次服务器调参不该悄悄改变检索结果。
	//
	// 为什么不调大：**延迟先到顶，召回不是瓶颈。** 实测（同一个库，人为把
	// 有效密度压到 0.026% 逼它扫到底）扫 18,525 条要 69–120 ms，
	// 线性外推到 20,000 条约 127 ms。§8 的总预算是 P95 < 300 ms，而这一段
	// 上面还压着 62 ms 的 query embedding —— 20000 已经把余量吃得差不多了。
	// 调大它换来的是「更少见的召回不足」加「更常见的超时」，那笔账不划算。
	hnswMaxScanTuples = 20000

	// hnswEFSearch 是每一轮的候选宽度（默认 40）。
	//
	// **实测在带租户过滤的这条路上它几乎没有区分度**：relaxed_order 下
	// overlap@20 平均 11.0（ef=40）/ 11.0（100）/ 11.2（200）/ 12.4（400），
	// 中位延迟 34.4 ms（40）vs 34.8 ms（100）—— 差别在噪声里。
	// 迭代扫描一开，决定召回的是扫了多少条，不是每轮取多宽。
	//
	// 取 100 而不是留着默认的 40，是为了另一侧：**租户足够密集、迭代扫描几乎
	// 不触发的时候**，ef_search 就是唯一控制召回质量的旋钮，而本实验覆盖不到
	// 那一侧。代价实测为零，所以按 §8 延迟预算表里那个 `ef_search=100` 取。
	hnswEFSearch = 100
)

// withTenantTx 是「开事务 → 设租户 → 跑 fn → 提交」这一串的**唯一**实现。
//
// 包内的 fn 除了 Tx 还拿得到 pgx.Tx，因为子事务屏障要在同一个事务里发自己那条
// INSERT（见 saga.go）。它是包内的：pgx.Tx 到不了本包之外，Tx 才是交出去的东西。
//
// 合成一处而不是让 WithSagaBranch 自己再写一遍 Begin + set_config：那句
// set_config 是整个租户隔离的落点，两份实现意味着将来有人只改对其中一份，
// 而漏掉的那一份的症状是线上偶发 42501。
func (r *Repo) withTenantTx(ctx context.Context, fn func(pgx.Tx, Tx) error) error {
	merchantID, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Commit 之后再 Rollback 是无害的 no-op（pgx 返回 ErrTxClosed），
	// 所以这条 defer 只在提前 return 的路径上真正起作用。
	defer tx.Rollback(ctx)

	// 用 set_config(..., is_local => true) 而不是 SET LOCAL 拼字符串。
	//
	// 两者语义等价——第三个参数 true 就是 LOCAL，作用域到事务结束为止——
	// 但 SET LOCAL 不吃绑定参数，只能 fmt.Sprintf 拼进 SQL。这里的 merchantID
	// 是 int64 且来自中间件，眼下确实没有注入面，可那是个随时会失效的前提：
	// 哪天租户标识从 int64 变成 code（TEXT），拼接就地变成漏洞，而那次改动
	// 看上去只是换了个类型。set_config 让这个前提根本不必存在。
	//
	// 必须是 local 而不是会话级：会话级的设置会留在连接上，被池交给下一个
	// 请求时就是一次跨租户泄露。internal/repository/tenant_test.go 里的
	// TestTenantSettingDiesWithTheTransaction 钉住这一点（把 true 改成 false 会红）。
	//
	// 三个 hnsw.* 与它写在同一句里，理由见下面 hnswIterativeScan 那一段：
	// 它们补的正是这一句 set_config 带来的那个副作用。合成一条语句不是为了
	// 省字，是为了省一次往返 —— 每个请求都要发的语句，多一次 RTT 就是每个
	// 请求都多一次。
	if err := enterTenantScope(ctx, tx, merchantID); err != nil {
		return err
	}

	// scope 取一份**副本**的地址而不是 &merchantID：同一个事务里造出来的
	// 每一个领域对象都会拿到这个指针，共用一个局部变量的地址意味着谁改了它
	// 就改了所有人的租户。它今天没人改，而这行代价是零。
	scope := merchantID
	if err := fn(tx, tenantTx{q: db.New(tx), scope: &scope, raw: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enterTenantScope 把当前事务切进**某一家店**的租户作用域。
//
// 它是那句 set_config 的**唯一**实现，被两个入口共用：withTenantTx（租户从
// ctx 取）与 WithNewTenant（租户是这个事务刚刚建出来的那一家，merchant.go）。
// 抽出来不是为了省行数 —— 这一句是整个租户隔离的落点，而它的两个调用点都
// 不在同一个文件里。抄一份的后果与文件头那段写的一样：将来有人只改对其中
// 一份，漏掉的那一份的症状是线上偶发 42501，或者（更糟）一个作用域不对却
// 不报错的事务。
//
// **它同时把 app.platform_scope 显式关掉。** 这一句看上去多余（大多数事务
// 从来没打开过它），它防的是一个具体的、会静默出错的路径：WithNewTenant 先
// 以平台作用域建店，再切到新店的租户作用域。漏掉这一句的话
// staff_scope_merchant() 仍然返回 NULL，于是「新店的第一个管理员」会被建成
// 一个**平台级管理员** —— 一行 merchant_id 为 NULL 的 staff，拥有跨租户
// 运维权。它不会报错，也不会有任何一条约束拦下来。
func enterTenantScope(ctx context.Context, tx pgx.Tx, merchantID int64) error {
	_, err := tx.Exec(ctx,
		`SELECT set_config('app.merchant_id', $1, true),
		        set_config('app.platform_scope', 'off', true),
		        set_config('hnsw.iterative_scan', $2, true),
		        set_config('hnsw.max_scan_tuples', $3, true),
		        set_config('hnsw.ef_search', $4, true)`,
		strconv.FormatInt(merchantID, 10),
		hnswIterativeScan,
		strconv.Itoa(hnswMaxScanTuples),
		strconv.Itoa(hnswEFSearch))
	return err
}
