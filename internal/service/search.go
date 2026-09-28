package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
)

// 混合检索：双路召回 → RRF 融合 → 业务重排（M3 Task 4 + M5）。
//
// ===========================================================================
// 一、契约写的是四层，这里有三层 —— 缺的那一层必须从响应里看得出来
// ===========================================================================
//
// 契约 /search 的描述是「四层流水线：双路召回 → RRF 融合 → Reranker 精排 →
// 业务重排」。M5 这一轮接上了**业务重排**（语义检索层 §6，逻辑在
// internal/search/business.go），**cross-encoder 精排仍然没有**：推理引擎
// infero 今天只有 /v1/embeddings，没有 /v1/rerank（§10 那条接口还只在纸上），
// 这一层没有东西可调。
//
// 静默少做是这个仓库反复在消灭的东西（券那件事：传了 user_coupon_id 却被忽略
// 等于让用户以为用了券）。检索这里没有「钱算错了」那么刺眼，但性质一样：
// 调用方拿到一份排序，它有没有经过精排、有没有被业务规则降权，
// 从响应里看不出来，而那正是它要据以判断「这个结果为什么长这样」的东西。
//
// 三个地方把这件事说出来：
//
//	· **strategy 回显的是真的跑过的那条流水线的名字**（DefaultStrategy =
//	  "rrf-biz-v1"），不是回显请求里那个字符串。语义检索层 §9.3 要的就是
//	  「strategy_id 写进检索日志、按策略分组对比线上指标」—— 回显请求值的话，
//	  同一个 id 在精排上线前后指的是两条不同的流水线，那份对比就毁了。
//	  业务重排上线也是一次流水线变更，所以名字从 "rrf-v1" 换成了 "rrf-biz-v1"；
//	  "rrf-v1" 仍然可以显式请求（不做业务重排），见 ResolveStrategy。
//	· **explain=true 时 scores 里只出现真的算过的那几项**：vector（向量路跑成了
//	  才有）/ keyword / rrf / business / final；rerank **整个不出现**。
//	  契约里它们是可选字段，「没算」的诚实形状是缺席，不是 0 ——
//	  与下单那边 freight_cents 同一条纪律。
//	· **search_logs.stages 记的是这一次真正跑过的阶段**（迁移 00027 文件头 ①），
//	  降级时少掉的 vector 在那里看得见。
//
// 精排那一笔账仍然挂在 handler/contract_test.go 的 NotYetImplementedStage 上
// （契约里删掉「Reranker 精排」这几个字 → 红），
// handler/search_test.go 的 TestExplainListsExactlyTheStagesThatRan 断言响应里
// 真的没有 rerank（哪天精排上线开始填它 → 红，逼人回来划掉挂账）。
// 业务重排本身也只实现了 §6 的一部分（库存；活动 / 口碑 / 毛利缺席），
// 缺的那几个因子为什么缺，写在 internal/search/business.go 的文件头第二节。
//
// ===========================================================================
// 二、降级链是硬要求（§8：任何一环故障，搜索都必须仍能返回结果）
// ===========================================================================
//
// 具体到 M3 只有一环会故障：推理引擎。它挂了 ⇒ 没有 query embedding ⇒
// 向量那一路整个没有 ⇒ **退化成纯关键词召回，仍然返回结果**。
//
// 三件事要说清楚：
//
//	· internal/inference 的客户端在引擎挂了时给的是**明确的错误**，不是零向量
//	  （那个包的文件头第 ③ 条）。这里不把它吞成空结果 —— 吞掉的话
//	  「引擎挂了」与「这家店真的没有这个东西」在响应里长得一模一样。
//	  它被记成一条 WARN，并且让 recall_source 只剩 keyword。
//	· emb 允许是 nil。这与 NewIndexService「nil 就拒绝构造」**刻意相反**：
//	  索引任务没有引擎就只干一半活而不报错，那是坏的；检索没有引擎仍然是
//	  §8 明文要求的一个合法形态（README 承诺的那条 `docker compose up`
//	  里就没有引擎）。
//	· 反过来，关键词那一路挂了（数据库出错）而向量这一路好着，也照样返回。
//	  两路都挂才报错 —— 那时真的没有结果可给。
//
// ===========================================================================
// 三、两路是并行发起的（§8：query embedding 与关键词召回同时发起，取 max）
// ===========================================================================
//
// 各开各的租户事务，不是共用一个。共用的话，关键词那一路跑完之后事务要**空等**
// 几十毫秒等 embedding 回来（CPU 上单条实测 62 ms），而那段时间里池里的那条
// 连接是被占着的 —— 并发一上来，连接池先于推理引擎成为瓶颈。
// 代价是两路看到的是两个快照：中间有商品上下架时，一路见得到、另一路见不到。
// RRF 对此免疫（它算的是并集上的名次），所以这个代价是可以付的。
//
// ===========================================================================
// 四、召回层**没有相似度阈值**，这是刻意的
// ===========================================================================
//
// 向量那一路返回的是「最近的 N 条」，不是「足够近的那些」。也就是说在一家只有
// 几件商品的店里，搜什么都会把它们全返回，只是顺序不同。
//
// 不加阈值的理由：定「余弦距离小于多少才算相关」需要 §9.1 的离线评测集，
// 而那个东西还不存在。现在拍一个数，就是 §9 开头点名的那件事 ——
// 「没有评估体系的检索优化等于盲调」。而且那个数是**跟着模型走**的：
// 换一次 embedding 模型它就失效，却不会有任何东西报错，只会让某一类查询
// 突然搜不到东西。
//
// 契约里的 Reranker 精排本来就排在召回之后，它才是该做这件事的地方。
// （M5 接上的业务重排**不是**：它只管「该不该卖」，不管「相关不相关」，
// 一件相关度很低的有货商品在业务重排之后仍然是一件相关度很低的商品。）
// handler/search_test.go 的
// TestHybridRecallFindsBothLexicalAndSemanticMatches 把这个代价写成了断言
// （它断言的是「裙子排在咖啡壶前面」，不是「咖啡壶不在结果里」），
// 免得哪天有人把它当成 bug 顺手加一个阈值上去。
//
// ===========================================================================
// 五、延迟：M3 不为 62 ms 的 query embedding 做任何事
// ===========================================================================
//
// §8 给 query embedding 的预算是 15 ms，实测 CPU 单条 62 ms，超了 4 倍。
// M3 计划算过这笔账：本轮不含 Reranker（80 ms 的大头），总链路约 108 ms，
// 目标 P95 < 300 ms，三倍余量。**所以这里不加 query 缓存、不换模型。**
// 要重算这笔账的时点是 Reranker 落地那一轮。
// M5 的业务重排是一次内存里的乘法加一次排序（几百条候选），不改变这笔账。
//
// ===========================================================================
// 六、每次成功的检索写一行 search_logs，写失败不让检索失败
// ===========================================================================
//
// 数据模型 §8「第一天就要埋」：没有这张表，§9 的离线评测集无从采样，
// §9.3 的按策略对比无从谈起。写的是解析后的 strategy、真正跑过的 stages、
// 这一次 query embedding 的模型名与版本、融合后的全部候选（recall_ids）与
// 真正返回的那几条（ranked_ids）、服务端耗时。
//
// 它是**辅助数据**，所以：
//
//	· 写失败只记一条 ERROR（SearchLogFailed 那句），检索照样返回 ——
//	  §8「任何一环故障，搜索都必须仍能返回结果」对日志比对推理引擎更该成立，
//	  日志连结果质量都不影响。
//	· 用自己的事务，不和召回共用：召回那两个事务早已提交，而日志写失败
//	  也不该有机会回滚任何东西。
//	· 上下文脱离请求的取消（context.WithoutCancel，保留租户），再套一个
//	  SearchLogTimeout：客户端断开不该让一次已经算完的检索少一行日志，
//	  而一个卡住的数据库也不该把检索拖过这条上限。
//	· 同步写，不开 goroutine：一次单行 INSERT 在毫秒量级，而异步写意味着
//	  进程退出时丢日志、测试里要等一个看不见的 goroutine。
//
// trace_id 由这里生成并落在这一行上，**只在这一行真的写进去了时**回给客户端
// （SearchResult.TraceID）。它唯一的消费方是 POST /search/events（search_event.go），
// 回一个库里没有的 id 只会让之后每一次回传都 404 —— 所以写失败时它缺席，
// 契约里 trace_id 是可选字段，缺席就是「这次检索没有可回传的落点」。

