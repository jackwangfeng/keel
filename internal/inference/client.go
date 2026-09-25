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
	"time"
)

// Dim 与 ModelName 是数据模型 §8 的 `embedding vector(1024)` 与「BGE-M3」
// 在 Go 侧的复述。它们是**闸门**，不是配置项：引擎返回别的维度或别的模型名时
// 这个包拒收，而不是把它当成一次成功的调用。
const (
	Dim       = 1024
	ModelName = "bge-m3"
)

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
}

// Config 是一个客户端的全部配置。
type Config struct {
	// Endpoint 形如 http://inference:8000。空着时 New 报错，见 EnvEndpoint。
	Endpoint string
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

// Client 是 /v1/embed 的客户端。零值不可用，走 New。
type Client struct {
	endpoint  string
	batchSize int
	timeout   time.Duration
	hc        *http.Client
}

var _ Embedder = (*Client)(nil)

// New 建客户端。Endpoint 为空时**拒绝**，理由见 EnvEndpoint。
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("没有配置 %s，拒绝构造推理引擎客户端：引擎地址必须显式指定。"+
			"给它一个默认值的话，忘了配的部署会每次索引都连不上，而索引是异步的——"+
			"没有人在等它的返回码，症状是几小时后的「新品怎么搜不到」", EnvEndpoint)
	}
	c := &Client{
		endpoint:  cfg.Endpoint,
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
	return New(Config{Endpoint: os.Getenv(EnvEndpoint)})
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
	body, err := json.Marshal(embedRequest{Model: ModelName, Texts: texts, Normalize: true})
	if err != nil {
		return nil, fmt.Errorf("%w: 序列化请求失败: %v", ErrProtocol, err)
	}
	// 单次请求的上限。取 ctx 与 c.timeout 里更早的那个——查询侧的 15 ms 预算
	// （§8）靠传一个更紧的 ctx 生效，而不是靠改这里。
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.endpoint+"/v1/embed", bytes.NewReader(body))
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
	if resp.Model != ModelName {
		return fmt.Errorf("%w: 引擎说它是 %q，而 product_text_vectors 里这批向量要记成 %q。"+
			"换模型等于改 DDL（vector(%d) 的维度是列级固定的）",
			ErrProtocol, resp.Model, ModelName, Dim)
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
