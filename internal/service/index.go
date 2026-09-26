package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/understanding"
)

// 商品理解服务的慢路径：入队（触发点扫描）+ 出队（worker）+ 两个 processor。
//
// M3 Task 3 落的是同一件事的**扫描式**版本：一轮扫描里判定、调引擎、写回一气呵成。
// M4 阶段 1 把中间那道缝切开，插进 §12 那张 jobs 表 —— 从此「谁在什么时候做」
// 与「怎么做」是两件事。下面第零节讲这次改造换来了什么、代价是什么；
// 第一节往后是 M3 那份文件头，判据、水位线、引擎故障三段一个字都没变，
// 因为改造没有动它们。
//
// ===========================================================================
// 零、为什么要真的插一张队列表进来
// ===========================================================================
//
// 扫描式版本能跑，而且在单租户下跑得挺好。它缺的是三样，每一样都只在多租户
// 或多进程下才显出来：
//
//   - **失败没有地方住。** 扫描式的重试全靠「水位线没推，下一轮还在候选里」，
//     于是失败次数、退避、死信都不存在：一件永远写不回去的商品会每 30 秒
//     被重算一次，永远。而 last_error 不能写（写它会把水位线推掉，
//     db/queries/semantic.sql 的 MarkProductIndexed 上有实测）——
//     也就是说那一轮里「失败」这件事在库里没有任何痕迹。
//     队列给了它一个不在水位线上的家：jobs.attempts / run_after / last_error。
//   - **公平只在一个进程里成立。** 扫描式的每租户上限与轮转起点是进程内的
//     游标。起两个副本，两个进程各自从自己的游标开始扫，同一批商品被算两遍。
//     队列的公平在 SQL 里（§12 那两条出队语句），而且 FOR UPDATE SKIP LOCKED
//     让多个消费者天然不撞车。
//   - **回填与实时挤在同一条路上。** §8 说得很重：`priority` 只在租户内排序，
//     真正保护其他商家的是队列的每租户在途上限。没有队列就没有「在途」这个概念，
//     那条上限无处安放。
//
// **代价写清楚，两笔：**
//
//  1. 触发点是粗的（下面第一节），它捞回来的大部分商品判定结论是「什么都不用做」。
//     扫描式里这批商品只花一次判定 + 一次水位线更新；现在它们还要多一次
//     INSERT（入队）和一次 UPDATE（标成功）。这是真金白银的写放大。
//     **换掉它的办法是在入队前先判定一次，而那条路被否决了**：判定要读
//     input_hashes、要算指纹，两处各写一份迟早分叉，而分叉的症状是
//     「某一条路上的商品不再被重算」——正是这整个文件在防的那件事。
//     队列的意义就是让「什么时候做」与「怎么做」解耦，把判定塞回生产者
//     等于把刚切开的缝又焊上。
//  2. 一件商品从「标题改了」到「向量更新了」多了一跳。延迟预算仍然满足
//     （语义检索层 §2.2 给的是 < 1 分钟，扫描 30 秒一轮 + worker 轮询 1 秒）。
//
// ### 入队点**只有一个**
//
// 触发点扫描（EnqueueOnce）与全量（Backfill）都走 tx.EnqueueJob，除此之外
// 没有第二处入队。发布商品那条路**刻意不入队** —— 上架会拨动
// products.updated_at，触发点当轮就能捞到它。加一个「发布即入队」的快捷方式
// 听起来只是省 30 秒，实际是让「什么时候该重算」变成两份判断，
// 而它们分叉的那天，症状是某一类改动再也不会触发重算。
//
// ===========================================================================
// 一、判据是两段的，不是一段
// ===========================================================================
//
// M3 计划第四条提的判据是「product_text_vectors.updated_at 与
// products.updated_at 的先后关系」。它**只能当一半用**，理由由
// internal/db/semantic_test.go 的 TestStalenessCriterionRawMaterial 用真实数据
// 钉住了：touch_updated_at 挂在整张 products 上，改 sales_count 也会让
// updated_at 前进。拿它单独当判据，一次下单就把全店商品判成向量过期，
// 而重算一遍 embedding 的钱是真花出去的。
//
//	· 触发点（不许漏算）：时间戳的先后关系。粗，但不漏。在 SQL 里
//	  （db/queries/semantic.sql 的 ListStaleProductsForIndex）。它看的是
//	  **products 与 categories 两张表**的 updated_at —— 送进模型的文本里有
//	  类目名，而 `UPDATE categories SET name = ...` 一行 products 都不碰。
//	  少了类目那一支，改一次类目名就让该类目下全部商品的向量永久过期，
//	  判定那一半连跑的机会都没有。完整论证与「为什么不挂触发器」写在那条 SQL 上。
//	· 判定（不许滥算）：**只看指纹**。在这个文件的 decide 里，
//	  只比 product_understanding.input_hashes 的那一格与当前文本算出的指纹。
//
// 两个 processor 两格指纹，各记各的（数据模型 §8「增量重算的指纹：只认一处」）：
// text_embedding 的输入是标题/副标题/类目，search_text 的输入只有标题/副标题。
// 换一次类目只重算向量，不重写 bigram 串。
//
// **M4 起「哪个 processor 关心哪几个字段」有了可机械检查的落点**：
// internal/understanding 的 Processor.InputFields()，以及那个包里逐字段扰动的
// TestFingerprintDependsExactlyOnInputFields。decide 从此调 processor 的
// Fingerprint，不再直接调 search.ProductText 的两个方法。
//
// ### 一条不许引入的「优化」
//
// 00016 的文件头点名了它：给 products 加一个把 product_text_vectors.updated_at
// 同步过去的级联触发器（「让派生数据的时间戳保持一致」）。加上之后先后关系
// 变成恒等式，判据永远说「没过期」，而且不报任何错。
//
// 这个文件里与它最像、但**性质不同**的一件事是 MarkProductIndexed：
// 每判定一次就把 product_understanding.updated_at 推到当前。差别在于
//
//	· 它由应用在**判定之后**写，而判定的依据是指纹；触发器是数据库在
//	  任何一次 products 更新时无条件写的，中间没有任何判断。
//	· 它推的是 product_understanding 的时间戳，不是向量表的。
//	  00016 文件头那条可机械检查的闸门 ——「若 products.updated_at >
//	  product_text_vectors.updated_at，则 input_hashes ->> 'text_embedding'
//	  必须等于当前文本指纹」—— 因此照旧成立，而且 index_test.go 里
//	  TestStalenessGateHoldsAfterIndexing 就是在跑它。
//
// ### 为什么需要这个水位线
//
// 没有它的话，一件被 sales_count 变更推过的商品每一轮都会被重新捞回来，
// 而且因为 ORDER BY updated_at 它排在队首，把每租户配额占满 ——
// 队尾那件真的改了标题的商品一轮也轮不到。那是公平调度要防的饿死，
// 只不过换了个地方发生。
//
// ===========================================================================
// 二、租户从哪来 —— 生产者照 sweep.go 那一套，消费者照 §12 那一套
// ===========================================================================
//
// **生产者**（EnqueueOnce / Backfill）和超时补偿一样跑在任何 HTTP 请求之外：
// 没有 Host（tenant.Resolver 用不上），也没有 gid（dtm.TenantContextFromGID
// 用不上）。答案同样是枚举 merchants（tenant-root 类，刻意没有 RLS）再一家一家
// 进 WithTenant，论证写在 repository/sweep.go 与 service/sweep.go 的文件头。
// 每租户上限 + 每轮总预算 + 轮转起点 + 兜底那一趟，形状照搬 sweep.go。
//
// **消费者**（WorkOnce）不需要枚举：租户写在出队拿到的那一行上
// （jobs.merchant_id），worker 按它进 WithTenant。这是队列相对扫描式的另一个
// 好处 —— 公平调度由 SQL 给出（§12 的「上限 + 兜底」），应用层那份进程内的
// 轮转游标在消费侧不再需要。
//
// 生产侧那份仍然需要：触发点扫描本身是按租户切的 N 条查询（products 有 RLS，
// 没有一条能跨租户的 SELECT），所以「这一轮先扫谁」还得应用层决定。
// 生产侧那份公平调度因此照搬 sweep.go：每租户上限 + 每轮总预算 + 轮转起点 +
// 兜底那一趟，形状与理由见 sweep.go「公平调度」那一段。这里只说一处**不同**：
// 上限的量级不一样。超时补偿的一笔是一个短事务，这里的一件商品要花掉一次
// 模型推理（infero + Qwen3-Embedding-0.6B 在 RTX A4000 上批量约 1.6 ms/条；
// M3 那个 BGE-M3 跑 CPU 的实现是 33 ms/条，差 20 倍 —— 留着这个对照是因为
// 下面那个「一轮一家店最多打一次」的选择当年是按 33 ms 定的，而它在 1.6 ms
// 上仍然成立：贵的从来不是这一批算多久，是别让一家店占满一轮的预算），
// 所以每租户上限直接取 inference.DefaultBatchSize —— 一轮一家店最多打一次
// /v1/embeddings，正好是 §10「索引侧批大小 32–64」的上限。
//
// ===========================================================================
// 三、引擎挂了怎么办：一个向量都不写，但别把不用引擎的那部分也拖下水
// ===========================================================================
//
// 客户端出错时一个向量都不返回（internal/inference 的文件头第 ③ 条），
// 这里也不吞：零向量入库之后余弦距离对它恒等于 1，它会出现在**每一次**检索的
// 结果里，而没有任何东西会报错。
//
// 但引擎挂了的那一批里，那些只需要重写 bigram 串的商品**照常处理**。
// 不这么做的话，一次引擎故障会连带把关键词召回的维护也停掉。
//
// 需要向量而没拿到的那些任务走 RetryJob：指数退避放回队列（§12），
// 五次之后进死信。**这是队列换来的最实在的一样东西** —— 扫描式版本里
// 「引擎连续挂了五轮」和「挂了一轮」在库里长得一模一样。
//
// ### 写回失败也走 RetryJob，而 last_error 终于有地方放了
//
// M3 那一版在这里挂过一笔账：写回失败不写 product_understanding.last_error，
// 因为那张表上挂着 touch_product_understanding_updated_at，**任何一次 UPDATE
// 都会把 updated_at 推到 now()**，而那一列就是触发点的水位线 ——
// 「记下这次失败」这个动作本身会把商品从候选集里踢出去。
//
// 那笔账现在还在（product_understanding.last_error 仍然恒为 NULL），
// 但失败不再是没有痕迹的：它落在 jobs.last_error 上，而 jobs 与水位线无关。
// 这正是「同一件事只记一处」那条规矩的正解 —— 重试与失败是队列的账。
// IndexRepository 是这个任务需要的仓储能力。
//
// 它横跨两面：WithTenant + ActiveMerchants 是生产者那一半（与 SweepRepository
// 同形），剩下五个是队列那一半，全部跑在 pool 上、不在任何租户事务里
// （理由见 repository/jobs.go 的文件头）。
type IndexRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)

	DequeueJobs(ctx context.Context, req repository.DequeueRequest) ([]repository.Job, error)
	FinishJobs(ctx context.Context, ids []int64) error
	RetryJob(ctx context.Context, id int64, reason string) error
	ReapStuckJobs(ctx context.Context, queue string, olderThan time.Duration) (int64, error)
	PurgeFinishedJobs(ctx context.Context, queue string, retain time.Duration, limit int) (int64, error)
}