// SearchRepository 是检索需要的仓储能力。
type SearchRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

const (
	// DefaultSearchSize / MaxSearchSize 与契约里 size 的 default / maximum 一致。
	DefaultSearchSize = 20
	MaxSearchSize     = 100

	// DefaultStrategy 是默认跑的那条流水线的名字：
	// 双路召回 + RRF + 业务重排，没有精排。
	//
	// 不叫 "default"：契约把请求里 strategy 的默认值定成了 "default"，
	// 那是一个**别名**（「不指定就用当时的默认策略」），而回显要的是
	// 「这一次到底跑了哪条」。两者同名的话，精排上线之后 search_logs 里
	// 前后两个月的 "default" 指的是两条不同的流水线，而 §9.3 那张按策略
	// 分组的对比表会把它们当成同一桶。
	//
	// 业务重排上线正是那样一次变更，所以这个名字从 "rrf-v1" 换成了
	// "rrf-biz-v1"，而不是让 "rrf-v1" 从此悄悄多做一层。
	DefaultStrategy = "rrf-biz-v1"

	// StrategyRRFOnly 是 M3 那条流水线：双路召回 + RRF，不做业务重排。
	//
	// 它仍然可以被显式请求，而且**跑的就是它名字说的那条**。留着它是因为
	// §9.3 要的「可切换、可对比」此刻有了第一对可比的东西：业务重排到底让
	// CTR@10 / 搜索→加购率变好还是变坏，只有在两条流水线都还跑得出来时才能答。
	StrategyRRFOnly = "rrf-v1"

	// AliasStrategyDefault 是契约里 strategy 的默认值，解析成 DefaultStrategy。
	AliasStrategyDefault = "default"
)

// 阶段名：search_logs.stages 里记的就是这几个字符串，按执行顺序。
//
// 与 explain 的 scores 键同名（vector / keyword / rrf / business）：
// 同一件事在日志与响应里叫两个名字，按阶段分组查日志的人与读 explain 的人
// 就要各自维护一张对照表。
const (
	StageVector   = "vector"
	StageKeyword  = "keyword"
	StageRRF      = "rrf"
	StageBusiness = "business"
)

// SearchLogTimeout 是写一行检索日志的上限（文件头第六节）。
//
// 一次单行 INSERT 正常在毫秒量级；200 ms 是「数据库明显不对劲了」的线。
// 超过它就放弃这一行、记 ERROR、照常返回 —— 检索结果已经算完了，
// 让用户为一行辅助数据多等是本末倒置。
const SearchLogTimeout = 200 * time.Millisecond

// RecallMultiplier 是每一路要多召回几倍。
//
// 融合之后要交出 size 条，而两路各自的 top-size 重叠时并集会小于 2×size，
// 只召回 size 条的话「两路都命中」的那批会把名额占掉，融合就退化成一次交集。
// 取 3 倍是个朴素的起点（§4 没有给这个数）：它让每一路都有足够多的
// 「只有我捞到了」的候选进到融合里，而代价是数据库多返回 2×size 行 ——
// 在 size ≤ 100 的量级上那是几百行，不值得为它调参。
//
// 真要定这个数，要的是 §9.1 的离线评测集（Recall@50），而它还不存在。
const RecallMultiplier = 3

