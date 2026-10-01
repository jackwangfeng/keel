// Package searcheval 是检索离线评测集（语义检索层 §9.1）的数据格式与算分逻辑。
//
// 一份评测集分三段，每段一个 JSONL 文件，前一段的产物是后一段的输入：
//
//	① 候选对（Pair）：cmd/keel-searcheval sample 从 search_logs 抽查询，给每条查询配上
//	   向量近邻与关键词命中的候选商品，连同标题、类目、余弦相似度一起写出来。
//	   打标的人（或模型）只看这份文件，不用连库。
//	② 标签（Label）：每个 (商户, 查询, 商品) 一个 0–1 的相关度。来源可以是人工，
//	   也可以是 System One 类判别模型（Kev / Jev）给的「相关」概率。Judge 记来源。
//	③ 报告：Calibrate 把两份对上，扫一遍相关度下限（service.DefaultVectorFloor）。
//
// # 为什么只拿「只被向量路捞到的」那部分校准下限
//
// 下限只作用在它们身上（service/search.go 的 applyFloor：关键词路捞到的一律可信）。
// 关键词命中的候选照样写进候选对、照样打标 —— 它们是 §9.1 其余指标（Recall / NDCG）要的，
// 但不进下限的混淆矩阵，否则一个与下限无关的部分会把精确率抬高或拉低。
package searcheval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
)

// Pair 是一个待打标的 (查询, 商品)。
type Pair struct {
	MerchantID int64  `json:"merchant_id"`
	Query      string `json:"query"`
	// QueryHits 是这条查询在 search_logs 里出现的次数（-extra 加进来的查询为 0）。
	QueryHits int64  `json:"query_hits"`
	ProductID int64  `json:"product_id"`
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle,omitempty"`
	Category  string `json:"category"`
	// Similarity 是余弦相似度（1 - 距离）；商品没有向量行时缺席。
	Similarity *float64 `json:"similarity,omitempty"`
	// VectorRank 是它在向量近邻里的名次（从 1 起）；不在近邻里为 0。
	VectorRank int `json:"vector_rank"`
	// KeywordAnd / KeywordOr：全部二元组都命中 / 至少命中一个（AND ⊆ OR）。
	KeywordAnd bool `json:"keyword_and"`
	KeywordOr  bool `json:"keyword_or"`
	// EmbedModel 是给查询算向量的模型（名字@版本）：换模型后相似度的尺度会变，下限要重标。
	EmbedModel string `json:"embed_model"`
}

// VectorOnly：只可能经向量路进来、因此受下限管的候选。
func (p Pair) VectorOnly() bool { return p.VectorRank > 0 && !p.KeywordOr && p.Similarity != nil }

// Label 是一个 (查询, 商品) 的相关度标签。
type Label struct {
	MerchantID int64  `json:"merchant_id"`
	Query      string `json:"query"`
	ProductID  int64  `json:"product_id"`
	// Relevance 是 0–1：人工标注给 0 或 1，判别模型给「相关」的概率。
	Relevance float64 `json:"relevance"`
	// Judge 是标签来源，如 "human" 或 "kev-0.8b@<版本>"。
	Judge string `json:"judge"`
}

type key struct {
	merchant int64
	query    string
	product  int64
}

// ReadJSONL 逐行解出 T。空行跳过；任何一行解不出就报错并带行号。
func ReadJSONL[T any](r io.Reader) ([]T, error) {
	var out []T
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// WriteJSONL 每个元素一行。
func WriteJSONL[T any](w io.Writer, items []T) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return nil
}

// Options 是 Calibrate 的可调项。
type Options struct {
	// RelevantAt：Relevance ≥ 它算相关。0 用 0.5。
	RelevantAt float64
	// From / To / Step 是下限的扫描区间。全为 0 时用 0.20–0.90、步长 0.01。
	From, To, Step float64
	// TargetPrecision：推荐「精确率不低于它的最低下限」。0 用 0.9。
	TargetPrecision float64
}

func (o Options) withDefaults() Options {
	if o.RelevantAt == 0 {
		o.RelevantAt = 0.5
	}
	if o.From == 0 && o.To == 0 && o.Step == 0 {
		o.From, o.To, o.Step = 0.20, 0.90, 0.01
	}
	if o.TargetPrecision == 0 {
		o.TargetPrecision = 0.9
	}
	return o
}