// IndexConfig 是一轮的预算。
type IndexConfig struct {
	// PerTenantCap 每轮每租户至多**入队**几件商品。<= 0 用 DefaultIndexPerTenantCap。
	PerTenantCap int

	// RoundBudget 每轮总共至多入队几件。<= 0 用 DefaultIndexRoundBudget。
	RoundBudget int

	// Interval 两轮扫描之间的间隔。<= 0 用 DefaultIndexInterval。
	Interval time.Duration

	// PerTenantInflight 是**每租户在途上限**，队列那一半的核心参数。
	// <= 0 用 DefaultIndexPerTenantInflight。
	PerTenantInflight int

	// Workers 是并发消费者数。<= 0 用 DefaultIndexWorkers。
	Workers int

	// PollInterval 是队列空了之后隔多久再看一眼。<= 0 用 DefaultIndexPollInterval。
	PollInterval time.Duration
}

const (
	// DefaultIndexPerTenantCap 一轮里单家商户至多入队 64 件。
	//
	// 它刻意等于 inference.DefaultBatchSize：一轮一家店最多入队一批，
	// 出队侧最多凑成一次 /v1/embeddings，正好落在语义检索层 §10
	//「索引侧批大小 32–64」的上限上。
	// 调大它不会让引擎跑得更快，只会让客户端把一次调用拆成两个批 ——
	// 那是同一件事，只是错误的粒度变粗了（一批失败拖累另一批）。
	DefaultIndexPerTenantCap = inference.DefaultBatchSize

	// DefaultIndexRoundBudget 一轮总共至多入队 512 件。
	//
	// 没有总预算的话，「每租户上限」只是把一轮拉长到没有边界。
	DefaultIndexRoundBudget = 512

	// DefaultIndexInterval 30 秒一轮扫描。
	//
	// 语义检索层 §2.2 那张「更新触发时机」表给「标题/属性/类目变更」的时效是
	// **< 1 分钟**。30 秒一轮 + 一次出队的等待，落在那个要求里；
	// 60 秒一轮就压线了，一次积压就超。
	DefaultIndexInterval = 30 * time.Second

	// DefaultIndexPerTenantInflight 每租户至多 64 条任务同时在途。
	//
	// ===========================================================================
	// 这一条不是调优参数，它是队列在多租户下成立的前提
	// ===========================================================================
	//
	// 商品理解服务设计 §8 把理由写得很重：`priority` 只在租户内排序 ——
	// A 商家把回填标成最低优先级，也不妨碍这十万条任务占满整个 worker 池，
	// 而 B 商家的一条商品发布就卡在后面。**一个新商家导入一次商品目录，
	// 就能让所有其他店铺的搜索索引停止更新几小时。**
	//
	// 真正起作用的是这条上限：回填再多也只能占住有限几个 worker。
	// idx_jobs_inflight 那个索引就是为它建的。
	//
	// 取值等于一次 /v1/embed 的批大小：一家店同时在途的量，正好是它能把
	// 引擎占住的那一次调用。再大就是让一家店占住两个批。
	DefaultIndexPerTenantInflight = inference.DefaultBatchSize

	// DefaultIndexWorkers 两个并发消费者。
	//
	// **为什么至少是 2**：在途上限是按 status = 1 数出来的，而单个消费者的
	// 「出队 → 处理 → 交回」是串行的 —— 它下一次出队时自己手上的任务已经
	// 交回了，在途数是 0，于是 busy 永远是空集，那条上限一次都不会生效。
	// 两个消费者才让「A 正在跑的时候 B 来取」这件事真的发生。
	//
	// **为什么不更多**：它们最终都排在同一块 GPU 上（infero 是单实例，
	// compose.infero.yaml 的文件头）。再加消费者只是把等待从队列挪到引擎里，
	// 而每租户上限的效果不随消费者数线性变好 —— 它由上限本身决定。
	DefaultIndexWorkers = 2

	// DefaultIndexPollInterval 队列空了之后 1 秒再看一眼。
	//
	// 不用 LISTEN/NOTIFY：那要给每个消费者一条独占连接，而 §12 的方案
	// 明确是「一张表」。1 秒的轮询在 30 秒一轮的生产节奏下不构成延迟来源，
	// 空转的代价是每个消费者每秒一条走索引的查询。
	DefaultIndexPollInterval = time.Second

	// indexStuckAfter 超过这么久还没交回的任务会被回收（repository.ReapStuckJobs）。
	//
	// 它必须显著大于一次正常处理的耗时（一批 64 件 ≈ 一次 /v1/embed，
	// 实测 CPU 上 33ms/条 ≈ 2 秒；GPU 上快一个量级），又必须显著小于
	// 「运维发现搜索不更新了」的时间。5 分钟。
	//
	// 取小了会把还在跑的任务回收掉，于是同一件商品被算两遍 —— 钱花两次，
	// 结果一样，不报错。取大了则那家店的在途配额被一个死进程占住 5 分钟以上。
	indexStuckAfter = 5 * time.Minute

	// indexDoneRetention 成功的任务保留 7 天（§12）。死信永久保留。
	indexDoneRetention = 7 * 24 * time.Hour

	// indexPurgeBatch 一次清理最多删几条。见 repository.PurgeFinishedJobs 的注释：
	// 清理永远是一次有界的操作。
	indexPurgeBatch = 1000
)

