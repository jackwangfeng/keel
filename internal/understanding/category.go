package understanding

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/keel/keel/internal/inference"
)

// 类目推荐：设计 §1 能力清单里 category_classify 的**第一步**，也是今天
// 在推理引擎只有 /v1/embeddings、没有 /v1/generate 的前提下能做的那一半。
//
// ===========================================================================
// 做法：标题向量 vs 类目路径名向量，余弦最近
// ===========================================================================
//
// 商品的「标题 + 副标题」算一条向量，每个叶子类目的**路径名**（「服装 > 女装 >
// 连衣裙」）算一条向量，按余弦取 Top 3。这是零样本的：不需要任何已标注的
// 商品，新开的店第一次导入就能用（ai-capabilities 第 3 项「需积累：否」）。
//
// 它**不是**分类模型，代价说清楚：
//
//   - 只看类目的名字。「默认分类」「其他」「新品」这种不描述商品的类目名，
//     向量上与什么都不近 —— 它们永远不会被推荐，只能手选。
//   - 同级类目名字很像时（「女装 > 外套」与「男装 > 外套」）主要靠标题里的
//     性别词区分，标题里没有时分数会挤在一起 —— 所以有下面那道阈值。
//   - 设计 §1 写的是「标题 + 图」，图这一路今天没有（image_embedding 未落地）。
//
// 更准的路子是拿店里**已经归好类的商品**做近邻投票（product_text_vectors 里
// 现成就有向量）—— 那要求店里先有商品，冷启动恰恰没有；两路融合留给以后。
//
// ===========================================================================
// 置信度：两道门，都过了才自动选
// ===========================================================================
//
// 自动选中要求 Top-1 **同时**满足：
//
//   - 余弦 ≥ DefaultCategoryGate.MinScore（0.50）—— 「至少有点像」。它挡的是
//     「店里没有一个类目对得上」的商品（标题写成「测试 001」、或者店里只有一个
//     「默认分类」）：那时 Top-1 只是矮子里拔将军。
//   - 与 Top-2 的分差 ≥ DefaultCategoryGate.MinMargin（0.03）—— 「明显比第二名像」。
//
// **起主要作用的是第二道。** 离线评测（testdata/category_eval/，`make category-eval`，
// 要真引擎）里只按余弦卡阈值时，precision 要到 0.70 才过 95%，而那时只有 23% 的
// 商品被自动选中 —— 余弦的绝对值更多反映「标题写得像不像类目名」，
// 而不是「分得清分不清」；推错的那些恰恰是 Top-1 与 Top-2 挤在一起的
// （「百褶裙」在连衣裙与半身裙之间只差 0.02）。按分差卡，同样 ≥ 95% 的 precision
// 下能自动选中 74%。完整的表与取舍写在商品理解服务设计文档的「批量导入」一节。
//
// 不过门时候选照样给（按分数排好），只是不替商家选，界面标「需人工确认」。
//
// **换模型时这两个数会失效，而且不报错** —— 余弦的尺度是模型相关的。
// 标定模型是 inference.ModelName（Qwen3-Embedding-0.6B），换模型必须重跑评测。
//
// ===========================================================================
// 类目向量的缓存
// ===========================================================================
//
// 类目一家店几十到几百个，每次预检都重算一遍不贵，但没必要。缓存以**路径名文本**
// 为键：类目改名、移动之后路径名变了，键就变了，旧向量自然不再命中 ——
// 「按类目变更失效」不需要任何人记得去调一个 Invalidate。
//
// 另一种失效是**换模型**：每条缓存记着算它的 model_version，本次查询的向量来自
// 另一个版本时整个缓存作废重算。两个模型的向量放在一起比余弦是没有意义的数字，
// 而且不会报错。
//
// 缓存在进程内，不落库：多实例部署时各算各的，代价是每个实例冷启动时多一次
// 嵌入调用；换来的是不多一张要登记租户策略、要随类目写路径同步的表。

// CategoryGate 是「自动选中」的两道门，见文件头第二节。
type CategoryGate struct {
	MinScore  float64 // Top-1 余弦下限
	MinMargin float64 // Top-1 与 Top-2 的分差下限
}

// DefaultCategoryGate 是离线评测定出来的那一组。
// 标定模型：inference.ModelName（Qwen3-Embedding-0.6B）。
var DefaultCategoryGate = CategoryGate{MinScore: 0.50, MinMargin: 0.03}

// CategoryTopK 是返回的候选数。
const CategoryTopK = 3

// categoryQueryInstruct 是给查询侧（商品标题）加的任务指令。
//
// Qwen3-Embedding 是指令感知的：查询侧带一句「任务是什么」，文档侧（类目名）不带，
// 这是它官方推荐的非对称检索用法。离线评测里带与不带两种都跑了，结果写在
// 设计文档那一节；留下的是更好的那一种。
const categoryQueryInstruct = "Instruct: 给定一个电商商品标题，找出它所属的商品类目\nQuery: "

// CategoryQueryText 是一件商品送去嵌入的文本。导出给离线评测用 ——
// 评测与线上必须用**同一种**拼法，否则评测出来的阈值说的是另一件事。
func CategoryQueryText(title, subtitle string) string {
	t := strings.TrimSpace(title)
	if s := strings.TrimSpace(subtitle); s != "" {
		t += " " + s
	}
	return categoryQueryInstruct + t
}

// CategoryDocText 是一个类目送去嵌入的文本：路径名本身。
func CategoryDocText(pathName string) string { return pathName }

