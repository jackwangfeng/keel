// Package inference 是 Keel 与推理引擎之间那个唯一的耦合点（语义检索层 §10）。
//
// 它只做一件事：把一批文本换成一批**可以直接写进 product_text_vectors 的**向量。
// 「可以直接写进」这几个字是这个包的全部重量所在，它意味着三件事都已经核过：
//
//	① 维度是 1024。数据模型 §8 把它写在列类型上（vector(1024)），
//	   而 pgvector 的维度是列级固定的——换模型往往同时换维度，于是
//	   「换模型」= 新建表 + 双写 + 原子切换（语义检索层 §2.2 的影子表方案）。
//	② 每个向量的 L2 范数是 1。§2.3 写明了这件事的性质：配合 vector_cosine_ops
//	   写入未归一化的向量**不会报错，只会悄悄拉低召回质量**。所以这里不相信
//	   请求里那个 normalize: true 传到了——它自己算一遍。
//	③ 出错时一个向量都不返回。一个零向量写进库之后余弦距离对它恒等于 1，
//	   它会出现在**每一次**检索的结果里；而没有任何东西会报错。
//	   宁可让调用方拿到一个明确的错误去走降级链（§8），也不要一个能入库的坏值。
//
// 三条都是「不会报错的错误」。这个包存在的理由就是把它们变成会报错的错误。
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"time"
)

// Dim 是数据模型 §8 的 `embedding vector(1024)` 在 Go 侧的复述。它是**闸门**，
// 不是配置项：引擎返回别的维度时这个包拒收，而不是把它当成一次成功的调用。
//
// **它刻意不在下面那张方言表里。** pgvector 的维度写在**列类型**上，而列类型
// 只有一份：两条腿（infero / keel-python）写的是同一张 product_text_vectors。
// 也就是说 dim 回答的不是「这个引擎怎么说话」，是「这张表能收什么」——
// 把它做成方言的一格，等于承认「换个方言就能换维度」，而那件事的真实代价是
// 新建表 + 双写 + 原子切换（语义检索层 §2.2 的影子表方案），不是改一行配置。
//
// 今天两条腿都是 1024 维，而那是它们能共存的**前提**，不是巧合：BGE-M3 原生
// 1024，Qwen3-Embedding-0.6B 也原生 1024 —— 换引擎那轮免掉的正是上面那一步，
// 是整次切换里唯一真正贵的东西。哪天要接一条不是 1024 维的腿，红的必须是
// validate 里那条 dim 断言（「引擎报 dim=N，而列是 vector(1024)」），
// 而不是一个能安静配出来的值。
const Dim = 1024

// ---------------------------------------------------------------------------
// 引擎方言
// ---------------------------------------------------------------------------

