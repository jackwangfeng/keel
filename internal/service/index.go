package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
)

// 商品派生数据入库（M3 Task 3）：文本向量 + bigram 关键词串，全量与增量。
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
// 二、定时任务的租户从哪来 —— 照 sweep.go 那一套，不另发明
// ===========================================================================
//
// 这个任务和超时补偿一样跑在任何 HTTP 请求之外：没有 Host（tenant.Resolver
// 用不上），也没有 gid（dtm.TenantContextFromGID 用不上）。答案同样是枚举
// merchants（tenant-root 类，刻意没有 RLS）再一家一家进 WithTenant，
// 论证写在 repository/sweep.go 与 service/sweep.go 的文件头，这里不重复。
//
// 公平调度也照搬：每租户上限 + 每轮总预算 + 轮转起点 + 兜底那一趟。
// 形状与理由见 sweep.go「公平调度」那一段。这里只说一处**不同**：
// 上限的量级不一样。超时补偿的一笔是一个短事务，这里的一件商品要花掉一次
// 模型推理（CPU 上批量 33ms/条，实测记在 compose.inference.yaml 的文件头），
// 所以每租户上限直接取 inference.DefaultBatchSize —— 一轮一家店最多打一次
// /v1/embed，正好是 §10「索引侧批大小 32–64」的上限。
//
// ===========================================================================
// 三、引擎挂了怎么办：一个向量都不写，但别把不用引擎的那部分也拖下水
// ===========================================================================
//
// 客户端出错时一个向量都不返回（internal/inference 的文件头第 ③ 条），
// 这里也不吞：零向量入库之后余弦距离对它恒等于 1，它会出现在**每一次**检索的
// 结果里，而没有任何东西会报错。
//
// 但引擎挂了的那一轮里，那些只需要重写 bigram 串的商品**照常处理**。
// 不这么做的话，一次引擎故障会连带把关键词召回的维护也停掉，而且更糟的是：
// 那些商品的水位线推不上去，于是它们一直占着配额，引擎恢复之后队列还堵着。
//
// 失败的那一批不写 product_understanding.last_error。一次引擎不可用会波及整批，
// 为它写 N 行内容完全相同的 last_error，只是把一次故障放大成一轮写风暴；
// 而重试由下一轮的候选集天然承担 —— 它们的水位线没被推过，下一轮还在。
//
// ### 写库失败那一类也不写，理由不是写风暴
//
// 上面那段只覆盖引擎故障。写回失败（writeBack 的 default: 分支）是一件一件
// 发生的，没有写风暴的问题，而它恰恰是 00016 说「要人来看」的那一类。
// 它照样不写 last_error，理由是另一条，而且更硬：
//
// product_understanding 上挂着 touch_product_understanding_updated_at，
// **任何一次 UPDATE 都会把 updated_at 推到 now()**，而那一列就是触发点的
// 水位线（ListStaleProductsForIndex 比的正是它）。于是「记下这次失败」这个
// 动作本身会把这件商品从候选集里踢出去 —— 它的向量从此永远停在旧文本上，
// 不报任何错。本机实测两行，写在 db/queries/semantic.sql 的 MarkProductIndexed 上。
//
// 所以这一轮的选择是：**写回失败什么都不写，水位线不动，下一轮照常重试**，
// 并把「last_error 恒为 NULL / status 恒为 1 / idx_pu_unfinished 等价于全表」
// 这三件事在三处注释里明确挂账，而不是让它们读起来像已经在工作。
// 真要让 last_error 工作，代价是先把「失败」与「水位线」拆开（多一列
// last_error_at，或者让触发点不看 pu.updated_at），那是一次独立的动作。
//
// 支撑这个选择的性质由 index_test.go 的
// TestWriteBackFailureLeavesTheProductInTheCandidateSet 钉住：写回失败之后
// 水位线一动不动，下一轮把它补上。

// IndexRepository 是这个任务需要的仓储能力。
//
// 与 SweepRepository 一样多一个 ActiveMerchants —— 定时任务拿到租户的唯一入口。
type IndexRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
	ActiveMerchants(ctx context.Context) ([]int64, error)
}

// IndexConfig 是一轮的预算。
type IndexConfig struct {
	// PerTenantCap 每轮每租户至多判定几件商品。<= 0 用 DefaultIndexPerTenantCap。
	PerTenantCap int

	// RoundBudget 每轮总共至多判定几件。<= 0 用 DefaultIndexRoundBudget。
	RoundBudget int

	// Interval 两轮之间的间隔。<= 0 用 DefaultIndexInterval。
	Interval time.Duration
}