// SearchConfig 是检索的可调项。
type SearchConfig struct {
	// EmbedTimeout 是 query embedding 这一步的上限。<= 0 用
	// DefaultQueryEmbedTimeout。
	EmbedTimeout time.Duration
}

// DefaultQueryEmbedTimeout 是查询侧给引擎的时间上限里**与长度无关的那一段**。
//
// 250 ms 不是 §8 那个 15 ms 的预算，而是**放弃等待的那条线**：实测 CPU 单条
// **短**查询 62 ms，250 ms 是它的 4 倍，留给一次 GC、一次批处理排队、
// 一次网络抖动。超过它就走降级链交纯关键词结果 —— 那比让用户等到 5 秒
// （inference 客户端的 DefaultTimeout，那是**索引侧**的量级）再看到结果要好得多。
//
// ## 「62 ms 的 4 倍」这句话只对短查询成立
//
// 上一版把这个常数当成整个 embedding 步骤的上限，那是错的：**embedding 的
// 耗时随输入长度线性涨，而一个常数上限不涨**。直接打引擎实测（重复三次稳定）：
//
//	3 字   0.048s      60 字  0.125s     150 字 0.258s
//	210 字 0.424s      600 字 0.980s
//
// 契约给 query 的上限是 maxLength: 200。也就是说在**契约允许长度的 75%**
// （150 字）处，向量那一路就已经必然超时 —— 端到端实测 latency_ms 稳定停在
// 250，返回的是纯关键词结果，而响应是一个正常的 200、strategy 照样回
// "rrf-v1"。用户看不出任何异常，日志里只有一条 WARN。
//
// 所以上限改成跟着长度走：DefaultQueryEmbedTimeout + PerRuneEmbedBudget × 字数。
// 200 字（契约上限）时是 250 + 600 = 850 ms，对着实测的 0.42 s 有两倍余量；
// 短查询仍然正好是 250 ms，与改之前一模一样。
//
// ## 这会让长查询超出 P95 < 300 ms 的目标，这是**刻意**的
//
// 一条 200 字的查询在 CPU 上光 embedding 就要 0.42 s，**无论超时定成多少
// 都进不了 300 ms**。能选的只有两件事：安静地降级（快，但语义那一路整个没了，
// 而调用方看不出来），或者慢一点但把活干完。选后者 —— 静默少做正是这个仓库
// 反复在消灭的东西。
//
// M3 计划里那笔延迟账（62 + 30 + 1 + 15 = 108 ms，三倍余量）同样只在极短
// 查询上成立，那份文档要跟着改。
const DefaultQueryEmbedTimeout = 250 * time.Millisecond

// PerRuneEmbedBudget 是每个字再多给的等待时间。
//
// 实测的斜率约 1.6 ms/字（600 字 0.98 s 与 3 字 0.048 s 之间），取 3 ms/字
// 是它的将近两倍 —— 与 250 ms 对 62 ms 取 4 倍是同一种留法，只是这一段
// 要留的是「这台机器比实测那台慢一点」而不是「一次 GC」。
//
// 它不是配置项：一个能被调小的超时意味着某个部署会在长查询上全面静默降级，
// 而那正是这一条要消灭的东西。
const PerRuneEmbedBudget = 3 * time.Millisecond

// MaxQueryRunes 是查询词的字数上限，**与契约里 query 的 maxLength 相同**。
//
// 契约写了这个上限，而 handler 此前一个字都没校验。不校验的代价不是
// 「多花一点钱」：它与上面那段是同一件事的两半 —— 没有上限的话，
// 「给引擎的时间随长度走」就没有上界，一条 60 KB 的 query（请求体大小闸门
// 之内完全放得下）会让一个公开接口上的 goroutine 占着引擎跑几十秒。
//
// 数的是 rune 不是 byte：OpenAPI 的 maxLength 数的是字符，
// 「连衣裙」在 UTF-8 里是 9 字节 3 个字符，按字节算会在 67 个汉字处就拒绝，
// 而那是契约允许的长度的三分之一。
const MaxQueryRunes = 200

// SearchFilters 是契约 SearchFilters 在业务层的形状。
//
// 它比 repository.SearchFilters 多一个 InStockOnly（阶段 1a 起「只看有货」在召回之后由
// 这一层过滤，数据访问层不再收它），仍然分成两个类型 ——
// 理由与 ProductSummary / repository.Product 那一对相同（见 product.go）：
// 这一个描述「接口答应支持哪些筛选」，那一个描述「数据访问层收什么参数」。
// 让 handler 直接拿 repository 的类型，等于让 HTTP 这一层 import 数据访问层，
// 而 CONTRIBUTING 的硬规矩一说的正是那条边界。
//
// InStockOnly 不是指针：契约给了它 default（false），所以「没传」在契约里是一个
// 有确定值的状态，而不是「未知」。把它做成指针会让每一个调用点都得再决定一次
// 那个默认值是什么 —— 而那个决定只该有一处（handler 的 defaultSearchFilters）。
type SearchFilters struct {
	CategoryID    *int64
	MinPriceCents *int64
	MaxPriceCents *int64
	InStockOnly   bool
}

func (f SearchFilters) toRepo() repository.SearchFilters {
	return repository.SearchFilters{
		CategoryID:    f.CategoryID,
		MinPriceCents: f.MinPriceCents,
		MaxPriceCents: f.MaxPriceCents,
	}
}