// Row 是一个下限上的成绩（只算只被向量路捞到的候选）。
type Row struct {
	Floor     float64 `json:"floor"`
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	FN        int     `json:"fn"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	F1        float64 `json:"f1"`
	// FallbackRight：本该「猜你想要」的查询（所有候选都不相关）里，这个下限下真的一条可信命中都没有的占比。
	FallbackRight float64 `json:"fallback_right"`
	// FallbackWrong：有相关候选的查询里，这个下限下却一条可信命中都没有、被判成「猜你想要」的占比。
	FallbackWrong float64 `json:"fallback_wrong"`
}

// Report 是一次校准的结果。
type Report struct {
	Pairs      int `json:"pairs"`       // 对上标签的候选对
	VectorOnly int `json:"vector_only"` // 其中只被向量路捞到的（进混淆矩阵的那部分）
	Relevant   int `json:"relevant"`    // 只被向量路捞到、且相关的
	Unlabeled  int `json:"unlabeled"`   // 没有标签的候选对（不参与）
	Queries    int `json:"queries"`
	// NothingRelevant 是全部候选都不相关的查询数（本该「猜你想要」）。
	NothingRelevant int      `json:"nothing_relevant"`
	Rows            []Row    `json:"rows"`
	BestF1          *Row     `json:"best_f1,omitempty"`
	AtPrecision     *Row     `json:"at_precision,omitempty"` // 精确率 ≥ TargetPrecision 的最低下限
	Judges          []string `json:"judges"`
}

// Calibrate 把候选对与标签对上，扫一遍下限。标签按 (商户, 查询, 商品) 对；同一个键多条标签取平均。
func Calibrate(pairs []Pair, labels []Label, opt Options) Report {
	opt = opt.withDefaults()

	sum := map[key]float64{}
	cnt := map[key]int{}
	judges := map[string]bool{}
	for _, l := range labels {
		k := key{l.MerchantID, l.Query, l.ProductID}
		sum[k] += l.Relevance
		cnt[k]++
		judges[l.Judge] = true
	}

	type qkey struct {
		merchant int64
		query    string
	}
	type labeled struct {
		Pair
		rel bool
	}
	byQuery := map[qkey][]labeled{}
	var rep Report
	for _, p := range pairs {
		k := key{p.MerchantID, p.Query, p.ProductID}
		if cnt[k] == 0 {
			rep.Unlabeled++
			continue
		}
		rel := sum[k]/float64(cnt[k]) >= opt.RelevantAt
		rep.Pairs++
		if p.VectorOnly() {
			rep.VectorOnly++
			if rel {
				rep.Relevant++
			}
		}
		qk := qkey{p.MerchantID, p.Query}
		byQuery[qk] = append(byQuery[qk], labeled{p, rel})
	}
	rep.Queries = len(byQuery)
	for j := range judges {
		rep.Judges = append(rep.Judges, j)
	}
	sort.Strings(rep.Judges)

	anyRelevant := map[qkey]bool{}
	for qk, ls := range byQuery {
		for _, l := range ls {
			if l.rel {
				anyRelevant[qk] = true
				break
			}
		}
		if !anyRelevant[qk] {
			rep.NothingRelevant++
		}
	}

	steps := int(math.Round((opt.To-opt.From)/opt.Step)) + 1
	for i := 0; i < steps; i++ {
		f := math.Round((opt.From+float64(i)*opt.Step)*1e6) / 1e6
		row := Row{Floor: f}
		var fbRight, fbWrong int
		for qk, ls := range byQuery {
			trusted := false
			for _, l := range ls {
				if l.KeywordOr {
					trusted = true
				}
				if !l.VectorOnly() {
					continue
				}
				kept := *l.Similarity >= f
				trusted = trusted || kept
				switch {
				case kept && l.rel:
					row.TP++
				case kept && !l.rel:
					row.FP++
				case !kept && l.rel:
					row.FN++
				}
			}
			if !trusted {
				if anyRelevant[qk] {
					fbWrong++
				} else {
					fbRight++
				}
			}
		}
		row.Precision = ratio(row.TP, row.TP+row.FP)
		row.Recall = ratio(row.TP, row.TP+row.FN)
		if row.Precision+row.Recall > 0 {
			row.F1 = 2 * row.Precision * row.Recall / (row.Precision + row.Recall)
		}
		row.FallbackRight = ratio(fbRight, rep.NothingRelevant)
		row.FallbackWrong = ratio(fbWrong, rep.Queries-rep.NothingRelevant)
		rep.Rows = append(rep.Rows, row)
	}

	for i := range rep.Rows {
		r := &rep.Rows[i]
		// F1 全是 0（一个相关的都没有，或者一个都没留对）时不推荐：那时「最高」只是扫描区间的第一档。
		if r.F1 > 0 && (rep.BestF1 == nil || r.F1 > rep.BestF1.F1) {
			rep.BestF1 = r
		}
		if rep.AtPrecision == nil && r.TP+r.FP > 0 && r.Precision >= opt.TargetPrecision {
			rep.AtPrecision = r
		}
	}
	return rep
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
