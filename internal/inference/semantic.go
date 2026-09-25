package inference

import (
	"fmt"
	"math"
)

// 这一段是 M3 的验收标准在代码里的样子。
//
// 计划第二条写着：「验收标准是『搜「连衣裙」能返回相关商品』。一个哈希凑出来的
// 伪 embedding 能让所有测试变绿，而搜索结果毫无意义。」
//
// 一个「接口通了」的测试挡不住这件事——伪 embedding 一样能通过维度检查、
// 一样能通过归一化检查（随便一个向量除以自己的模长就归一化了）。能把两者
// 分开的只有一条：**语义相近的两条文本，向量也要相近**。
//
// 所以判据放在这里（而不是某个 _test.go 里），因为它要被两边共同引用：
//   - 真引擎那条测试（internal/inference，keel_real_engine 标签）断言 ≥ 阈值
//   - 替身那条测试（internal/inference/fake，keel_fake_embedder 标签）断言 < 阈值
// 一份阈值两处引用，对不上的时候只有一个嫌疑人。

// SemanticProbeTexts 是那三条文本：前两条语义相近（都是裙子），第三条无关。
//
// 刻意用中文，而且刻意不共享任何字符——「连衣裙」与「长裙」只共享一个「裙」字，
// 而 bigram 关键词召回（§3）对这两条的相似度很低。也就是说这三条文本能
// 区分开「真的懂语义」与「字面重合度高」，后者正是伪 embedding 最容易碰巧
// 蒙对的情形。
var SemanticProbeTexts = []string{
	"红色碎花连衣裙 女装 夏季新款",
	"女士夏季长裙 雪纺 显瘦",
	"汽车轮胎 205/55R16 四季胎",
}

// MinSemanticMargin 是「相近的那两条排在无关的那条前面」这句话的量化。
//
// margin = cos(裙, 裙) − max(cos(裙, 轮胎))。真引擎实测 0.19（BGE-M3，CPU）。
// 0.10 是它的一半：留够余量给模型小版本升级带来的漂移，同时远大于
// 「两个随机单位向量的余弦」的量级——1024 维下那个量的标准差约
// 1/sqrt(1024) ≈ 0.031，两个之差的标准差约 0.044。也就是说一个没有语义的
// 替身要偶然跨过 0.10，得是 2σ 以上的巧合，而且它是确定性的：
// 跑一次就定死了，不会今天绿明天红。
const MinSemanticMargin = 0.10

// SemanticScores 是三条探针文本两两之间的余弦相似度。
type SemanticScores struct {
	NearNear float64 // cos(裙, 裙)
	NearFar  float64 // max(cos(裙, 轮胎))，取两条里更高的那个
	Margin   float64 // NearNear − NearFar
	Matrix   [3][3]float64
}

// SemanticMargin 按 SemanticProbeTexts 的顺序算三条向量的相似度矩阵与 margin。
//
// 向量必须已经归一化（这个包的 Client 保证这一点），所以内积就是余弦。
func SemanticMargin(vecs [][]float32) (SemanticScores, error) {
	var s SemanticScores
	if len(vecs) != len(SemanticProbeTexts) {
		return s, fmt.Errorf("要 %d 个向量，拿到 %d 个", len(SemanticProbeTexts), len(vecs))
	}
	for i := range vecs {
		for j := range vecs {
			if len(vecs[i]) != len(vecs[j]) {
				return s, fmt.Errorf("向量 %d 与 %d 维度不同（%d vs %d）",
					i, j, len(vecs[i]), len(vecs[j]))
			}
			var dot float64
			for k := range vecs[i] {
				dot += float64(vecs[i][k]) * float64(vecs[j][k])
			}
			s.Matrix[i][j] = dot
		}
	}
	s.NearNear = s.Matrix[0][1]
	s.NearFar = math.Max(s.Matrix[0][2], s.Matrix[1][2])
	s.Margin = s.NearNear - s.NearFar
	return s, nil
}