// IndexService 把商品的两份派生数据算出来并写进库。
type IndexService struct {
	repo IndexRepository
	emb  inference.Embedder
	log  *slog.Logger
	cfg  IndexConfig

	// cursor 是**生产侧**的轮转起点，理由与 SweepService.cursor 完全一样。
	// 消费侧不需要它：公平调度在 SQL 里（文件头第二节）。
	cursor uint64

	// workerID 落进 jobs.locked_by，排查卡死用（§12）。
	workerID string

	textEmbedding understanding.TextEmbedding
	searchText    understanding.SearchText
}

// NewIndexService 建派生数据入库服务。emb 为 nil 时返回错误 ——
// 一个没有引擎的索引任务能跑、能把 bigram 串写对、能把水位线推上去，
// 唯独一条向量都不产生，而那是它一半的职责。让它在装配期就说不出话，
// 好过在运行期每一轮安静地少做一半事。
func NewIndexService(r IndexRepository, emb inference.Embedder,
	cfg IndexConfig, log *slog.Logger) (*IndexService, error) {
	if emb == nil {
		return nil, errors.New("没有推理引擎客户端，拒绝构造派生数据入库服务：" +
			"没有它这个任务只会写 bigram 串、一条向量也不产生，而它不会报错")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.PerTenantCap <= 0 {
		cfg.PerTenantCap = DefaultIndexPerTenantCap
	}
	if cfg.RoundBudget <= 0 {
		cfg.RoundBudget = DefaultIndexRoundBudget
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultIndexInterval
	}
	if cfg.PerTenantInflight <= 0 {
		cfg.PerTenantInflight = DefaultIndexPerTenantInflight
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultIndexWorkers
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultIndexPollInterval
	}
	host, _ := os.Hostname()
	return &IndexService{
		repo:     r,
		emb:      emb,
		log:      log,
		cfg:      cfg,
		workerID: fmt.Sprintf("%s/%d", host, os.Getpid()),
	}, nil
}

// IndexReport 是一轮（或一次全量）的结果。
//
// Embedded 与 Judged 分开记是这个报告最要紧的一件事：判据的全部意义就是让
// 前者远小于后者。两个数贴着跑，说明判定那一半没起作用，钱在白花，
// 而检索结果看上去完全正常。
type IndexReport struct {
	// —— 生产侧 ——

	// Tenants 这一轮扫过几家商户。
	Tenants int

	// Scanned 触发点捞回来几件（= 入队候选）。
	Scanned int

	// Enqueued 真的入了几条任务。
	Enqueued int

	// Deduped 队列里已经有同一条了，没入。**正常路径**：
	// 扫描每 30 秒一轮，而一件商品的加工可能跨好几轮。
	Deduped int

	// Fallback 这一轮跑过兜底那一趟（去掉每租户上限）。
	Fallback bool

	// —— 消费侧 ——

	// Judged 判定过几件商品（= 出队之后真的读到了那一行商品）。
	Judged int

	// Embedded 真的调引擎重算了几条向量。
	Embedded int

	// SearchTextWritten 真的重写了几条 bigram 串。
	SearchTextWritten int

	// Skipped 判定为「两样都不用动」，只推了水位线。
	Skipped int

	// Raced 判定与写回之间商品又被改了，整条跳过留给下一轮。正常路径。
	Raced int

	// Gone 出队之后商品已经不在了（下架 / 软删）。正常路径：队列是异步的。
	Gone int

	// Failed 真的出错了。非零就该有人看日志。
	Failed int

	// DeadLettered 这一轮有几条任务用尽重试次数进了死信。非零必须有人看。
	DeadLettered int

	// EngineDown 这一轮有几批引擎调用失败。它与 Failed 分开：
	// 引擎不可用是一件会自己好的事（走降级、退避重试），
	// 而 Failed 里混着写库失败、数据坏了这些要人来看的东西。
	EngineDown int
}

// Merge 把两份报告加起来。导出是给 cmd/keel-index 用的：那条命令要把
// 「入队」与「抽干」两段的数字合成一份打印出来。
func (r IndexReport) Merge(o IndexReport) IndexReport { return r.add(o) }

func (r IndexReport) add(o IndexReport) IndexReport {
	r.Tenants += o.Tenants
	r.Scanned += o.Scanned
	r.Enqueued += o.Enqueued
	r.Deduped += o.Deduped
	r.Judged += o.Judged
	r.Embedded += o.Embedded
	r.SearchTextWritten += o.SearchTextWritten
	r.Skipped += o.Skipped
	r.Raced += o.Raced
	r.Gone += o.Gone
	r.Failed += o.Failed
	r.DeadLettered += o.DeadLettered
	r.EngineDown += o.EngineDown
	r.Fallback = r.Fallback || o.Fallback
	return r
}

// Run 起一个生产者（按 Interval 扫触发点入队）与 cfg.Workers 个消费者，
// 直到 ctx 被取消。
//
// 第一轮扫描在启动后立刻跑（理由同 SweepService.Run：进程刚重启时可能已经
// 积压了很久）。**回收（ReapStuckJobs）排在第一次扫描之前**，而且这个顺序
// 是硬的：上一个进程被 kill 掉时手上那批任务停在「执行中」，它们占着那家店的
// 在途配额；不先回收就先入队，新任务会被自己上一条命的残骸挡在门外。
func (s *IndexService) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.consume(ctx)
		}()
	}

	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.housekeep(ctx)
		s.produceOnce(ctx)
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "派生数据入库任务收到停止信号，等消费者收尾")
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