// SearchRequest 是一次检索的入参（契约 /search 的请求体在这一层的形状）。
type SearchRequest struct {
	Query    string
	Size     int
	Strategy string
	Explain  bool
	Filters  SearchFilters

	// StoreID 非 nil 表示客户端显式指名了一家门店；nil 走回落链。
	// 解析规则与 GET /products 逐字一致 —— 两处写岔的症状是
	// 「列表里有这件商品，搜不出来」，而那看起来像索引的问题。
	StoreID *int64
}

// SearchHit 是交给 handler 的一条结果。
type SearchHit struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	SalesCount    int32
	Status        int16
	InStock       bool
	// StockUnknown 为真表示这一次没拿到库存（拆分形态下库存服务不可用）：InStock 没有意义，
	// handler 让 in_stock 字段缺席（契约里它是可选的）。
	StockUnknown bool

	// ImageURL 同 ProductSummary.ImageURL：主图地址，没有图为 nil（字段缺席）。
	ImageURL *string

	// PromotionTags / PromoMinPriceCents 同 ProductSummary（promotion_tags.go，列表同一份判据）。
	// 算不出来时两个都空 —— 检索不为活动标签失败（applyPromotions）。
	PromotionTags      []ProductPromotionTag
	PromoMinPriceCents *int64

	// Source 是它被哪一路捞回来的（契约 SearchHit.recall_source）。
	Source search.RecallSource

	// VectorScore 是 1 - 余弦距离，也就是余弦相似度。
	//
	// 交出相似度而不是距离：契约把这个字段叫 scores.vector，而 scores 里
	// 每一项都是「越大越好」（rrf / final 都是）。混一个「越小越好」的进去，
	// 调参的人迟早会按同一个方向读它。只有 Source 含 vector 时有意义。
	VectorScore float64

	// KeywordScore 是 ts_rank_cd。只有 Source 含 keyword 时有意义。
	KeywordScore float64

	// RRFScore 是融合得分。
	RRFScore float64

	// BusinessScore 是业务重排的乘子（internal/search.BusinessFactor），
	// 只有 SearchResult.Stages 含 StageBusiness 时有意义。
	BusinessScore float64

	// FinalScore 是排序真正依据的那个分：跑了业务重排时是 RRF × 业务乘子，
	// 没跑（strategy = rrf-v1）时就是 RRF 分。
	FinalScore float64
}

// SearchResult 是一次检索的全部产出。
type SearchResult struct {
	Items []SearchHit

	// Store 是「本次检索按哪家门店算的」，契约里**必返**（与 GET /products
	// 的同名字段同义）。MatchType = none 时 Items 是空数组 ——
	// 那是「你不在服务范围」，不是「没搜到」。
	Store StoreContext

	// Strategy 是**真的跑过的**那条流水线的标识，不是请求里那个字符串。
	Strategy string

	// Stages 是这一次**真正跑过**的阶段，按执行顺序（Stage* 常量）。
	// explain 据此决定 scores 里出现哪几个键，search_logs.stages 原样记它。
	// 不在服务范围时是空切片 —— 那一次一个阶段都没跑。
	Stages []string

	// Degraded 为真表示向量那一路没跑成（引擎不可用 / 超时 / 没配引擎），
	// 这一次是纯关键词结果。handler 不把它放进响应 —— 契约里没有这个字段，
	// 而加一个字段要改契约，那是一次独立的动作。
	//
	// 所以它的去处只有两处，两处都要真的存在：
	//
	//	· **日志**：下面 Search 里那条 WARN 就是由它驱动的（`if degraded`，
	//	  不是再判一次 vecErr）。这是「搜索质量怎么突然变差了」在系统里
	//	  唯一的痕迹。
	//	· **测试**：handler/search_test.go 的
	//	  TestDegradationIsMarkedOnTheResultAndLeavesAWarnInTheLog 同时读这个
	//	  字段和那条 WARN，并且两个方向都锁：引擎挂了必须有，引擎好着必须没有。
	//
	// 在那条测试之前这个字段一处读者都没有（grep 只有定义、注释、赋值），
	// 那条 WARN 也没有任何断言 —— 整段删掉，全绿。
	Degraded bool

	// TraceID 是这次检索在 search_logs 里那一行的定位键，客户端回传行为时带回来
	// （POST /search/events）。**检索日志没写进去时为空串**：那时库里没有这一行，
	// 回一个 id 出去只会换来之后每一次回传的 404（文件头第六节）。
	TraceID string
}

// SearchService 是混合检索。
type SearchService struct {
	// inv 是库存服务（微服务拆分阶段 1a）：召回之后按这家店批量问一次水位，算 in_stock、
	// 执行 in_stock_only。
	inv  inventory.Service
	repo SearchRepository
	emb  inference.Embedder
	log  *slog.Logger
	cfg  SearchConfig
}

// NewSearchService 建检索服务。
//
// **emb 为 nil 是合法的**，那时这个服务只跑关键词那一路。理由写在文件头第二节：
// 与 NewIndexService 拒绝 nil 刻意相反。
func NewSearchService(r SearchRepository, inv inventory.Service, emb inference.Embedder,
	cfg SearchConfig, log *slog.Logger) *SearchService {
	if log == nil {
		log = slog.Default()
	}
	if cfg.EmbedTimeout <= 0 {
		cfg.EmbedTimeout = DefaultQueryEmbedTimeout
	}
	return &SearchService{repo: r, inv: inv, emb: emb, log: log, cfg: cfg}
}

// ErrEmptyQuery：查询串里切不出任何可检索的词（空串、纯标点、纯空白）。
// handler 把它映射成 422 —— 这不是「没搜到」，是「这串东西搜不了」。
var ErrEmptyQuery = errors.New("查询词里没有可检索的内容")

