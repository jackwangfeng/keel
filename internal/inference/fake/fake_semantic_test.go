//go:build keel_fake_embedder

// 跑法：go test -tags keel_fake_embedder ./internal/inference/...
// （Makefile 的 test-db 目标带着这一行跑，所以它在默认闸门里。）

package fake_test

import (
	"context"
	"math"
	"testing"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/inference/fake"
)

// 替身能通过**所有形状闸门**。这一条不是在夸它，是在说明问题有多隐蔽：
// 维度对、范数是 1、条数对、模型名对——客户端 validate 里的每一条它都过。
// 也就是说，「伪 embedding 混进生产」这件事不会被本仓库任何一道形状闸门发现。
func TestFakeEmbedderPassesEveryShapeGate(t *testing.T) {
	var e fake.Embedder
	res, err := e.Embed(context.Background(), inference.SemanticProbeTexts)
	if err != nil {
		t.Fatalf("替身失败: %v", err)
	}
	if len(res.Vectors) != len(inference.SemanticProbeTexts) {
		t.Fatalf("条数对不上: %d", len(res.Vectors))
	}
	for i, v := range res.Vectors {
		if len(v) != inference.Dim {
			t.Fatalf("第 %d 条是 %d 维", i, len(v))
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if n := math.Sqrt(sum); math.Abs(n-1) > inference.NormTolerance {
			t.Fatalf("第 %d 条范数 %v —— 替身连归一化都没做对，"+
				"那它连「形状对但没语义」这个角色都演不了", i, n)
		}
	}
	if res.Model != inference.ModelName {
		t.Fatalf("model 是 %q", res.Model)
	}
}

// **替身跑不出真实的语义相关性。** 这是它与真引擎唯一的、也是决定性的差别。
//
// 断言的形状与 realengine_test.go 里那条严格相反，共用同一个阈值
// inference.MinSemanticMargin：真引擎 ≥ 阈值，替身 < 阈值。
// 两条一起才有意义——只有前者的话，「真引擎」是不是真的没人知道；
// 只有后者的话，阈值定多高都能绿。
//
// 为什么这条断言不会抖：替身是确定性的（SHA-256 → ChaCha8 → 固定向量），
// 同一批文本永远得到同一组相似度。而它的量级也不是碰运气——1024 维下
// 两个随机单位向量的余弦标准差约 1/sqrt(1024) ≈ 0.031。
func TestFakeEmbedderCannotRankSemanticNeighbors(t *testing.T) {
	var e fake.Embedder
	res, err := e.Embed(context.Background(), inference.SemanticProbeTexts)
	if err != nil {
		t.Fatalf("替身失败: %v", err)
	}
	s, err := inference.SemanticMargin(res.Vectors)
	if err != nil {
		t.Fatalf("算相似度失败: %v", err)
	}
	for i, row := range s.Matrix {
		t.Logf("%-28s %7.4f %7.4f %7.4f",
			inference.SemanticProbeTexts[i], row[0], row[1], row[2])
	}
	t.Logf("替身 margin = %.4f（真引擎的下限是 %.2f）", s.Margin, inference.MinSemanticMargin)

	if s.Margin >= inference.MinSemanticMargin {
		t.Fatalf("替身居然跨过了语义下限（margin=%.4f ≥ %.2f）。"+
			"那它就不再是一个「显然不是真引擎」的替身了——"+
			"它会让「生产上用的到底是不是真模型」这个问题变得不可观测。"+
			"要么换掉替身的实现，要么换掉探针文本",
			s.Margin, inference.MinSemanticMargin)
	}

	// 顺带把「为什么做不到」说清楚：任意两条不同文本之间几乎正交。
	// 没有这一条的话，上面那条断言也可能因为「替身给所有文本同一个向量」
	// 而绿（那时 margin 恒为 0），而那是另一种坏替身。
	for i := range s.Matrix {
		for j := range s.Matrix[i] {
			if i == j {
				if math.Abs(s.Matrix[i][j]-1) > 1e-3 {
					t.Fatalf("自己和自己的余弦是 %.4f，替身不是确定性的", s.Matrix[i][j])
				}
				continue
			}
			if math.Abs(s.Matrix[i][j]) > 0.15 {
				t.Fatalf("替身给 %d 与 %d 的余弦是 %.4f —— 它不该有任何结构",
					i, j, s.Matrix[i][j])
			}
		}
	}
}

// 替身是确定性的：同一条文本两次调用得到同一个向量。
// 不确定的替身会让用它的测试变成抛硬币。
func TestFakeEmbedderIsDeterministic(t *testing.T) {
	a := fake.Vector("连衣裙")
	b := fake.Vector("连衣裙")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 维两次不同: %v vs %v", i, a[i], b[i])
		}
	}
}

// 替身也不许退化成循环单条：它记下每一次调用的批，
// 好让用它的上层测试（M3 任务 3 的入库）能核对这条性质。
func TestFakeEmbedderRecordsBatches(t *testing.T) {
	var e fake.Embedder
	if _, err := e.Embed(context.Background(), []string{"a", "b", "c"}); err != nil {
		t.Fatalf("替身失败: %v", err)
	}
	if len(e.Calls) != 1 || len(e.Calls[0]) != 3 {
		t.Fatalf("替身没有把整批记成一次调用: %v", e.Calls)
	}
}
