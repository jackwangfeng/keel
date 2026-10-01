package searcheval

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/keel/keel/internal/inference"
)

// 用 System One 判别模型（Kev / Jev）给候选对打标。
//
// 一条查询一次请求：状态是这条查询，每件候选一道是非题（题面带商品的标题、副标题、类目），
// 答案「是」的概率就是 Label.Relevance。这样查询只编码一次，每道题从那里分叉
// （引擎在同一个请求里的题之间合批，跨请求不合批），比一对一请求省掉候选数倍的重复计算。
//
// 题面写的是「是不是买家要找的东西」，不是「字面上相不相关」：下限要分开的正是
// 「同类可替代」与「只是沾了一个词」，后者在关键词路上已经够多了。

// RelevanceCriteria 是是非题两头的说明。
var RelevanceCriteria = map[string]string{
	"true":  "是：就是买家要找的这类商品，或能直接替代它的同类商品",
	"false": "不是：品类不同、用途不同，或只是名字里碰巧有相同的字",
}

// RelevanceState 是一条查询的共享状态。
func RelevanceState(query string) map[string]string {
	return map[string]string{"场景": "买家在一家店里用搜索框找商品", "买家搜索的词": query}
}

// RelevanceQuestion 是一件候选的那道题。
func RelevanceQuestion(p Pair) inference.SystemOneQuestion {
	var b strings.Builder
	fmt.Fprintf(&b, "下面这件商品是不是买家要找的？\n商品标题：%s", p.Title)
	if p.Subtitle != "" {
		fmt.Fprintf(&b, "\n副标题：%s", p.Subtitle)
	}
	if p.Category != "" {
		fmt.Fprintf(&b, "\n类目：%s", p.Category)
	}
	return inference.SystemOneQuestion{Type: inference.QuestionNoul, Instructions: b.String(), Criteria: RelevanceCriteria}
}

// Decider 是 inference.SystemOneClient 里打标要用的那一个方法（测试换成假的）。
type Decider interface {
	Decide(ctx context.Context, r inference.SystemOneRequest) (*inference.SystemOneResponse, error)
}

// LabelQuery 给同一条查询（同一家店）的一批候选打标，每 perRequest 道题一次请求。
// 候选里混进了别的查询或别的商户是调用方的错，直接报错。
func LabelQuery(ctx context.Context, d Decider, model string, pairs []Pair, perRequest int) ([]Label, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	if perRequest <= 0 {
		perRequest = 32
	}
	q, m := pairs[0].Query, pairs[0].MerchantID
	out := make([]Label, 0, len(pairs))
	for start := 0; start < len(pairs); start += perRequest {
		chunk := pairs[start:min(start+perRequest, len(pairs))]
		req := inference.SystemOneRequest{State: RelevanceState(q), Model: model,
			Questions: make(map[string]inference.SystemOneQuestion, len(chunk))}
		for _, p := range chunk {
			if p.Query != q || p.MerchantID != m {
				return nil, fmt.Errorf("一批里混进了别的查询：%d/%q 与 %d/%q", m, q, p.MerchantID, p.Query)
			}
			req.Questions[questionID(p.ProductID)] = RelevanceQuestion(p)
		}
		resp, err := d.Decide(ctx, req)
		if err != nil {
			return nil, err
		}
		judge := resp.Model + "@" + resp.ModelVersion
		for _, p := range chunk {
			a := resp.Answers[questionID(p.ProductID)]
			if a.Noul == nil {
				return nil, fmt.Errorf("商品 %d 没有拿到是非题的答案", p.ProductID)
			}
			out = append(out, Label{MerchantID: m, Query: q, ProductID: p.ProductID, Relevance: *a.Noul, Judge: judge})
		}
	}
	return out, nil
}

func questionID(productID int64) string { return "p" + strconv.FormatInt(productID, 10) }

// GroupByQuery 按 (商户, 查询) 分组，保持各组第一次出现的先后。
func GroupByQuery(pairs []Pair) [][]Pair {
	type qk struct {
		m int64
		q string
	}
	idx := map[qk]int{}
	var groups [][]Pair
	for _, p := range pairs {
		k := qk{p.MerchantID, p.Query}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], p)
	}
	return groups
}