// ErrQueryTooLong：查询词超过契约给的 maxLength。handler 同样映射成 422。
//
// 校验放在 service 而不是 handler：ErrEmptyQuery 也在这里，两条是同一类
// 「这串东西搜不了」，分开两处的话，下一个调用方（比如将来的 gRPC 面或者
// 一条命令行工具）只会带上其中一条。
var ErrQueryTooLong = errors.New("查询词超过长度上限")

// Search 跑一次混合检索。
func (s *SearchService) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	// 检索日志里的 latency_ms 从这里量起，到排序完成为止（不含写日志本身）。
	// 与响应里的 latency_ms 不是同一个数：那个由 handler 从解请求体之前量起。
	start := time.Now()
	size := clampSearchSize(req.Size)
	strategy := ResolveStrategy(req.Strategy)

	// 长度先于一切：下面 recallByVector 给引擎的时间是按字数算的，
	// 没有这道闸门它就没有上界。
	runes := len([]rune(req.Query))
	if runes > MaxQueryRunes {
		return SearchResult{}, fmt.Errorf("%w：%d 字，上限 %d 字",
			ErrQueryTooLong, runes, MaxQueryRunes)
	}

	tsquery := search.TSQueryOr(req.Query)
	if tsquery == "" {
		return SearchResult{}, fmt.Errorf("%w: %q", ErrEmptyQuery, req.Query)
	}

	// 门店解析先跑一次，**在两路召回之前**，而且只跑一次。
	//
	// 它自己开一个事务，与下面两路各自的事务分开。这里不是「本该在同一个
	// 事务里却图省事」：两路召回本来就跑在两个并行的事务里（§8 要它们同时
	// 发起），所以「三者同一个快照」从一开始就不成立。要紧的是**两路拿到的
	// 是同一个 StoreScope** —— 那决定了它们对「哪些商品存在」的意见一致，
	// 而 RRF 只在两份列表谈论同一批商品时才有意义。
	var (
		scope    repository.StoreScope
		matchTyp MatchType
	)
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		scope, matchTyp, err = scopeIn(ctx, tx, req.StoreID)
		return err
	}); err != nil {
		if errors.Is(err, ErrOutOfServiceArea) {
			// 不在服务范围：空结果 + match_type = none，仍然 200。
			// 不去跑两路召回 —— 没有门店就没有「卖不卖 / 多少钱 / 有没有货」，
			// 而一份按 store_id = 0 算出来的结果是三个都错的。
			//
			// strategy 回显解析后的值，不是请求原文：这一支此前回的是
			// req.Strategy，于是「不在服务范围」的那一次会把客户端随手写的
			// 字符串原样写进回显 —— 与本文件头第一节那条纪律相反。
			res := SearchResult{
				Items: []SearchHit{}, Strategy: strategy, Stages: []string{},
				Store: StoreContext{MatchType: MatchNone},
			}
			// 也记一行：stages 为空、ranked_ids 为空。「不在服务范围」的检索
			// 有多少，本身就是一个该被看见的数（默认门店没配、围栏画漏了）。
			res.TraceID = s.recordSearchLog(ctx, req.Query, res, nil, embedModel{}, start)
			return res, nil
		}
		return SearchResult{}, err
	}

	recall := int32(size * RecallMultiplier)
	if req.Filters.InStockOnly {
		// in_stock_only 从 SQL 里的过滤变成了召回之后的过滤（阶段 1a，见 applyStock）：
		// 召回窗口里缺货的那些会在过滤时掉出去。窗口放大一倍，让「返回的条数少于 size」
		// 只在缺货比例真的很高时才发生。
		recall *= inStockOnlyRecallBoost
	}

	// 两路并行。§8：query embedding 与关键词召回同时发起，取 max 而非 sum。
	var (
		wg              sync.WaitGroup
		vecHits, kwHits []repository.SearchHit
		vecErr, kwErr   error
		model           embedModel
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		vecHits, model, vecErr = s.recallByVector(ctx, scope, req.Query, req.Filters.toRepo(), recall)
	}()
	go func() {
		defer wg.Done()
		kwErr = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
			var err error
			kwHits, err = tx.SearchProductsByKeyword(ctx, scope, tsquery, req.Filters.toRepo(), recall)
			return err
		})
	}()
	wg.Wait()

	degraded := vecErr != nil
	if degraded {
		// 明确记下来，而不是当成「这一路没有结果」。
		//
		// 判的是 degraded 而不是再判一次 vecErr != nil：这条 WARN 是
		// SearchResult.Degraded 唯一的运行期去处，让它们共用同一个条件，
		// 「字段说降级了」与「日志里有一条」就不会各自漂移。
		// 引擎不可用是会自己好的一类（§8 的降级链），所以是 WARN 不是 ERROR；
		// 但它必须出现在日志里 —— 否则「搜索质量怎么突然变差了」这件事
		// 在任何地方都没有痕迹。
		s.log.WarnContext(ctx, "向量召回这一路没跑成，本次退化为纯关键词召回"+
			"（语义检索层 §8 的降级链）。结果仍然返回，但语义相关的商品会少",
			"query", req.Query, "err", vecErr)
	}
	if kwErr != nil {
		s.log.ErrorContext(ctx, "关键词召回这一路没跑成", "query", req.Query, "err", kwErr)
	}
	if vecErr != nil && kwErr != nil {
		// 两路都没了，真的没有结果可给。回关键词那一路的错误：
		// 它是数据库出的问题，比「引擎连不上」更需要人看。
		return SearchResult{}, fmt.Errorf("两路召回都失败，向量路：%v；关键词路：%w",
			vecErr, kwErr)
	}

	// 库存：两路的候选合在一起，按这家店批量问一次（阶段 1a）。in_stock_only 在这里过滤，
	// 排在融合之前 —— 与拆分前「缺货的根本进不了召回」同一个效果：RRF 只在留下来的那批里排名次。
	var stockKnown bool
	vecHits, kwHits, stockKnown = s.applyStock(ctx, scope, vecHits, kwHits, req.Filters.InStockOnly)

	byID := make(map[int64]repository.SearchHit, len(vecHits)+len(kwHits))
	vecIDs := make([]int64, 0, len(vecHits))
	for _, h := range vecHits {
		byID[h.ID] = h
		vecIDs = append(vecIDs, h.ID)
	}
	kwIDs := make([]int64, 0, len(kwHits))
	for _, h := range kwHits {
		// 两路都命中时，两条行里各有一半分数：向量路有 Distance、
		// 关键词路有 Rank。合成一条，别让后写的那条把前一条的分数抹掉。
		if prev, ok := byID[h.ID]; ok {
			h.Distance = prev.Distance
		}
		byID[h.ID] = h
		kwIDs = append(kwIDs, h.ID)
	}

	fused := search.FuseRRF(vecIDs, kwIDs)

	// 这一次真正跑过的阶段。召回那两段按「跑成了」算，不按「发起了」算：
	// 降级时 vector 不在这里，explain 里也就没有 scores.vector ——
	// 一个没跑成的阶段在 explain 里给出 0 分，与「跑了、分数是 0」分不开。
	stages := make([]string, 0, 4)
	if vecErr == nil {
		stages = append(stages, StageVector)
	}
	if kwErr == nil {
		stages = append(stages, StageKeyword)
	}
	stages = append(stages, StageRRF)

	// 业务重排（语义检索层 §6）。作用在融合出来的**全部**候选上、截断到 size
	// **之前** —— 理由写在 internal/search/business.go 的文件头。
	var ranked []search.Ranked
	if strategy == StrategyRRFOnly {
		ranked = make([]search.Ranked, 0, len(fused))
		for _, f := range fused {
			ranked = append(ranked, search.Ranked{Fused: f, Business: 1, Final: f.Score})
		}
	} else {
		signals := make(map[int64]search.BusinessSignals, len(byID))
		for id, row := range byID {
			// 没拿到库存时一律按有货排：缺货下沉是一个乘性惩罚，按「不知道」去罚
			// 等于把整页随机打乱成全员缺货 —— 那不是降级，是瞎排。
			signals[id] = search.BusinessSignals{InStock: row.InStock || !stockKnown}
		}
		var missing int
		ranked, missing = search.RerankByBusiness(fused, signals)
		if missing > 0 {
			// 走不到：融合的 id 全部来自召回行。真走到了是这里的 bug，
			// 按有货处理并记一条，而不是让整次检索失败。
			s.log.ErrorContext(ctx, "业务重排有候选缺信号，已按有货处理", "missing", missing)
		}
		stages = append(stages, StageBusiness)
	}
	if len(ranked) > size {
		ranked = ranked[:size]
	}

	items := make([]SearchHit, 0, len(ranked))
	for _, r := range ranked {
		row := byID[r.ID]
		items = append(items, SearchHit{
			ID: row.ID, Title: row.Title, Subtitle: row.Subtitle,
			MinPriceCents: row.MinPriceCents, MaxPriceCents: row.MaxPriceCents,
			SalesCount: row.SalesCount, Status: row.Status, InStock: row.InStock,
			StockUnknown: !stockKnown,
			ImageURL:     imageURLOf(row.MainImageUploadID),
			Source:       r.Source(),
			// 1 - 余弦距离 = 余弦相似度。只有向量路捞到它时这个数才有意义，
			// 没捞到时 Distance 是零值 0，而 1-0=1 会冒充「完美匹配」——
			// 所以这里按名次判一次，而不是无条件算。
			VectorScore:   vectorScoreOf(r.Fused, row),
			KeywordScore:  row.Rank,
			RRFScore:      r.Score,
			BusinessScore: r.Business,
			FinalScore:    r.Final,
		})
	}
	s.applyPromotions(ctx, scope, items)
	res := SearchResult{
		Items:    items,
		Strategy: strategy,
		Stages:   stages,
		Degraded: degraded,
		Store:    storeContextOf(scope, matchTyp),
	}
	res.TraceID = s.recordSearchLog(ctx, req.Query, res, fused, model, start)
	return res, nil
}

