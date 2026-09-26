//go:build keel_fake_embedder

// 这一行标签是这个文件存在的前提，别删。理由写在 doc.go 里：
// 删掉它，替身就能在生产路径上被选中，而它出的事不会报错——
// 商品照常索引、向量照常入库、搜索照常返回结果，只是结果和搜的词无关。
// internal/inference/nofake_test.go 守着这一行。

package fake

import (
	"context"
	"crypto/sha256"
	"math"
	"math/rand/v2"

	"github.com/keel/keel/internal/inference"
)

// Version 是替身的「模型版本」。刻意长成这样，而不是一串像 commit sha 的十六进制：
// 它会被写进 product_text_vectors.model_version，万一真漏进了库，
// 那一列会直接把这件事喊出来，而不是伪装成一次正常的索引。
const Version = "fake-not-a-real-model"

// Dialect 是替身**假装**自己是哪条腿。默认 infero，因为那是今天的默认部署。
//
// 它存在只有一个理由：替身报的 model 名要能和 product_text_vectors.model_name
// 对上，否则 service/index.go 的 decide 会把每一件商品都判成「换模型了」，
// 于是每一轮都重算全部 —— 一条永远追不完的队列，而且不报错。
// 换腿的测试（两种方言各跑一遍判定）把它改成 inference.DialectKeelPython。
var Dialect = inference.MustDialect(inference.DialectInfero)

// Embedder 是确定性替身：同样的文本永远得到同样的单位向量。
//
// 它**不假装**有语义。文本经 SHA-256 变成种子，种子驱动一个 PRNG 生成
// 1024 个正态分布的数，再除以模长。两条不同的文本因此得到两个近乎正交的
// 向量——不论它们的意思有多接近。这是刻意的：替身要能证明自己**不是**
// 真引擎（见 fake_semantic_test.go），一个「稍微懂一点语义」的替身
// 才是最危险的那种，因为它会让「用的是不是真引擎」这个问题变得不可观测。
type Embedder struct {
	// Model 是这个替身声称的模型名。零值时用 Dialect.ModelName。
	Model string

	// Calls 记下每一次调用送进来的批。测试用它核对「批量没有退化成循环单条」
	// 这条性质在**不走 HTTP** 的路径上也成立。
	Calls [][]string
}

var _ inference.Embedder = (*Embedder)(nil)

// Embed 实现 inference.Embedder。
func (e *Embedder) Embed(_ context.Context, texts []string) (*inference.Result, error) {
	e.Calls = append(e.Calls, append([]string(nil), texts...))
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = Vector(t)
	}
	return &inference.Result{
		Vectors:      out,
		Model:        e.ModelName(),
		ModelVersion: Version,
	}, nil
}

// ModelName 实现 inference.Embedder。理由写在那个接口上：索引侧要在**调用
// 引擎之前**知道这个字符串，好判断库里的存量行是不是另一个模型算的。
func (e *Embedder) ModelName() string {
	if e.Model != "" {
		return e.Model
	}
	return Dialect.ModelName
}

// Vector 把一条文本变成一个确定性的 1024 维单位向量。
func Vector(text string) []float32 {
	sum := sha256.Sum256([]byte(text))
	seed := [32]byte(sum)
	r := rand.New(rand.NewChaCha8(seed))
	v := make([]float32, inference.Dim)
	var norm float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