// consume 是一个消费者的主循环：有活就一直干，没活就歇 PollInterval。
func (s *IndexService) consume(ctx context.Context) {
	for {
		rep, err := s.WorkOnce(ctx)
		switch {
		case err != nil:
			s.log.ErrorContext(ctx, "出队这一批没跑起来", "err", err)
		case rep.Judged > 0 || rep.Gone > 0:
			s.log.InfoContext(ctx, "处理完一批理解任务",
				"judged", rep.Judged, "embedded", rep.Embedded,
				"search_text", rep.SearchTextWritten, "skipped", rep.Skipped,
				"raced", rep.Raced, "gone", rep.Gone, "failed", rep.Failed,
				"dead_lettered", rep.DeadLettered, "engine_down", rep.EngineDown)
			continue // 还有活，别歇
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.PollInterval):
		}
	}
}

func (s *IndexService) produceOnce(ctx context.Context) {
	rep, err := s.EnqueueOnce(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "触发点扫描这一轮没跑起来", "err", err)
		return
	}
	if rep.Scanned == 0 {
		s.log.DebugContext(ctx, "触发点这一轮没有待入队的商品", "tenants", rep.Tenants)
		return
	}
	s.log.InfoContext(ctx, "触发点扫描完成一轮",
		"tenants", rep.Tenants, "scanned", rep.Scanned,
		"enqueued", rep.Enqueued, "deduped", rep.Deduped, "fallback", rep.Fallback)
}