// inStockOnlyRecallBoost 是 in_stock_only 时召回窗口的放大倍数，见 Search 里那一段。
const inStockOnlyRecallBoost = 2

// applyStock 给两路召回的每一条填 InStock，in_stock_only 时滤掉缺货的（微服务拆分阶段 1a）。
//
// in_stock 的判据不变：这件商品任意一个在售 SKU（未软删、status = 1）在**这家门店**的水位 > 0。
// 拆分前它是两条召回查询里各写一遍的 EXISTS（「逐字一致」那条纪律），现在只有这一处实现：
// 先取候选商品的在售 SKU（core），再向库存服务按这家店问一次水位。
//
// ===========================================================================
// in_stock_only 变成召回之后的过滤：代价说清楚
// ===========================================================================
//
// 拆分前缺货的商品根本进不了召回（过滤在 SQL 里，LIMIT 数的是有货的）；现在召回窗口数的
// 是全部，过滤在后。召回窗口里缺货的比例高时，一次检索返回的条数会少于 size ——
// 窗口已经放大了一倍（inStockOnlyRecallBoost），但它不是保证。检索没有分页（只有 size），
// 所以不会出现「第二页重复 / 跳条」，只会是这一页短一些。
//
// ===========================================================================
// 库存服务不可用（只在拆分形态出现）
// ===========================================================================
//
// 检索照常返回：in_stock 是契约里的可选字段，每一条都让它缺席（StockUnknown），业务重排
// 按有货处理；in_stock_only **不过滤**（过滤不了），并记一条 WARN。选「不过滤」而不是
// 「返回空」：空结果会被读成「没有这类商品」，而缺席的 in_stock 至少如实说了「这次不知道」。
// 返回的第三个值为 false 即这种情况。
func (s *SearchService) applyStock(ctx context.Context, scope repository.StoreScope,
	vecHits, kwHits []repository.SearchHit, inStockOnly bool) ([]repository.SearchHit, []repository.SearchHit, bool) {

	ids := make([]int64, 0, len(vecHits)+len(kwHits))
	for _, h := range vecHits {
		ids = append(ids, h.ID)
	}
	for _, h := range kwHits {
		ids = append(ids, h.ID)
	}
	if len(ids) == 0 {
		return vecHits, kwHits, true
	}
	inStock, err := s.productsInStock(ctx, scope, ids)
	if err != nil {
		s.log.WarnContext(ctx, "检索没拿到库存：in_stock 缺席、in_stock_only 不生效，结果照常返回",
			"err", err, "in_stock_only", inStockOnly)
		return vecHits, kwHits, false
	}
	mark := func(hits []repository.SearchHit) []repository.SearchHit {
		out := hits[:0]
		for _, h := range hits {
			h.InStock = inStock[h.ID]
			if inStockOnly && !h.InStock {
				continue
			}
			out = append(out, h)
		}
		return out
	}
	return mark(vecHits), mark(kwHits), true
}