// Dialect 是「同一份请求体，两个引擎说法不一样」的那三样东西。
//
// 请求体与响应体的**形状**两条腿逐字段一致
// （`{model, texts, normalize}` → `{embeddings, dim, model, model_version}`），
// keel-integration.md 早就写明「路径名可以谈，请求体形状不能谈」。所以方言里
// 只有三格，而且都是字符串：
//
//	EmbedPath        打哪条路径
//	ModelName        请求里写哪个 model、响应里必须回哪个 model
//	PoolingSentinel  每条文本尾部补什么（空串 = 什么都不补）
//
// ## 为什么是「两个具名 profile」，不是「路径/模型名/哨兵各一个环境变量」
//
// 被否决的方案是 KEEL_EMBED_PATH / KEEL_EMBED_MODEL / KEEL_EMBED_SENTINEL
// 三个各自独立的变量。它更灵活，而这里刻意不要那种灵活，两条理由：
//
//   - **这三格不是互相独立的。** 它们是「某个引擎跑某个 checkpoint」这**一件**
//     事的三个侧面：路径由引擎决定（infero 是 OpenAI 风格的 /v1/embeddings，
//     Python 那条是 §10 原文的 /v1/embed），模型名由 checkpoint 决定，而哨兵
//     补不补取决于「这个 checkpoint 怎么池化」撞上「这个引擎的 tokenizer 执不
//     执行 post_processor」——它是**两者的交叉项**，单独配它没有意义。
//     三个变量能配出 2×2×2 种组合，其中只有 2 种对应真实存在的部署，另外 6 种
//     全是**不会报错的错配**。举最容易犯的那个：给 BGE-M3 补上 `<|endoftext|>`。
//     那串字符在 XLM-R 的词表里就是几个普通 token，于是每条文本尾部多了一段
//     噪声 —— 维度对、L2 范数是 1、model 名对、HTTP 200，这个包的每一道闸门
//     都是绿的，只有召回质量安静地烂掉。这正是这个包存在的理由要防的那类错误。
//   - **代价是「加一条腿要改一次代码」，而那笔账划算。** 加一条腿本来就得有人
//     读一遍它的 tokenizer 配置、跑一次语义判据（SemanticMargin 对
//     MinSemanticMargin）。让这件事必须落成一次带注释的提交，好过让它变成某台
//     机器上一行没人解释过的环境变量 —— 而那一行出错时不报错。
//
// 留下来的那份灵活在另一头：方言名是一个**开放的字符串**，配错了在**启动期**
// 就报错并把已知的名字列出来（LookupDialect），而不是在运行期打到一个 404 上。
type Dialect struct {
	// Name 是配置里写的那个名字，也就是 EnvDialect 的取值。
	Name string

	// EmbedPath 是引擎上那条批量 embedding 接口的路径。
	EmbedPath string

	// ModelName 有两个用途，而且**必须是同一个字符串**：请求里的 model 字段，
	// 以及 product_text_vectors.model_name 里最终落下去的值。
	//
	// 后一个用途决定了这里该写什么：那一列回答的是「这批向量是哪个模型算的、
	// 要不要重算」，所以它必须与引擎**真的说出口**的那个字符串逐字相等，
	// 而不是我们希望它说的那个。写一个更好看的名字进来，validate 那道闸门
	// 当场就红（引擎报的是别的），而库里记的还是引擎说的那个。
	ModelName string

	// PoolingSentinel 是每条文本尾部要补的 token。**空串表示不补**，而且
	// 空串是一个有内容的答案，不是「还没填」：见 keel-python 那一条的注释。
	PoolingSentinel string
}

// 已知的方言名。它们是 EnvDialect 的合法取值。
const (
	// DialectInfero 是自研推理引擎 infero（CUDA / Metal，**没有 CPU 后端**），
	// 跑 Qwen/Qwen3-Embedding-0.6B 的 safetensors checkpoint。
	// 起法见 scripts/infero-up.sh，叠加层见 compose.infero.yaml。
	DialectInfero = "infero"

	// DialectKeelPython 是本仓库 services/inference/ 那个 Python 服务
	// （FastAPI + sentence-transformers + torch CPU 轮子），跑 BAAI/bge-m3。
	// 叠加层见 compose.inference.yaml。**它是无 GPU 部署唯一的语义检索路径**
	// （架构 §6 形态 A 的「无 GPU 时降级为小模型 CPU 推理」）。
	DialectKeelPython = "keel-python"
)