// CategoryOption 是一个可被推荐的类目（叶子）。
type CategoryOption struct {
	ID       int64
	PathName string // 「服装 > 女装 > 连衣裙」
}

// CategoryCandidate 是一个候选。
type CategoryCandidate struct {
	ID       int64
	PathName string
	Score    float64 // 余弦，[-1, 1]
}

// ErrNoEmbedder 是没有配置推理引擎（KEEL_EMBED_ENDPOINT 为空）。
// 调用方把它与「引擎配了但挂了」区分开：前者是部署形态，后者是故障。
var ErrNoEmbedder = errors.New("没有配置推理引擎，类目推荐不可用")

// CategoryRecommender 算类目候选。零值不可用，走 NewCategoryRecommender。
type CategoryRecommender struct {
	emb inference.Embedder

	mu      sync.Mutex
	version string // 缓存里全部向量来自哪个 model_version
	cache   map[string][]float32
}

// maxCachedCategories 是缓存条数的上限。多租户时每家店的类目各占一份，
// 超了就整个清掉重来 —— 比 LRU 简单，而类目向量本来就便宜。
const maxCachedCategories = 20000

// NewCategoryRecommender 建一个。emb 可以是 nil（没配引擎的部署），
// 那时 Recommend 一律返回 ErrNoEmbedder，调用方降级为「不推荐，需手选」。
func NewCategoryRecommender(emb inference.Embedder) *CategoryRecommender {
	return &CategoryRecommender{emb: emb, cache: map[string][]float32{}}
}

// Recommend 给每条查询算 Top K 候选（按分数降序）。
//
// 一次调用里查询与缺缓存的类目**合成一批**送去嵌入（inference.Client 自己按 64
// 切批），不按商品循环单条调 —— 设计 §6「禁止循环单条调用」。
//
// 任何引擎错误都原样返回（通常 wrap 着 inference.ErrUnavailable），不返回半截结果：
// 调用方拿到错误就整批降级，而不是有的商品有推荐、有的没有、原因看不出来。
func (r *CategoryRecommender) Recommend(ctx context.Context, queries []string,
	options []CategoryOption) ([][]CategoryCandidate, error) {

	if r == nil || r.emb == nil {
		return nil, ErrNoEmbedder
	}
	out := make([][]CategoryCandidate, len(queries))
	if len(queries) == 0 || len(options) == 0 {
		return out, nil
	}

	docTexts := make([]string, len(options))
	for i, o := range options {
		docTexts[i] = CategoryDocText(o.PathName)
	}

	r.mu.Lock()
	missing := uniqueMissing(docTexts, r.cache)
	r.mu.Unlock()

	texts := append(append([]string{}, queries...), missing...)
	res, err := r.emb.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(res.Vectors) != len(texts) {
		return nil, fmt.Errorf("%w: 送了 %d 条，回了 %d 条", inference.ErrProtocol, len(texts), len(res.Vectors))
	}
	qvecs := res.Vectors[:len(queries)]

	r.mu.Lock()
	if r.version != res.ModelVersion || len(r.cache)+len(missing) > maxCachedCategories {
		// 换了模型（或者缓存涨满了）：旧向量整体作废。这一批里刚算的那些是新版本的，留下。
		r.cache = map[string][]float32{}
		r.version = res.ModelVersion
	}
	for i, t := range missing {
		r.cache[t] = res.Vectors[len(queries)+i]
	}
	stillMissing := uniqueMissing(docTexts, r.cache)
	r.mu.Unlock()

	if len(stillMissing) > 0 {
		// 缓存刚被作废（换模型），上一步只补了「当时缺的」。把其余的也按新模型算一遍。
		res2, err := r.emb.Embed(ctx, stillMissing)
		if err != nil {
			return nil, err
		}
		if len(res2.Vectors) != len(stillMissing) || res2.ModelVersion != res.ModelVersion {
			return nil, fmt.Errorf("%w: 同一次推荐里引擎的模型变了（%s → %s）",
				inference.ErrProtocol, res.ModelVersion, res2.ModelVersion)
		}
		r.mu.Lock()
		for i, t := range stillMissing {
			r.cache[t] = res2.Vectors[i]
		}
		r.mu.Unlock()
	}

	r.mu.Lock()
	dvecs := make([][]float32, len(options))
	for i, t := range docTexts {
		dvecs[i] = r.cache[t]
	}
	r.mu.Unlock()

	for qi, q := range qvecs {
		cands := make([]CategoryCandidate, 0, len(options))
		for oi, o := range options {
			cands = append(cands, CategoryCandidate{ID: o.ID, PathName: o.PathName, Score: cosine(q, dvecs[oi])})
		}
		sort.SliceStable(cands, func(a, b int) bool { return cands[a].Score > cands[b].Score })
		if len(cands) > CategoryTopK {
			cands = cands[:CategoryTopK]
		}
		out[qi] = cands
	}
	return out, nil
}

func uniqueMissing(texts []string, cache map[string][]float32) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range texts {
		if _, ok := cache[t]; ok || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// cosine 按定义算，不假设已归一化：引擎承诺归一化（inference.Client 校验过），
// 但替身与将来别的实现未必，而这里多除一次模长几乎不花钱。
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Confident 说一组候选能不能自动选第一个。
//
// 只有一个候选（店里只有一个叶子类目）时没有第二名可比，只看余弦下限。
func (g CategoryGate) Confident(cands []CategoryCandidate) bool {
	if len(cands) == 0 || cands[0].Score < g.MinScore {
		return false
	}
	return len(cands) == 1 || cands[0].Score-cands[1].Score >= g.MinMargin
}