// applyPromotions 给这一页结果填活动标签与活动价，与商品列表同一份判据（promotion_tags.go）。
// 只算截断之后的这一页。任何一步出错都只记一条、这一页不带标签 —— 检索的纪律是
// 「任何一环故障都必须仍能返回结果」（§8）；库存服务不在时 finish 自己降级（单价类报价不生效）。
func (s *SearchService) applyPromotions(ctx context.Context, scope repository.StoreScope, items []SearchHit) {
	if len(items) == 0 {
		return
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	var promo promoTagMaterial
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		promo, err = loadPromotionTags(ctx, tx, scope, ids, time.Now())
		return err
	}); err != nil {
		s.log.WarnContext(ctx, "检索结果取不到活动素材，这一页不带活动标签", "err", err)
		return
	}
	tags, prices, err := promo.finish(ctx, s.inv, true)
	if err != nil {
		s.log.WarnContext(ctx, "检索结果算不出活动标签，这一页不带活动标签", "err", err)
		return
	}
	floors := promo.promoFloors(prices)
	for i := range items {
		it := &items[i]
		it.PromotionTags = tags[it.ID]
		it.PromoMinPriceCents = promoMinPriceOf(floors, it.ID, it.MinPriceCents)
	}
}

// productsInStock 回答「这批商品在这家店有没有货」。数据库错误与库存服务错误都原样返回，
// 由 applyStock 统一降级 —— 检索的纪律是「任何一环故障都必须仍能返回结果」（§8）。
func (s *SearchService) productsInStock(ctx context.Context, scope repository.StoreScope,
	productIDs []int64) (map[int64]bool, error) {
	var skus map[int64][]int64
	if err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		skus, err = tx.OnSaleSKUsOfProducts(ctx, productIDs)
		return err
	}); err != nil {
		return nil, err
	}
	var all []int64
	for _, s := range skus {
		all = append(all, s...)
	}
	levels, err := s.inv.StoreStock(ctx, scope.StoreID, all)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(productIDs))
	for pid, list := range skus {
		for _, id := range list {
			if levels[id].Available > 0 {
				out[pid] = true
				break
			}
		}
	}
	return out, nil
}

// embedModel 是这一次给查询做 embedding 的模型（inference.Result 上的两个字段）。
// 向量路没跑成时是零值，写进日志是两个 NULL。
type embedModel struct{ Name, Version string }

// SearchLogFailed 是写检索日志失败时那条 ERROR 的消息，导出给测试逐字匹配 ——
// 它是「埋点断了」在系统里唯一的痕迹，没有断言盯着的话，哪天被人顺手删掉
// 也不会有任何东西红。
const SearchLogFailed = "检索日志没写进去（search_logs）。检索结果照常返回，但这一次检索在效果评测与策略对比里缺席"

// recordSearchLog 写一行 search_logs，返回那一行的 trace_id。
//
// **不返回错误**：失败只记日志（文件头第六节），返回空串 —— 调用方据此不把
// trace_id 回给客户端。
func (s *SearchService) recordSearchLog(ctx context.Context, query string, res SearchResult,
	fused []search.Fused, model embedModel, start time.Time) string {

	recallIDs := make([]int64, 0, len(fused))
	for _, f := range fused {
		recallIDs = append(recallIDs, f.ID)
	}
	rankedIDs := make([]int64, 0, len(res.Items))
	for _, it := range res.Items {
		rankedIDs = append(rankedIDs, it.ID)
	}
	entry := repository.SearchLog{
		Query:     query,
		RecallIDs: recallIDs,
		RankedIDs: rankedIDs,
		LatencyMs: int32(time.Since(start).Milliseconds()),
		TraceID:   newTraceID(),
		Strategy:  res.Strategy,
		Stages:    res.Stages,
	}
	if model.Name != "" {
		entry.ModelName = &model.Name
	}
	if model.Version != "" {
		entry.ModelVersion = &model.Version
	}

	// 脱离请求的取消、保留租户（context 的值原样带过去），再套自己的上限。
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SearchLogTimeout)
	defer cancel()
	if err := s.repo.WithTenant(lctx, func(tx repository.Tx) error {
		return tx.InsertSearchLog(lctx, entry)
	}); err != nil {
		s.log.ErrorContext(ctx, SearchLogFailed,
			"query", query, "strategy", res.Strategy, "trace_id", entry.TraceID, "err", err)
		return ""
	}
	return entry.TraceID
}