// dialects 是那张表。不导出，取用走 LookupDialect / MustDialect ——
// 一张导出的 map 是可写的，而「运行期被人改过的方言」这种事不会报错。
var dialects = map[string]Dialect{

	// -----------------------------------------------------------------------
	// infero + Qwen3-Embedding-0.6B（GPU）
	// -----------------------------------------------------------------------
	DialectInfero: {
		Name: DialectInfero,

		// OpenAI 风格。语义检索层 §10 原本把这条路径写成 `/v1/embed`，
		// 那是下面 keel-python 那条腿的路径 —— 两者的请求体逐字段一致，
		// 所以在这一层，换引擎真的只是换一个字符串。
		EmbedPath: "/v1/embeddings",

		// **这个名字看着像被截断了，因为它确实是。**
		//
		// infero 的 model id 取自 `--model` 那个路径的 `Path::file_stem()`
		// （crates/server/src/engine.rs 的 derive_model_id），而 checkpoint
		// 目录叫 `Qwen3-Embedding-0.6B` —— `file_stem` 在**最后一个点**处切开，
		// 于是 `.6B` 被当成扩展名丢掉了。
		//
		// 这里照抄引擎真的说出口的那个字符串。infero 哪天把 file_stem 改对了，
		// validate 那道闸门会**立刻变红**并指着这一行 —— 那是对的：那一天
		// model_name 确实变了，库里的存量行确实需要重算
		// （service/index.go 的 decide 认的就是这个字符串）。
		ModelName: "Qwen3-Embedding-0",

		// 池化哨兵。**不补的话语义会塌掉，而且塌得悄无声息。**
		//
		// Qwen3-Embedding 的池化方式是 last-token pooling：整条文本的向量取自
		// **最后一个 token 位置**的 hidden state。而它的 checkpoint 自带一条
		// tokenizer 后处理规则（tokenizer.json 的 `post_processor` →
		// `TemplateProcessing`），给每条序列尾部追加一个 `<|endoftext|>`(151643)。
		// 也就是说官方配方里「最后一个 token」指的是那个 EOS，不是正文末字。
		//
		// **infero 今天不执行这条规则。** 它的 tokenizer 整个没有 post_processor
		// 的概念（crates/tokenizer/src/lib.rs 的 encode 只做 BPE + 显式特殊
		// token 解析），于是池化取到了正文末字那一行。本机实测，Keel 自己那三条
		// 探针文本（SemanticProbeTexts）：
		//
		//	不补哨兵   近近 0.3380  近远 0.2419  margin +0.0960  ← 低于 MinSemanticMargin
		//	补上哨兵   近近 0.4802  近远 0.1884  margin +0.2917  ← 过线，且优于 BGE-M3 的 +0.19
		//
		// 注意失败的形态：维度对、L2 范数对到小数点后 9 位、model 名对、HTTP 200。
		// 形状断言一条都不会红，**只有 margin 这一条抓得住**。这正是
		// keel-integration.md 里「哈希伪引擎 margin −0.05」那条判据存在的理由。
		//
		// 另外两个被排除掉的猜想，都是实测排除的，不是推理排除的：
		//   - **不是 instruction 前缀的事。** Qwen3-Embedding 的 model card 给
		//     查询侧配了 `Instruct: ...\nQuery:` 前缀。只加前缀不补哨兵，
		//     margin +0.0958 —— 和什么都不做的 +0.0960 在噪声里没有区别。
		//     前缀又加又补哨兵反而更低（+0.2338），因为这三条探针是「文档 vs
		//     文档」的对称比较，给两边都套上查询前缀本来就不是它的用法。
		//   - **不是模型不行。** 同一个模型补上哨兵就是 +0.2917。
		//
		// **为什么这条补丁落在客户端，而不是等 infero 修。** 这是 infero 的 bug，
		// 正确的修法在引擎那边（让 tokenizer 执行 checkpoint 自己的
		// post_processor），已经如实报上去了。但在它修好之前，这里是唯一能让
		// product_text_vectors 里的向量真的有语义的地方。
		//
		// 删除条件：infero 的 tokenizer 开始执行 post_processor 之后，这里会变成
		// 追加两个 EOS。那天把这一格改回空串并重跑一次 margin。
		PoolingSentinel: "<|endoftext|>",
	},

	// -----------------------------------------------------------------------
	// services/inference/ + BGE-M3（CPU，无 GPU 部署唯一的语义检索路径）
	// -----------------------------------------------------------------------
	DialectKeelPython: {
		Name: DialectKeelPython,

		// 语义检索层 §10 的原文路径。
		EmbedPath: "/v1/embed",

		// services/inference/app.py 里的 MODEL_NAME，它同时是那个服务**当场拒绝**
		// 的依据：请求里的 model 与它对不上就回 400（理由写在那个文件的模块头，
		// 与这里同构 —— 1024 维但来自别的模型的向量混进同一张表，余弦距离照算
		// 不误，没有任何东西会报错）。
		ModelName: "bge-m3",

		// **空串是答案，不是空白。**
		//
		// 补哨兵这件事解决的是「last-token pooling 撞上不执行 post_processor 的
		// tokenizer」，而这条腿两个前提都不成立：
		//
		//   - BGE-M3 用的是 **CLS 池化**（checkpoint 的 1_Pooling/config.json 里
		//     `pooling_mode_cls_token: true`）。向量取自序列**第一个**位置，
		//     往尾巴上补什么都不改变它取到的那一行。
		//   - 这条腿的 tokenizer 由 sentence-transformers / transformers 自己跑，
		//     该加的特殊 token（XLM-R 的 `<s>` / `</s>`）它自己会加。
		//
		// 而**补错了是有代价的**，所以这一格必须是空串而不是「随便填个一样的」：
		// `<|endoftext|>` 不在 XLM-R 的词表里，它会被切成几个普通 subword 跟在
		// 正文后面 —— 相当于给每一条商品文本尾部拼上一段固定噪声。维度对、
		// 范数是 1、model 名对、HTTP 200，这个包的每一道闸门都绿，只有召回
		// 质量安静地掉一截。这就是上面否决「三个独立环境变量」的那个例子。
		PoolingSentinel: "",
	},
}

