package search

import "sort"

// RRF 融合（语义检索层 §4）。
//
//	RRF(d) = Σ_i  1 / (k + rank_i(d))       k = 60
//
// 为什么是 RRF 而不是加权分数融合，§4 给了三条，其中第一条是硬的：
// 余弦距离与 ts_rank_cd **量纲完全不同**，归一化之后仍然不可比 ——
// 前者的分布取决于 embedding 空间的各向异性，后者取决于查询有几个词。
// RRF 只用名次，对分数分布免疫。
//
// 这段逻辑放在 internal/search 而不是 service，与 Bigram 同一个理由：
// 它是纯函数，不碰数据库也不碰网络，而 service 里的东西要开事务才测得动。

// RRFK 是 §4 的 k = 60。
//
// 「文献通用默认值，直接用，不必调」——§4 的原话。在有离线评测集（§9.1）
// 之前调它等于盲调：k 控制的是「名次靠后的结果贡献衰减得多快」，
// 而那件事只有 NDCG@10 能回答。
const RRFK = 60

// RecallSource 说一条结果是被哪一路捞回来的（契约 SearchHit.recall_source）。
type RecallSource string

const (
	SourceVector  RecallSource = "vector"
	SourceKeyword RecallSource = "keyword"
	SourceBoth    RecallSource = "both"
)

// Fused 是融合之后的一条。
type Fused struct {
	ID    int64
	Score float64

	// VectorRank / KeywordRank 是它在各路里的名次，**1 起算，0 表示那一路没捞到它**。
	// 0 而不是 -1：这两个数会出现在 explain 里给人看，而「第 0 名」不会被误读成
	// 「排在第一之前」。
	VectorRank  int
	KeywordRank int
}

// Source 按两个名次算 recall_source。
func (f Fused) Source() RecallSource {
	switch {
	case f.VectorRank > 0 && f.KeywordRank > 0:
		return SourceBoth
	case f.VectorRank > 0:
		return SourceVector
	default:
		return SourceKeyword
	}
}

// FuseRRF 融合两路召回的 id 列表（都必须**已经按各自的相关性排好序**），
// 返回按 RRF 得分从高到低排好的结果。
//
// 只收两路而不是 `lists ...[]int64`，是因为契约把这件事定死在两路上了：
// SearchHit.recall_source 的枚举就是 {vector, keyword, both}。将来加第三路
// （图像向量）时，改的不该只是这个函数的签名 —— 那个枚举也要动，而它在契约里。
// 变长参数会让「加一路」变成一个不需要碰契约的动作，那正是要避免的。
//
// 同一路里重复出现的 id 只认第一次（名次最好的那次）。SQL 那两条各自
// GROUP 过一遍主键，正常情况下不会重复；这里认第一次是为了让函数在被别处
// 复用时也有确定的语义，而不是悄悄把分数加两遍。
func FuseRRF(vector, keyword []int64) []Fused {
	byID := map[int64]*Fused{}
	order := []int64{}

	add := func(ids []int64, set func(f *Fused, rank int)) {
		for i, id := range ids {
			f, ok := byID[id]
			if !ok {
				f = &Fused{ID: id}
				byID[id] = f
				order = append(order, id)
			}
			set(f, i+1)
		}
	}
	add(vector, func(f *Fused, rank int) {
		if f.VectorRank == 0 {
			f.VectorRank = rank
		}
	})
	add(keyword, func(f *Fused, rank int) {
		if f.KeywordRank == 0 {
			f.KeywordRank = rank
		}
	})

	out := make([]Fused, 0, len(order))
	for _, id := range order {
		f := byID[id]
		// 只对**真的命中了**的那一路累加。把没命中的那一路也按
		// 1/(k+0) 记一笔的话，两路都没命中的商品会拿到最高分 ——
		// 而且那种写法能编译、能跑、结果看上去只是「排序有点怪」。
		if f.VectorRank > 0 {
			f.Score += 1 / float64(RRFK+f.VectorRank)
		}
		if f.KeywordRank > 0 {
			f.Score += 1 / float64(RRFK+f.KeywordRank)
		}
		out = append(out, *f)
	}

	// 分数相同时按 id 升序。**必须有这个次级键**：两条 RRF 分完全相同是常态
	// （两路都在第 3 名的两件商品分一模一样），而 sort.Slice 不稳定 ——
	// 没有次级键的话同一个请求发两次可以拿到不同的顺序，
	// 那会让 §9.1 的离线评测集一开始就测不出东西。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}