// housekeep 回收卡死的任务、清理过期的成功任务。两件事都不该让一轮扫描失败。
func (s *IndexService) housekeep(ctx context.Context) {
	if n, err := s.repo.ReapStuckJobs(ctx, repository.QueueProductUnderstanding,
		indexStuckAfter); err != nil {
		s.log.ErrorContext(ctx, "回收卡死任务失败", "err", err)
	} else if n > 0 {
		// WARN 而不是 INFO：正常运行里这个数恒为 0。非零意味着上一次有 worker
		// 没能把任务交回来（进程被 kill、或者一次处理真的跑了五分钟以上），
		// 而那期间那家店的在途配额是被占着的。
		s.log.WarnContext(ctx, "回收了卡在执行中的理解任务（worker 没在预期时间内交回）",
			"count", n, "stuck_after", indexStuckAfter)
	}
	if n, err := s.repo.PurgeFinishedJobs(ctx, repository.QueueProductUnderstanding,
		indexDoneRetention, indexPurgeBatch); err != nil {
		s.log.ErrorContext(ctx, "清理过期的成功任务失败", "err", err)
	} else if n > 0 {
		s.log.DebugContext(ctx, "清理了过期的成功任务", "count", n)
	}
}

// ---------------------------------------------------------------------------
// 生产者：触发点扫描 → 入队
// ---------------------------------------------------------------------------

// EnqueueOnce 跑一轮触发点扫描并把捞到的商品入队。
//
// 公平调度的形状与 SweepService.SweepOnce 逐条对应，理由写在 sweep.go。
// 这里的「预算」花在**入队**上，不再花在推理上 —— 推理那一侧的预算由
// 每租户在途上限管（DefaultIndexPerTenantInflight）。
func (s *IndexService) EnqueueOnce(ctx context.Context) (IndexReport, error) {
	merchants, err := s.repo.ActiveMerchants(ctx)
	if err != nil {
		return IndexReport{}, err
	}
	rep := IndexReport{Tenants: len(merchants)}
	if len(merchants) == 0 {
		return rep, nil
	}

	start := int(s.cursor % uint64(len(merchants)))
	s.cursor++
	budget := s.cfg.RoundBudget

	capped := false
	for i := 0; i < len(merchants) && budget > 0; i++ {
		take := min(s.cfg.PerTenantCap, budget)
		one := s.enqueueTenant(ctx, merchants[(start+i)%len(merchants)], take, false, 0, false)
		rep = rep.add(one.IndexReport)
		budget -= one.Scanned
		if one.Scanned >= take {
			capped = true
		}
	}

	// 兜底那一趟：预算没用完、而第一趟有人被上限卡住。两个条件缺一不可，
	// 理由逐字同 sweep.go —— 公平的目的是防饿死，不是让机器闲着。
	if capped && budget > 0 {
		rep.Fallback = true
		for i := 0; i < len(merchants) && budget > 0; i++ {
			one := s.enqueueTenant(ctx, merchants[(start+i)%len(merchants)], budget, false, 0, false)
			rep = rep.add(one.IndexReport)
			budget -= one.Scanned
		}
	}
	rep.Tenants = len(merchants)
	return rep, nil
}

// Backfill 把一家商户的在架商品**全部**入队（全量），按 id 游标翻页直到跑完。
//
// force = true 时任务的 payload 带上 force，worker 跳过判定、无条件重算 ——
// 换模型、改拼接模板之后用它。
//
// 常规的全量（force = false）入队之后仍然走判定，所以在一个已经索引过的库上
// 跑它几乎不花引擎的钱（花的是入队与判定那两次写）；它补的是「触发点扫不到的
// 存量」：00016 刚落地时全库 search_text 都是 NULL、向量表是空的，
// 而那一刻没有任何 products.updated_at 前进。
//
// 它不是定时任务，是 cmd/keel-index 调的那条路。**它只入队，不处理** ——
// 调用方接着调 Drain。两件事分开的好处很直接：一个正在跑着服务进程的部署，
// 全量入队之后由服务进程自己的消费者慢慢消化，而那批任务照样受每租户在途
// 上限管 —— 也就是说 §8 那句「一个新商家导入一次商品目录」的场景，
// 由队列而不是由这条命令负责不压垮别人。
func (s *IndexService) Backfill(ctx context.Context, merchantID int64, force bool) (IndexReport, error) {
	rep := IndexReport{Tenants: 1}
	after := int64(0)
	for {
		one := s.enqueueTenant(ctx, merchantID, s.cfg.PerTenantCap, true, after, force)
		rep = rep.add(one.IndexReport)
		rep.Tenants = 1
		if one.Scanned == 0 {
			return rep, nil
		}
		if one.lastID <= after {
			// 游标没前进。全量那条查询是 `id > @after_id ORDER BY id`，
			// 正常情况下最后一行的 id 一定比游标大；不大就说明查询被改坏了，
			// 而症状会是一个永远跑不完的全量任务。
			return rep, fmt.Errorf("全量翻页的游标没有前进（%d → %d），"+
				"ListProductsForIndex 的 ORDER BY 或 WHERE 被改过了", after, one.lastID)
		}
		after = one.lastID
	}
}

// jobPayload 是 jobs.payload 的形状。刻意只有两个字段，理由见 00022 的
// payload 那一列的注释：这张表没有 RLS，payload 越薄越好。
type jobPayload struct {
	ProductID int64 `json:"product_id"`
	// Force 跳过判定无条件重算。只有 cmd/keel-index -force 会置位。
	Force bool `json:"force,omitempty"`
}