const (
	// DefaultIndexPerTenantCap 一轮里单家商户至多判定 64 件。
	//
	// 它刻意等于 inference.DefaultBatchSize：一轮一家店最多打一次 /v1/embed，
	// 正好落在语义检索层 §10「索引侧批大小 32–64」的上限上。
	// 调大它不会让引擎跑得更快，只会让客户端把一次调用拆成两个批 ——
	// 那是同一件事，只是错误的粒度变粗了（一批失败拖累另一批）。
	DefaultIndexPerTenantCap = inference.DefaultBatchSize

	// DefaultIndexRoundBudget 一轮总共至多 512 件。
	//
	// 它是这个任务对引擎的压力上限：512 件 × 33ms/件 ≈ 17 秒的推理，
	// 落在 30 秒一轮的间隔里还有余量。没有总预算的话，「每租户上限」
	// 只是把一轮拉长到没有边界。
	DefaultIndexRoundBudget = 512

	// DefaultIndexInterval 30 秒一轮。
	//
	// 语义检索层 §2.2 那张「更新触发时机」表给「标题/属性/类目变更」的时效是
	// **< 1 分钟**。30 秒一轮 + 一轮的处理时间，落在那个要求里；
	// 60 秒一轮就压线了，一次积压就超。
	DefaultIndexInterval = 30 * time.Second
)

// IndexService 把商品的两份派生数据算出来并写进库。
type IndexService struct {
	repo IndexRepository
	emb  inference.Embedder
	log  *slog.Logger
	cfg  IndexConfig

	// cursor 是轮转起点，理由与 SweepService.cursor 完全一样，见那里。
	cursor uint64
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
	return &IndexService{repo: r, emb: emb, log: log, cfg: cfg}, nil
}

// IndexReport 是一轮（或一次全量）的结果。
//
// Embedded 与 Judged 分开记是这个报告最要紧的一件事：判据的全部意义就是让
// 前者远小于后者。两个数贴着跑，说明判定那一半没起作用，钱在白花，
// 而检索结果看上去完全正常。
type IndexReport struct {
	// Tenants 这一轮走过几家商户。
	Tenants int

	// Judged 判定过几件商品（= 触发点捞回来的数量）。
	Judged int

	// Embedded 真的调引擎重算了几条向量。
	Embedded int

	// SearchTextWritten 真的重写了几条 bigram 串。
	SearchTextWritten int

	// Skipped 判定为「两样都不用动」，只推了水位线。
	Skipped int

	// Raced 判定与写回之间商品又被改了，整条跳过留给下一轮。正常路径。
	Raced int

	// Failed 真的出错了。非零就该有人看日志。
	Failed int

	// EngineDown 这一轮有几家商户的引擎调用失败。它与 Failed 分开：
	// 引擎不可用是一件会自己好的事（走降级、下一轮重试），
	// 而 Failed 里混着写库失败、数据坏了这些要人来看的东西。
	EngineDown int

	// Fallback 这一轮跑过兜底那一趟（去掉每租户上限）。
	Fallback bool
}

func (r IndexReport) add(o IndexReport) IndexReport {
	r.Judged += o.Judged
	r.Embedded += o.Embedded
	r.SearchTextWritten += o.SearchTextWritten
	r.Skipped += o.Skipped
	r.Raced += o.Raced
	r.Failed += o.Failed
	r.EngineDown += o.EngineDown
	return r
}

// Run 按 Interval 一轮一轮地跑，直到 ctx 被取消。第一轮在启动后立刻跑
// （理由同 SweepService.Run：进程刚重启时可能已经积压了很久）。
func (s *IndexService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.runOnce(ctx)
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "派生数据入库任务收到停止信号，退出")
			return
		case <-t.C:
		}
	}
}

func (s *IndexService) runOnce(ctx context.Context) {
	rep, err := s.IndexOnce(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "派生数据入库这一轮没跑起来", "err", err)
		return
	}
	if rep.Judged == 0 {
		s.log.DebugContext(ctx, "派生数据入库这一轮没有待判定的商品", "tenants", rep.Tenants)
		return
	}
	s.log.InfoContext(ctx, "派生数据入库完成一轮",
		"tenants", rep.Tenants, "judged", rep.Judged, "embedded", rep.Embedded,
		"search_text", rep.SearchTextWritten, "skipped", rep.Skipped,
		"raced", rep.Raced, "failed", rep.Failed, "engine_down", rep.EngineDown,
		"fallback", rep.Fallback)
}