// newTraceID 生成一个 128 位随机的 trace_id（32 个十六进制字符）。
//
// 随机而不是自增：它是全局唯一索引上的键（uk_search_logs_trace），
// 数据模型 §2 那条分界线把它归在「我们自己生成的不可枚举标识」一类 ——
// 它回给客户端之后，一个可枚举的 id 等于让人按序号去回传别人的检索行为。
func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Linux 上读的是 getrandom(2)，失败意味着内核熵源坏了。
		// Go 1.24 起它本身就不返回错误（失败直接让进程崩溃），这里兜的是更早的版本。
		panic(fmt.Sprintf("crypto/rand 失败：%v", err))
	}
	return hex.EncodeToString(b[:])
}

// vectorScoreOf 只在向量路真的命中时给出相似度，否则给 0。
//
// 单独成函数是为了让上面那段注释有个落点：`1 - Distance` 对一条没被向量路
// 捞到的行会算出 1.0，也就是「余弦相似度满分」—— 一个看上去最像正确答案的
// 错误值，而 explain 正是给人拿来判断排序合不合理的地方。
func vectorScoreOf(f search.Fused, row repository.SearchHit) float64 {
	if f.VectorRank == 0 {
		return 0
	}
	return 1 - row.Distance
}

// recallByVector 跑「query embedding → 向量召回」这一段。
//
// 没有引擎（emb == nil）或引擎出错时返回错误，由调用方走降级链。
// **不返回一个空列表** —— 空列表会让「引擎挂了」和「这家店真的没有语义相近的
// 商品」在上层长得一模一样。
func (s *SearchService) recallByVector(ctx context.Context, scope repository.StoreScope, query string,
	f repository.SearchFilters, limit int32) ([]repository.SearchHit, embedModel, error) {

	if s.emb == nil {
		return nil, embedModel{}, fmt.Errorf("%w：没有配置 %s，本进程没有推理引擎客户端",
			inference.ErrUnavailable, inference.EnvEndpoint)
	}

	// 查询侧自己带一个更紧的上限：inference 客户端的 DefaultTimeout 是 5 秒，
	// 那是**索引侧**的量级（批 64 在 CPU 上要 2 秒）。在一个人正盯着的搜索框
	// 前面等 5 秒，比返回一份纯关键词结果糟糕得多。
	//
	// **它跟着输入长度走**，理由与实测数据写在 DefaultQueryEmbedTimeout 上：
	// embedding 的耗时随长度线性涨，一个常数上限会让长查询必然静默降级。
	ectx, cancel := context.WithTimeout(ctx, s.embedTimeoutFor(query))
	defer cancel()

	out, err := s.emb.Embed(ectx, []string{query})
	if err != nil {
		return nil, embedModel{}, err
	}
	if len(out.Vectors) != 1 {
		return nil, embedModel{}, fmt.Errorf("%w：要 1 个向量，拿到 %d 个",
			inference.ErrProtocol, len(out.Vectors))
	}

	var hits []repository.SearchHit
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		hits, err = tx.SearchProductsByVector(ctx, scope, out.Vectors[0], f, limit)
		return err
	})
	if err != nil {
		return nil, embedModel{}, err
	}
	return hits, embedModel{Name: out.Model, Version: out.ModelVersion}, nil
}

// embedTimeoutFor 给这一条查询算出等引擎的上限。
//
// 上界是确定的：query 的字数由 MaxQueryRunes 挡住（契约的 maxLength），
// 所以这个函数的值域是 [EmbedTimeout, EmbedTimeout + 200×PerRuneEmbedBudget]，
// 默认配置下是 250 ms 到 850 ms。没有那道长度闸门的话它就没有上界。
func (s *SearchService) embedTimeoutFor(query string) time.Duration {
	n := len([]rune(query))
	if n > MaxQueryRunes {
		// 正常走不到（Search 已经挡过了）。兜一笔，免得将来多一个调用方
		// 绕过那道闸门时，这里变成一个没有上界的等待。
		n = MaxQueryRunes
	}
	return s.cfg.EmbedTimeout + time.Duration(n)*PerRuneEmbedBudget
}

// clampSearchSize 把 size 收进契约允许的范围。钳制而不是报 400，
// 理由与 clampPaging 那一段完全相同。
func clampSearchSize(size int) int {
	if size < 1 {
		return DefaultSearchSize
	}
	if size > MaxSearchSize {
		return MaxSearchSize
	}
	return size
}

// ResolveStrategy 把请求里的 strategy 解析成真的会跑的那一条。
//
// 认得的只有两条：StrategyRRFOnly（"rrf-v1"，不做业务重排）与
// DefaultStrategy（"rrf-biz-v1"）。其余任何输入 —— 空串、别名 "default"、
// 拼错的、指向一条还不存在的策略的（比如带精排的）—— 都落到 DefaultStrategy。
// **不报 400**：契约里它是可选参数，而客户端能从回显里看出自己没落到想要的
// 那一桶（§9.3 原话：「服务端做分流时，客户端必须知道自己落到了哪一桶，
// 否则埋点对不上」）。
//
// 它导出是为了让这条规则有一个可以被直接测的落点：埋在 Search 里的话，
// 「传了个没人认得的策略会怎样」只能从一次完整检索的响应里反推。
func ResolveStrategy(requested string) string {
	switch strings.TrimSpace(requested) {
	case StrategyRRFOnly:
		return StrategyRRFOnly
	default:
		return DefaultStrategy
	}
}