// enqueueTenant 扫一家商户至多 limit 件商品并入队。
//
// full = true 走全量那条查询（按 id 游标），afterID 是游标；
// full = false 走触发点那条（按 updated_at 排序），afterID 无意义。
func (s *IndexService) enqueueTenant(ctx context.Context, merchantID int64,
	limit int, full bool, afterID int64, force bool) tenantResult {
	var res tenantResult
	if limit <= 0 {
		return res
	}
	// 租户上下文在这里产生 —— 与 sweep.go 同一个理由，
	// 登记在 internal/service/tenant_context_test.go 的 tenantContextAllowed 里。
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var cands []repository.IndexCandidate
		var err error
		if full {
			cands, err = tx.ListProductsForIndex(ctx, afterID, int32(limit))
		} else {
			cands, err = tx.ListStaleProductsForIndex(ctx, int32(limit))
		}
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return nil
		}
		res.Scanned = len(cands)
		res.lastID = cands[len(cands)-1].ProductID

		// 入队与扫描在**同一个事务**里。分开的话，扫描读到的那一批与入队写进去
		// 的那一批之间隔着一次提交，中途失败会留下「扫过了但没入队」——
		// 而扫描是幂等的、入队也是幂等的（uk_jobs_pending），所以合并进一个
		// 事务不花什么，却让这一轮要么整批进去要么一条都不进。
		for _, c := range cands {
			payload, err := json.Marshal(jobPayload{ProductID: c.ProductID, Force: force})
			if err != nil {
				return err
			}
			ok, err := tx.EnqueueJob(ctx, repository.NewJob{
				Queue:   repository.QueueProductUnderstanding,
				JobKey:  fmt.Sprintf("product:%d", c.ProductID),
				Payload: payload,
				// priority 留 0。回填也是 0 —— §12 说回填该用 -10，
				// 但那条建议的前提是「回填与实时共用一个队列且都由这个服务入队」，
				// 而本轮回填的入口是一条人工命令（cmd/keel-index），
				// 它与实时任务撞车的窗口是运维自己选的。真要给它 -10，
				// 那得先让 Backfill 知道自己是不是回填 —— 而 force 这个参数
				// 表达的是「跳过判定」，不是「我是回填」。两件事不要用同一个开关。
			})
			if err != nil {
				return err
			}
			if ok {
				res.Enqueued++
			} else {
				res.Deduped++
			}
		}
		return nil
	})
	if err != nil {
		log.ErrorContext(ctx, "触发点扫描 / 入队失败", "err", err)
		return tenantResult{IndexReport: IndexReport{Failed: 1}}
	}
	return res
}

// tenantResult 是一家商户这一段的结果。它就是 IndexReport 加一个游标。
type tenantResult struct {
	IndexReport
	lastID int64
}

// ---------------------------------------------------------------------------
// 消费者：出队 → 判定 → 处理 → 写回 → 交回
// ---------------------------------------------------------------------------

// WorkOnce 出一批任务并把它们做完。
//
// 一次最多取 PerTenantInflight 条 —— **这个数同时是批大小与在途上限，
// 而那不是巧合**：§12 那条上限是按 status = 1 的行数算的，所以一次出队
// 取多少，就是一个消费者能让一家店同时在途多少。取得比上限多的话，
// 一家有十万条待办的店会在一次出队里把整批预算吃光，而 busy 那个集合
// 是在出队**之前**算的 —— 上限拦不住它。
func (s *IndexService) WorkOnce(ctx context.Context) (IndexReport, error) {
	jobs, err := s.repo.DequeueJobs(ctx, repository.DequeueRequest{
		Queue:             repository.QueueProductUnderstanding,
		Limit:             s.cfg.PerTenantInflight,
		PerTenantInflight: s.cfg.PerTenantInflight,
		WorkerID:          s.workerID,
	})
	if err != nil || len(jobs) == 0 {
		return IndexReport{}, err
	}

	// 按租户分组：写回要在那家店的租户事务里做，而引擎调用按租户攒批
	// （一家店一批，正好一次 /v1/embed）。
	byTenant := map[int64][]repository.Job{}
	order := []int64{}
	for _, j := range jobs {
		if _, ok := byTenant[j.MerchantID]; !ok {
			order = append(order, j.MerchantID)
		}
		byTenant[j.MerchantID] = append(byTenant[j.MerchantID], j)
	}

	var rep IndexReport
	for _, mid := range order {
		rep = rep.add(s.workTenant(ctx, mid, byTenant[mid]))
	}
	return rep, nil
}

// Drain 反复 WorkOnce 直到队列里没有本队列的任务为止，返回合计。
//
// 它给 cmd/keel-index 用：那条命令入队之后要把活干完再退出。
// 服务进程里不用它 —— 那边是 cfg.Workers 个常驻消费者。
func (s *IndexService) Drain(ctx context.Context) (IndexReport, error) {
	var total IndexReport
	for {
		one, err := s.WorkOnce(ctx)
		total = total.add(one)
		if err != nil {
			return total, err
		}
		if one.Judged == 0 && one.Gone == 0 && one.Failed == 0 {
			return total, nil
		}
	}
}