// EnvDialect 是引擎方言。**没有默认值，空着就拒绝构造。**
//
// 与 EnvEndpoint 同一条纪律，而且理由比那一条更硬一点：地址配错了是
// connection refused（吵，但看得见），方言配错了是 404 —— 而配**漏**了要是
// 有默认值，症状会是「向量算出来了、入库了、检索照常返回结果，只是结果和搜的
// 词无关」。两条腿今天真实存在、真实互斥，谁也不比谁更「默认」：
// 有 GPU 的部署是 infero，没有 GPU 的部署是 keel-python。让代码替某台机器
// 猜一个，等于让猜错的那一半安静地跑坏。
//
// 所以：配了 EnvEndpoint 却没配这个，是**启动期硬失败**
// （internal/app 的 Run 靠 ErrDialectUnset / ErrEndpointUnset 分开这两件事），
// 不是运行期发现路径 404。
const EnvDialect = "KEEL_EMBED_DIALECT"

// DialectNames 返回已知方言名，字典序。只用于错误信息与文档。
func DialectNames() []string {
	out := make([]string, 0, len(dialects))
	for name := range dialects {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LookupDialect 按名字取方言。取不到时的错误里带着已知的名字 ——
// 一个「配错了」的部署要在启动期就看见自己该写什么。
func LookupDialect(name string) (Dialect, error) {
	if name == "" {
		return Dialect{}, fmt.Errorf("%w：没有配置 %s。配了 %s 却没说清对面是哪种引擎，"+
			"就只能靠猜，而猜错了不报错——路径猜错是 404（还算吵），哨兵猜错是"+
			"「向量算出来了、入库了、检索照常返回结果，只是结果和搜的词无关」。"+
			"已知的方言：%v（有 GPU 用 %s，没有 GPU 用 %s）",
			ErrDialectUnset, EnvDialect, EnvEndpoint, DialectNames(),
			DialectInfero, DialectKeelPython)
	}
	d, ok := dialects[name]
	if !ok {
		return Dialect{}, fmt.Errorf("%w：不认识的引擎方言 %q。已知的方言：%v",
			ErrRejected, name, DialectNames())
	}
	return d, nil
}

// MustDialect 是 LookupDialect 的 panic 版本，给测试与包级初始化用。
// 生产路径走 New / FromEnv，那条路上配错方言是一个 error，不是 panic。
func MustDialect(name string) Dialect {
	d, err := LookupDialect(name)
	if err != nil {
		panic(err)
	}
	return d
}

// NormTolerance 是 L2 范数允许偏离 1 的幅度。
//
// 1e-3 是给 float32 往返（引擎按 float64 序列化 JSON、这里收成 float32）
// 留的余量，不是给「差不多归一化了」留的。真的没归一化时范数是几十上百，
// 差着几个数量级，不存在「调松一点就过了」的中间地带。
const NormTolerance = 1e-3

// DefaultBatchSize 取 §10 的上限：「索引侧批大小 32–64」。
//
// 它是**批**的大小，不是并发度。这个包不会把 N 条文本拆成 N 次请求——
// 那正是 §10 第一条「批量接口，禁止循环单条调用」要禁的东西：
// 一条 64 字的商品标题，模型算一次要几十毫秒，而一次 HTTP 往返的固定开销
// （连接、序列化、模型侧的 tokenize 批处理准备）在循环单条时要乘 64 遍。
const DefaultBatchSize = 64

// EnvEndpoint 是引擎地址。**没有默认值，空着就拒绝构造。**
//
// 这条纪律是从 KEEL_DTM_DSN 那里照搬的（见 internal/app 的 EnvDTMDSN），
// 理由同构：一个「反正能跑起来」的默认值（比如 http://localhost:8000）
// 会让每一个忘了配它的部署安静地拿到坏结局——这里的坏结局是每次索引都
// connection refused，而索引是异步的，没有人在等它的返回码。
// 商品入库了、搜不到，症状出现在几小时后的「怎么搜不到新品」。
const EnvEndpoint = "KEEL_EMBED_ENDPOINT"

// 三类错误。分三类不是为了好看，是因为调用方对它们的处置不同：
//
//   - ErrUnavailable：引擎挂了/超时了。走 §8 的降级链（纯关键词召回），
//     并且**可以重试**。
//   - ErrProtocol：引擎回了东西，但那东西不能写进库（维度不对、没归一化、
//     条数对不上）。重试没用，这是引擎和 DDL 对不上，要人来看。
//   - ErrRejected：引擎明确拒绝了这次请求（批太大、文本为空、模型名不对）。
//     重试没用，是调用方的问题。
var (
	ErrUnavailable = errors.New("推理引擎不可用")
	ErrProtocol    = errors.New("推理引擎的响应不能写进库")
	ErrRejected    = errors.New("推理引擎拒绝了这次请求")
)

// 构造期的两类「没配」。它们与上面那三类不同族（那三类是**调用**的结果），
// 而分成两个哨兵不是为了好看，是因为**装配方对它们的处置不同**
// （internal/app 的 Run 里那一段）：
//
//   - ErrEndpointUnset：整条语义检索的腿没开。那是一种**受支持的部署** ——
//     /search 走 §8 的纯关键词降级链，README「还没在盒子里的」一节记着它。
//     所以装配方打一条 WARN 继续起，不拒绝启动。
//   - ErrDialectUnset：地址配了、方言没配。这是一次**配漏了**，没有任何部署
//     形态对应它 —— 继续起的话，索引侧每一轮都打到一个 404 上，而索引是异步的，
//     没有人在等它的返回码。所以装配方在这里拒绝启动，与 KEEL_DTM_DSN 同类。
var (
	ErrEndpointUnset = errors.New("没有配置推理引擎地址")
	ErrDialectUnset  = errors.New("没有配置推理引擎方言")
)

// Result 是一次 Embed 的全部产出。
//
// Model 与 ModelVersion 跟着向量一起出来，而不是由调用方从配置里拿一份——
// §10 要求「模型名与版本随响应返回」，而它们的去处是
// product_text_vectors.model_name / model_version，那两列回答的是
// 「这批向量是哪个模型算的、要不要重算」。从配置里抄一份的话，
// 引擎换了模型而配置没跟，库里记的版本就是假的，而重算判定从此失灵。
type Result struct {
	Vectors      [][]float32
	Model        string
	ModelVersion string
}

// Embedder 是索引侧与查询侧共同依赖的最小接口。
type Embedder interface {
	Embed(ctx context.Context, texts []string) (*Result, error)

	// ModelName 是这个 embedder 会往 product_text_vectors.model_name 里
	// 落的那个字符串。
	//
	// **它为什么必须在接口上，而不是让调用方从配置里再抄一份。**
	// 索引侧的重算判定（service/index.go 的 decide）要回答「库里这条向量是不是
	// 另一个模型算的」，而它是在**调用引擎之前**回答的 —— 那时 Result.Model
	// 还不存在。于是这个字符串只有两个来源：问 embedder，或者装配时再配一遍。
	// 后者会漂移：换了方言而那份配置没跟上，全库的向量会被判成「还是新鲜的」，
	// 从此永远不重算，而且不报错。
	//
	// 等价地说：换方言 = 换模型 = 库里的存量行全部过期
	// （compose.infero.yaml 文件头那条「换腿要重算全库」说的就是它）。
	// 让这件事从同一个对象上读出来，两边就不可能说出不同的话。
	ModelName() string
}

// Config 是一个客户端的全部配置。
type Config struct {
	// Endpoint 形如 http://inference:8000。空着时 New 报错，见 EnvEndpoint。
	Endpoint string
	// Dialect 是方言名（DialectInfero / DialectKeelPython）。
	// **空着时 New 报错**，见 EnvDialect —— 与 Endpoint 同一条纪律。
	Dialect string
	// Timeout 是**单次请求**的上限。0 用 DefaultTimeout。
	Timeout time.Duration
	// BatchSize 是一次请求最多送几条。0 用 DefaultBatchSize。
	BatchSize int
	// HTTPClient 可注入，测试用。nil 时自己建一个。
	HTTPClient *http.Client
}

// DefaultTimeout 是单次请求的默认上限。
//
// 5 秒是**索引侧**的量级：批 64 条中文短文本在无 GPU 的机器上就是秒级
// （实测数字记在 compose.inference.yaml 的文件头）。查询侧的预算是 15 ms（§8），
// 那条路径应当自己传一个更紧的 context——Embed 取 ctx 与这个值里更早的那个。
const DefaultTimeout = 5 * time.Second

// Client 是 EmbedPath 的客户端。零值不可用，走 New。
type Client struct {
	endpoint  string
	dialect   Dialect
	batchSize int
	timeout   time.Duration
	hc        *http.Client
}

var _ Embedder = (*Client)(nil)

// Dialect 返回这个客户端说的方言。返回的是副本（Dialect 全是字符串字段），
// 拿到它的人改不动客户端自己那一份。
func (c *Client) Dialect() Dialect { return c.dialect }

// ModelName 实现 Embedder，理由写在那个接口上。
func (c *Client) ModelName() string { return c.dialect.ModelName }

// New 建客户端。Endpoint 为空时**拒绝**，理由见 EnvEndpoint。
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("%w：没有配置 %s，拒绝构造推理引擎客户端：引擎地址必须显式指定。"+
			"给它一个默认值的话，忘了配的部署会每次索引都连不上，而索引是异步的——"+
			"没有人在等它的返回码，症状是几小时后的「新品怎么搜不到」",
			ErrEndpointUnset, EnvEndpoint)
	}
	// 方言与地址同一条纪律：空着就拒绝构造。地址有了而方言没有，是这个包
	// **最不该放过**的一种半配置 —— 理由写在 EnvDialect 上。
	dialect, err := LookupDialect(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	c := &Client{
		endpoint:  cfg.Endpoint,
		dialect:   dialect,
		batchSize: cfg.BatchSize,
		timeout:   cfg.Timeout,
		hc:        cfg.HTTPClient,
	}
	if c.batchSize <= 0 {
		c.batchSize = DefaultBatchSize
	}
	if c.batchSize > DefaultBatchSize {
		return nil, fmt.Errorf("批大小 %d 超过 %d（语义检索层 §10：索引侧批大小 32–64）",
			c.batchSize, DefaultBatchSize)
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	return c, nil
}

// FromEnv 按环境变量建客户端。这是**生产路径唯一的构造入口**。
//
// 它只会返回一个真的去打 HTTP 的客户端。替身不在这条路径上，也不可能在：
// 替身在 internal/inference/fake，那个包的实现文件带着 keel_fake_embedder
// 编译标签，默认构建里根本没有它（见 fake/doc.go 与 nofake_test.go）。
func FromEnv() (*Client, error) {
	return New(Config{
		Endpoint: os.Getenv(EnvEndpoint),
		Dialect:  os.Getenv(EnvDialect),
	})
}

type embedRequest struct {
	Model     string   `json:"model"`
	Texts     []string `json:"texts"`
	Normalize bool     `json:"normalize"`
}

type embedResponse struct {
	Embeddings   [][]float32 `json:"embeddings"`
	Dim          int         `json:"dim"`
	Model        string      `json:"model"`
	ModelVersion string      `json:"model_version"`
}

// Embed 把 texts 换成同样顺序的向量。
//
// **一次调用里 len(texts) 条文本最多发 ceil(len/batchSize) 次请求**，
// 不是 len(texts) 次。这条性质由 TestEmbedSendsOneRequestPerBatch 守着。
//
// 任何一步出错都返回 (nil, err)：不返回半截结果。半截结果的调用方要么
// 按下标对齐——那是一批张冠李戴的向量，要么补零——那是每次检索都会命中的
// 零向量。两个都不会报错。
func (c *Client) Embed(ctx context.Context, texts []string) (*Result, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w: texts 为空", ErrRejected)
	}
	out := make([][]float32, 0, len(texts))
	var model, version string
	for start := 0; start < len(texts); start += c.batchSize {
		end := min(start+c.batchSize, len(texts))
		chunk := texts[start:end]
		resp, err := c.postBatch(ctx, chunk)
		if err != nil {
			return nil, err
		}
		if err := c.validate(resp, len(chunk), start); err != nil {
			return nil, err
		}
		// 跨批的模型版本必须一致。不一致意味着这一批向量中途换过模型
		// （引擎被重新部署了），而它们会被同一条 UPDATE 记成同一个
		// model_version——从此没有任何东西知道前半截该重算。
		if model == "" {
			model, version = resp.Model, resp.ModelVersion
		} else if resp.Model != model || resp.ModelVersion != version {
			return nil, fmt.Errorf("%w: 同一次调用里引擎的模型变了（%s@%s → %s@%s），"+
				"这批向量来自两个模型，不能记成同一个 model_version",
				ErrProtocol, model, version, resp.Model, resp.ModelVersion)
		}
		out = append(out, resp.Embeddings...)
	}
	return &Result{Vectors: out, Model: model, ModelVersion: version}, nil
}

func (c *Client) postBatch(ctx context.Context, texts []string) (*embedResponse, error) {
	// 池化哨兵。**索引侧（service/index.go）与查询侧（service/search.go）都从
	// 这里过**，所以两边不可能只有一边补：两条路径共用同一次拼接，结构上不可能
	// 分叉。一边漏补的后果是拿两个不同空间里的向量算余弦距离 —— 算得出来、
	// 毫无意义、不报错。理由与实测数字写在 dialects 那张表的 PoolingSentinel 上。
	//
	// **方言化没有把这个循环拆成两处，这是刻意的。** 哨兵是一格**数据**
	// （keel-python 那条腿它是空串，拼上去是恒等操作），不是一个分支 ——
	// 写成 `if 这个方言要补 { ... }` 就多了一条只在某一个方言下才走的路径，
	// 而 TestEmbedAppendsPoolingSentinelToEveryText 这类测试只盯得住走到的那条。
	payload := make([]string, len(texts))
	for i, t := range texts {
		payload[i] = t + c.dialect.PoolingSentinel
	}
	body, err := json.Marshal(embedRequest{
		Model: c.dialect.ModelName, Texts: payload, Normalize: true})
	if err != nil {
		return nil, fmt.Errorf("%w: 序列化请求失败: %v", ErrProtocol, err)
	}
	// 单次请求的上限。取 ctx 与 c.timeout 里更早的那个——查询侧的 15 ms 预算
	// （§8）靠传一个更紧的 ctx 生效，而不是靠改这里。
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.endpoint+c.dialect.EmbedPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造请求失败: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := c.hc.Do(req)
	if err != nil {
		// 超时、连不上、DNS 解不出来，都落在这里。调用方按 ErrUnavailable 降级。
		// 两个 %w：外层让调用方能 errors.Is(err, ErrUnavailable) 走降级链，
		// 内层把原因留着——「超时了」（context.DeadlineExceeded）和
		// 「根本没人听」（ECONNREFUSED）对排查是两件事，而降级动作是同一个。
		return nil, fmt.Errorf("%w: 打 %s 失败（%d 条文本，单次上限 %s）: %w",
			ErrUnavailable, c.endpoint, len(texts), c.timeout, err)
	}
	defer httpResp.Body.Close()

	// 读一个有界的量。引擎理论上可以回一个无限流，而这个进程的内存不是无限的。
	// 上限按最大批 × 每维最长十进制表示粗算，留一倍余量。
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: 读响应失败: %v", ErrUnavailable, err)
	}
	switch {
	case httpResp.StatusCode == http.StatusOK:
	case httpResp.StatusCode >= 500:
		// 504 也在这里：§10 说「服务端超时后立刻返回，由调用方走降级链」，
		// 那条降级链的入口就是 ErrUnavailable。
		return nil, fmt.Errorf("%w: 引擎回了 %d: %s",
			ErrUnavailable, httpResp.StatusCode, snippet(raw))
	default:
		return nil, fmt.Errorf("%w: 引擎回了 %d: %s",
			ErrRejected, httpResp.StatusCode, snippet(raw))
	}

	var resp embedResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: 解析响应失败: %v（%s）", ErrProtocol, err, snippet(raw))
	}
	return &resp, nil
}