// IndexOnce 跑一轮增量。导出是为了让测试能在不等 ticker 的情况下驱动它。
//
// 公平调度的形状与 SweepService.SweepOnce 逐条对应，理由写在 sweep.go。
func (s *IndexService) IndexOnce(ctx context.Context) (IndexReport, error) {
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
		one := s.indexTenant(ctx, merchants[(start+i)%len(merchants)], take, false, 0, false)
		rep = rep.add(one.IndexReport)
		budget -= one.Judged
		if one.Judged >= take {
			capped = true
		}
	}

	// 兜底那一趟：预算没用完、而第一趟有人被上限卡住。两个条件缺一不可，
	// 理由逐字同 sweep.go —— 公平的目的是防饿死，不是让机器闲着。
	if capped && budget > 0 {
		rep.Fallback = true
		for i := 0; i < len(merchants) && budget > 0; i++ {
			one := s.indexTenant(ctx, merchants[(start+i)%len(merchants)], budget, false, 0, false)
			rep = rep.add(one.IndexReport)
			budget -= one.Judged
		}
	}
	return rep, nil
}

// Backfill 把一家商户的在架商品**全部**过一遍（全量），按 id 游标翻页直到跑完。
//
// force = true 时跳过判定，无条件重算 —— 换模型、改拼接模板之后用它。
// 常规的全量（force = false）仍然走判定，所以在一个已经索引过的库上跑它
// 几乎不花钱，它补的是「触发点扫不到的存量」：00016 刚落地时全库
// search_text 都是 NULL、向量表是空的，而那一刻没有任何 products.updated_at 前进。
//
// 它不是定时任务，是 cmd/keel-index 调的那条路。
func (s *IndexService) Backfill(ctx context.Context, merchantID int64, force bool) (IndexReport, error) {
	rep := IndexReport{Tenants: 1}
	after := int64(0)
	for {
		one := s.indexTenant(ctx, merchantID, s.cfg.PerTenantCap, true, after, force)
		rep = rep.add(one.IndexReport)
		if one.Judged == 0 {
			return rep, nil
		}
		if one.lastID <= after {
			// 游标没前进。全量那条查询是 `id > @after_id ORDER BY id`，
			// 正常情况下最后一行的 id 一定比游标大；不大就说明查询被改坏了，
			// 而症状会是一个永远跑不完的全量任务，安静地一直打引擎。
			return rep, fmt.Errorf("全量翻页的游标没有前进（%d → %d），"+
				"ListProductsForIndex 的 ORDER BY 或 WHERE 被改过了", after, one.lastID)
		}
		after = one.lastID
	}
}