// workTenant 处理一家商户的一批任务。
func (s *IndexService) workTenant(ctx context.Context, merchantID int64,
	jobs []repository.Job) IndexReport {

	var rep IndexReport
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	// payload 解不开的任务直接进退避 —— 它不会因为重试而变好，
	// 但也不该让同一批里别的任务陪葬。五次之后进死信，有人会看到。
	ids := make([]int64, 0, len(jobs))
	jobOf := map[int64]repository.Job{}
	force := map[int64]bool{}
	for _, j := range jobs {
		var p jobPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil || p.ProductID <= 0 {
			rep.Failed++
			s.retry(ctx, log, &rep, j, fmt.Errorf("payload 解不开或没有 product_id: %s", j.Payload))
			continue
		}
		ids = append(ids, p.ProductID)
		jobOf[p.ProductID] = j
		force[p.ProductID] = p.Force
	}
	if len(ids) == 0 {
		return rep
	}

	var cands []repository.IndexCandidate
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		cands, err = tx.ListProductsForIndexByID(ctx, ids)
		return err
	}); err != nil {
		// 整批读不回来。每条都退避 —— 不能直接标成功（那会让这批商品的派生数据
		// 永远停在旧版本上，而触发点要等到下一次商品变更才会再捞到它们）。
		log.ErrorContext(ctx, "读候选商品失败", "err", err)
		for _, j := range jobs {
			rep.Failed++
			s.retry(ctx, log, &rep, j, err)
		}
		return rep
	}

	// 读不回来的那些：商品在入队与出队之间被下架或软删了。**标成功**，
	// 不是失败 —— 没有东西可做不是故障（db/queries/semantic.sql 上有同一段话）。
	got := map[int64]bool{}
	for _, c := range cands {
		got[c.ProductID] = true
	}
	var done []int64
	for _, pid := range ids {
		if !got[pid] {
			rep.Gone++
			done = append(done, jobOf[pid].ID)
		}
	}

	rep.Judged = len(cands)

	// 判定。这一步不碰数据库、不碰网络，只算两个 sha256。
	plans := make([]indexPlan, 0, len(cands))
	for i, c := range cands {
		pl := s.decide(c, force[c.ProductID])
		pl.idx = i
		plans = append(plans, pl)
	}

	// 批量取向量。**一次调用，不是一件商品一次** —— 语义检索层 §10 第一条。
	// 客户端自己按 DefaultBatchSize 分批（internal/inference 的 Embed），
	// 这里不要绕过它自己切。
	var texts []string
	var needEmbed []int
	for i, p := range plans {
		if p.needEmbed {
			needEmbed = append(needEmbed, i)
			texts = append(texts, p.content)
		}
	}
	vectors := map[int][]float32{}
	var model, version string
	if len(texts) > 0 {
		out, err := s.emb.Embed(ctx, texts)
		if err != nil {
			// 一个向量都没拿到。**不写零向量，也不写任何指纹** ——
			// 这批任务退避重试，水位线不动。
			rep.EngineDown++
			level := slog.LevelError
			if errors.Is(err, inference.ErrUnavailable) {
				// 引擎不可用是会自己好的一类，走降级链、退避重试。
				// 与「引擎回了个不能入库的东西」分开记，后者要人来看。
				level = slog.LevelWarn
			}
			log.Log(ctx, level, "取向量失败，这一批一条向量都不写"+
				"（宁可没有向量，也不要一个零向量 —— 它对每个查询的余弦距离都是 1，"+
				"会出现在每一次检索结果里，而且不报错）",
				"products", len(texts), "err", err)
			// 只需要重写 bigram 串的那些照常做：它们不依赖引擎，
			// 而把它们一起停掉只会让队列在引擎恢复之后还堵着。
			for _, i := range needEmbed {
				plans[i].skipEntirely = true
			}
		} else {
			model, version = out.Model, out.ModelVersion
			for k, i := range needEmbed {
				vectors[i] = out.Vectors[k]
			}
		}
	}

	for _, p := range plans {
		j := jobOf[p.cand.ProductID]
		if p.skipEntirely {
			// 引擎挂了。退避重试，**不标成功** —— 标成功的话这件商品要等到
			// 下一次触发点扫描才会重新入队，而引擎恢复可能就在两秒后。
			s.retry(ctx, log, &rep, j, errors.New("推理引擎不可用，这一批没有取到向量"))
			continue
		}
		if err := s.writeBack(tctx, log, p, vectors[p.idx], model, version, &rep); err != nil {
			s.retry(ctx, log, &rep, j, err)
			continue
		}
		done = append(done, j.ID)
	}

	if err := s.repo.FinishJobs(ctx, done); err != nil {
		// 任务做完了但交不回去。**业务效果已经落库**（写回是自己的事务），
		// 所以这不是数据损失；代价是这些任务会卡在「执行中」直到被回收，
		// 期间占着这家店的在途配额，回收之后会被重做一遍 —— 而重做是幂等的
		// （指纹没变，判定会说什么都不用做）。
		log.ErrorContext(ctx, "任务做完了但标记失败，它们会被回收后重做（幂等）",
			"count", len(done), "err", err)
		rep.Failed++
	}
	return rep
}

// retry 把一条任务按指数退避放回队列，用尽次数则进死信。
func (s *IndexService) retry(ctx context.Context, log *slog.Logger,
	rep *IndexReport, j repository.Job, cause error) {
	err := s.repo.RetryJob(ctx, j.ID, cause.Error())
	switch {
	case err == nil:
		log.DebugContext(ctx, "任务退避重试", "job_id", j.ID,
			"job_key", j.JobKey, "attempts", j.Attempts, "err", cause)
	case errors.Is(err, repository.ErrJobDeadLettered):
		rep.DeadLettered++
		// ERROR 而不是 WARN：§7 说超限进死信队列**并告警**。
		// 一条进了死信的任务意味着这件商品的派生数据永久停在旧版本上，
		// 而触发点不会再捞到它（水位线虽然没推，但队列里那条是死信，
		// 下一轮入队会新建一条 —— 也就是说它其实还会重来，
		// 真正永久卡住的是那种每次都以同样方式失败的商品）。
		log.ErrorContext(ctx, "任务重试次数用尽，已转死信", "job_id", j.ID,
			"job_key", j.JobKey, "err", cause)
	default:
		rep.Failed++
		log.ErrorContext(ctx, "任务退避失败（它会卡在执行中直到被回收）",
			"job_id", j.ID, "err", err, "cause", cause)
	}
}

// indexPlan 是一件商品的判定结果。
type indexPlan struct {
	idx  int
	cand repository.IndexCandidate

	content    string // 送进模型的拼接文本，同时写进 product_text_vectors.content
	searchText string // bigram 串
	embedHash  string
	searchHash string

	needEmbed  bool
	needSearch bool

	// skipEntirely：这一件本轮什么都不做（引擎挂了）。
	skipEntirely bool
}