// validate 是这个包真正的产出：它决定一批向量能不能写进库。
//
// offset 只用于错误信息——出错的那一条在原始 texts 里的下标，
// 否则调用方拿到「第 3 条不对」而它送了 200 条，还要自己反推是哪一批的第 3 条。
func (c *Client) validate(resp *embedResponse, want, offset int) error {
	if resp.Model != c.dialect.ModelName {
		return fmt.Errorf("%w: 引擎说它是 %q，而方言 %q 说这批向量要记成 %q。"+
			"两条腿的向量空间不同（BGE-M3 与 Qwen3-Embedding 都是 1024 维，"+
			"但它们算出来的向量不能混在同一张 product_text_vectors 里），"+
			"所以对不上就拒收：混进去之后余弦距离照算不误，没有任何东西会报错",
			ErrProtocol, resp.Model, c.dialect.Name, c.dialect.ModelName)
	}
	if resp.ModelVersion == "" {
		return fmt.Errorf("%w: 引擎没有返回 model_version（§10 要求模型名与版本随响应返回）。"+
			"没有版本就没法回答「这批向量要不要重算」", ErrProtocol)
	}
	if len(resp.Embeddings) != want {
		return fmt.Errorf("%w: 送了 %d 条文本，收到 %d 个向量。"+
			"按下标对齐的话这是一批张冠李戴的向量", ErrProtocol, want, len(resp.Embeddings))
	}
	if resp.Dim != Dim {
		return fmt.Errorf("%w: 引擎报 dim=%d，而 product_text_vectors.embedding 是 vector(%d)",
			ErrProtocol, resp.Dim, Dim)
	}
	for i, v := range resp.Embeddings {
		if len(v) != Dim {
			return fmt.Errorf("%w: 第 %d 条向量有 %d 维，要 %d 维",
				ErrProtocol, offset+i, len(v), Dim)
		}
		// 归一化闸门。语义检索层 §2.3：
		//   「写入未归一化的向量会让距离计算失真，且这种错误不会报错、
		//     只会悄悄拉低召回质量」
		// 请求里带着 normalize: true，但那只是一句**请求**。这里核的是结果。
		// 引擎把那个参数读丢了、模型换成不带归一化池化的、中间有人加了一层
		// 做后处理的代理——三种都不会让任何一方报错。
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		norm := math.Sqrt(sum)
		if math.Abs(norm-1) > NormTolerance {
			return fmt.Errorf("%w: 第 %d 条向量的 L2 范数是 %.6f，不是 1（容差 %g）。"+
				"未归一化的向量配 vector_cosine_ops 会让距离失真，"+
				"而它入库之后不会报错、只会悄悄拉低召回（语义检索层 §2.3）",
				ErrProtocol, offset+i, norm, NormTolerance)
		}
	}
	return nil
}

func snippet(b []byte) string {
	const n = 200
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