// indexTenant 处理一家商户至多 limit 件商品。
//
// full = true 走全量那条查询（按 id 游标），afterID 是游标；
// full = false 走触发点那条（按 updated_at 排序），afterID 无意义。
func (s *IndexService) indexTenant(ctx context.Context, merchantID int64,
	limit int, full bool, afterID int64, force bool) tenantResult {
	var res tenantResult
	if limit <= 0 {
		return res
	}
	// 租户上下文在这里产生，也只在这里产生 —— 与 sweep.go 同一个理由，
	// 登记在 internal/service/tenant_context_test.go 的 tenantContextAllowed 里。
	tctx := tenant.NewContext(ctx, merchantID)
	log := s.log.With("merchant_id", merchantID)

	var cands []repository.IndexCandidate
	if err := s.repo.WithTenant(tctx, func(tx repository.Tx) error {
		var err error
		if full {
			cands, err = tx.ListProductsForIndex(ctx, afterID, int32(limit))
		} else {
			cands, err = tx.ListStaleProductsForIndex(ctx, int32(limit))
		}
		return err
	}); err != nil {
		log.ErrorContext(ctx, "捞待判定商品失败", "err", err)
		res.Failed++
		return res
	}
	if len(cands) == 0 {
		return res
	}
	res.Judged = len(cands)
	res.lastID = cands[len(cands)-1].ProductID

	// 判定。这一步不碰数据库、不碰网络，只算两个 sha256。
	plans := make([]indexPlan, 0, len(cands))
	for i, c := range cands {
		pl := decide(c, force, s.emb.ModelName())
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
			// 这批商品的水位线不推，下一轮还在候选里。
			res.EngineDown++
			level := slog.LevelError
			if errors.Is(err, inference.ErrUnavailable) {
				// 引擎不可用是会自己好的一类，走降级链、下一轮重试。
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
		if p.skipEntirely {
			continue
		}
		s.writeBack(tctx, log, p, vectors[p.idx], model, version, &res)
	}
	return res
}

// tenantResult 是一家商户这一段的结果。它就是 IndexReport 加一个游标。
type tenantResult struct {
	IndexReport
	lastID int64
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
func decide(c repository.IndexCandidate, force bool, modelName string) indexPlan {
	t := search.ProductText{
		Title:        c.Title,
		Subtitle:     c.Subtitle,
		CategoryName: c.CategoryName,
	}
	p := indexPlan{
		cand:       c,
		content:    t.EmbedContent(),
		searchText: t.SearchText(),
		embedHash:  t.EmbedFingerprint(),
		searchHash: t.SearchTextFingerprint(),
	}

	switch {
	case force:
		p.needEmbed = true
	case c.VectorModelName == nil:
		// 没有向量行 —— 从没算过。
		p.needEmbed = true
	case *c.VectorModelName != modelName:
		// 换模型了。这是「要不要重算」的另一半依据（语义检索层 §2.2）：
		// 库里这条向量来自另一个模型，它与新模型算出来的向量不在同一个空间里，
		// 余弦距离算得出来、毫无意义，而且不报错。
		//
		// **换引擎方言也走这一条，而且这是它唯一的自愈路径。** 两条腿
		// （infero/Qwen3-Embedding-0 与 keel-python/bge-m3）维度都是 1024，
		// 所以 Postgres 收得下彼此的向量，一个字都不会报。把 KEEL_EMBED_DIALECT
		// 从一条改成另一条之后，库里存量行的 model_name 与这里的 modelName
		// 对不上，于是逐轮被判成待重算 —— 要立刻切干净就跑一次
		// `keel-index -force`（compose.infero.yaml 文件头「换腿要重算全库」）。
		//
		// modelName 由**引擎客户端自己**报（inference.Embedder.ModelName），
		// 不是另配一份。另配一份会漂移，而漂移的形态是这条判据永远说
		// 「没过期」—— 全库停在上一条腿算的向量上，检索照常返回结果。
		//
		// 注意它只认得**模型名**。同一个模型换版本（重新量化、权重刷新）
		// 这一层看不见 —— model_version 要调一次引擎才知道，而这里
		// 恰恰是在决定要不要调。§2.2 给模型升级的正解是影子表 + 原子切换，
		// 那要新建表、不是原地更新；本轮的人工杠杆是 cmd/keel-index -force。
		p.needEmbed = true
	default:
		p.needEmbed = c.InputHashes[search.ProcessorTextEmbedding] != p.embedHash
	}

	switch {
	case force:
		p.needSearch = true
	case c.SearchText == nil:
		// 这一列从没写过。00016 刚加上它时全库都是 NULL，而那一刻
		// products.updated_at 一个也没前进。
		p.needSearch = true
	default:
		p.needSearch = c.InputHashes[search.ProcessorSearchText] != p.searchHash
	}
	return p
}

// writeBack 把一件商品的两份派生数据写回去，**一个事务**。
//
// 事务里第一件事是 LockProductForIndex：判定与写回之间隔着一次 HTTP，
// 这段时间里商家完全可能又改了一次标题。那时写回去的是上一个版本算出的向量，
// 而水位线会被推到当前 —— 这件商品从此不再是候选，库里那条向量永远停在旧标题上。
// 时间戳对不上就整条跳过，留给下一轮。
func (s *IndexService) writeBack(ctx context.Context, log *slog.Logger, p indexPlan,
	vec []float32, model, version string, res *tenantResult) {

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
		hashes[search.ProcessorTextEmbedding] = p.embedHash
		hashes[search.ProcessorSearchText] = p.searchHash
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
			res.Embedded++
		}
		if searched {
			res.SearchTextWritten++
		}
		if !embedded && !searched {
			res.Skipped++
		}
	case errors.Is(err, errRacedDuringIndex), errors.Is(err, repository.ErrProductGoneDuringIndex):
		res.Raced++
		log.DebugContext(ctx, "商品在判定与写回之间又变了，跳过留给下一轮",
			"product_id", p.cand.ProductID)
	default:
		res.Failed++
		log.ErrorContext(ctx, "派生数据写回失败", "product_id", p.cand.ProductID, "err", err)
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
// 列表因此等价于全表 —— 它今天筛不掉任何东西。3（失败）没有写入点，
// 理由见上面文件头第三节那段。这两件事写出来，免得 00016 那条索引
// 与这个常量的注释读起来像是已经在分类。
const statusPartiallyDone int16 = 1

var errRacedDuringIndex = errors.New("商品在判定与写回之间又变了")