// decide 是「判定」那一半。**它只看指纹与模型名，不看任何时间戳。**
//
// 时间戳的先后关系已经在 SQL 里当过触发点了；在这里再看一次，等于把
// 「粗但不漏」的那一半当成「准」的那一半用 —— 一次下单就会把全店商品重算。
//
// 指纹经由 internal/understanding 的两个 processor 算，不直接调
// search.ProductText 的方法：那两个 processor 声明了自己关心哪些字段
// （InputFields），而那份声明由 TestFingerprintDependsExactlyOnInputFields
// 逐字段核对。绕过它们等于绕过那条检查。
func (s *IndexService) decide(c repository.IndexCandidate, force bool) indexPlan {
	in := understanding.ProductInput{
		ProductID:    c.ProductID,
		Title:        c.Title,
		Subtitle:     c.Subtitle,
		CategoryName: c.CategoryName,
	}
	p := indexPlan{
		cand:       c,
		content:    s.textEmbedding.Content(in),
		searchText: s.searchText.Text(in),
		embedHash:  s.textEmbedding.Fingerprint(in),
		searchHash: s.searchText.Fingerprint(in),
	}

	switch {
	case force:
		p.needEmbed = true
	case c.VectorModelName == nil:
		// 没有向量行 —— 从没算过。
		p.needEmbed = true
	case *c.VectorModelName != inference.ModelName:
		// 换模型了。这是「要不要重算」的另一半依据（语义检索层 §2.2）：
		// 库里这条向量来自另一个模型，它与新模型算出来的向量不在同一个空间里，
		// 余弦距离算得出来、毫无意义，而且不报错。
		//
		// 注意它只认得**模型名**。同一个模型换版本（重新量化、权重刷新）
		// 这一层看不见 —— model_version 要调一次引擎才知道，而这里
		// 恰恰是在决定要不要调。§2.2 给模型升级的正解是影子表 + 原子切换，
		// 那要新建表、不是原地更新；本轮的人工杠杆是 cmd/keel-index -force。
		p.needEmbed = true
	default:
		p.needEmbed = c.InputHashes[s.textEmbedding.Name()] != p.embedHash
	}

	switch {
	case force:
		p.needSearch = true
	case c.SearchText == nil:
		// 这一列从没写过。00016 刚加上它时全库都是 NULL，而那一刻
		// products.updated_at 一个也没前进。
		p.needSearch = true
	default:
		p.needSearch = c.InputHashes[s.searchText.Name()] != p.searchHash
	}
	return p
}

// writeBack 把一件商品的两份派生数据写回去，**一个事务**。
//
// 事务里第一件事是 LockProductForIndex：判定与写回之间隔着一次 HTTP，
// 这段时间里商家完全可能又改了一次标题。那时写回去的是上一个版本算出的向量，
// 而水位线会被推到当前 —— 这件商品从此不再是候选，库里那条向量永远停在旧标题上。
// 时间戳对不上就整条跳过，留给下一轮。
//
// 返回 nil 表示这条任务可以标成功（**包括竞态那一支** —— 竞态不是失败，
// 重试它没有意义：下一次触发点扫描会按新标题重新入队）。
func (s *IndexService) writeBack(ctx context.Context, log *slog.Logger, p indexPlan,
	vec []float32, model, version string, rep *IndexReport) error {

	embedded, searched := false, false
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		now, err := tx.LockProductForIndex(ctx, p.cand.ProductID)
		if err != nil {
			return err
		}
		if !now.Equal(p.cand.UpdatedAt) {
			return errRacedDuringIndex
		}
		hashes := map[string]string{}
		if p.needEmbed {
			if err := tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID:    p.cand.ProductID,
				Content:      p.content,
				Embedding:    vec,
				ModelName:    model,
				ModelVersion: version,
			}); err != nil {
				return err
			}
			embedded = true
		}
		if p.needSearch {
			if err := tx.SetProductSearchText(ctx, p.cand.ProductID, p.searchText); err != nil {
				return err
			}
			searched = true
		}
		// 两格指纹都记。needEmbed 为假时那一格本来就等于 p.embedHash
		// （decide 就是这么判的），重写一遍是恒等操作；而漏写的话，
		// 「从没算过」与「算过且没变」在 NULL 上长得一模一样。
		hashes[s.textEmbedding.Name()] = p.embedHash
		hashes[s.searchText.Name()] = p.searchHash
		return tx.MarkProductIndexed(ctx, repository.ProductIndexMark{
			ProductID:       p.cand.ProductID,
			Status:          statusPartiallyDone,
			Hashes:          hashes,
			PipelineVersion: search.PipelineVersion,
		})
	})

	switch {
	case err == nil:
		if embedded {
			rep.Embedded++
		}
		if searched {
			rep.SearchTextWritten++
		}
		if !embedded && !searched {
			rep.Skipped++
		}
		return nil
	case errors.Is(err, errRacedDuringIndex), errors.Is(err, repository.ErrProductGoneDuringIndex):
		rep.Raced++
		log.DebugContext(ctx, "商品在判定与写回之间又变了，跳过留给下一轮",
			"product_id", p.cand.ProductID)
		return nil
	default:
		rep.Failed++
		log.ErrorContext(ctx, "派生数据写回失败", "product_id", p.cand.ProductID, "err", err)
		return err
	}
}

// statusPartiallyDone 是 product_understanding.status 的 1（部分完成）。
//
// 不写 2（完成）：本轮只有两个 processor 落地（文本向量、bigram 串），
// 图像向量与属性抽取还没有实现。写 2 会让后台的「未完成」列表
// （00016 的 idx_pu_unfinished，WHERE status IN (0,1,3)）从第一天起就是空的，
// 而那张列表存在的全部意义是看见没做完的东西。
//
// **挂账：它是这一列今天唯一的写入点，所以 status 恒为 1**，那张「未完成」
// 列表因此等价于全表 —— 它今天筛不掉任何东西。3（失败）没有写入点：
// 失败现在记在 jobs 上（attempts / last_error / status = 3 死信），
// 那是队列的账，而 product_understanding.updated_at 是索引触发点的水位线，
// 往它上面写失败会把商品静默踢出候选集（文件头第三节）。
const statusPartiallyDone int16 = 1

var errRacedDuringIndex = errors.New("商品在判定与写回之间又变了")
