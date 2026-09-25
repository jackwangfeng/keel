//go:build keel_real_engine

// 这个文件要**真的**推理引擎跑着。跑法：
//
//	KEEL_EMBED_ENDPOINT=http://127.0.0.1:8081 \
//	    go test -tags keel_real_engine -count=1 ./internal/inference/
//
// 它不在 make test-db 里，理由是它要 2.2 GB 权重与一个起了几十秒的进程。
// 但它**必须存在并且真的被跑过**：这个包别的所有测试用的都是假引擎，
// 它们能证明客户端的判断力，证明不了「这套东西真的能算出语义相近」。
//
// 刻意不 Skip：带上这个标签就是在声明「引擎在」。没配 KEEL_EMBED_ENDPOINT 时
// 它 Fatal 而不是 Skip——一条会自己跳过的测试，在它该报警的时候是静默的。

package inference_test

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/keel/keel/internal/inference"
)

func realClient(t *testing.T) *inference.Client {
	t.Helper()
	ep := os.Getenv(inference.EnvEndpoint)
	if ep == "" {
		t.Fatalf("带了 keel_real_engine 标签却没配 %s。"+
			"这个标签的意思是「真引擎在」，没配就是这次跑没有验证任何东西——"+
			"所以这里是 Fatal 而不是 Skip", inference.EnvEndpoint)
	}
	c, err := inference.New(inference.Config{Endpoint: ep, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return c
}

// 真引擎返回的向量，L2 范数必须就是 1。
//
// 这一条和 TestEmbedRejectsUnnormalizedVectors 守的不是同一件事：
// 那条守「客户端会不会拒收未归一化的向量」（假引擎就够了），
// 这条守「真引擎到底归没归一化」。语义检索层 §2.3 说得很清楚，
// 未归一化的向量入库不会报错、只会悄悄拉低召回——而 normalize: true
// 这个参数传过去了，不等于它被执行了。
func TestRealEngineReturnsNormalizedVectors(t *testing.T) {
	c := realClient(t)
	res, err := c.Embed(context.Background(), inference.SemanticProbeTexts)
	if err != nil {
		t.Fatalf("真引擎调用失败: %v", err)
	}
	for i, v := range res.Vectors {
		if len(v) != inference.Dim {
			t.Fatalf("第 %d 条是 %d 维，而 product_text_vectors.embedding 是 vector(%d)",
				i, len(v), inference.Dim)
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		norm := math.Sqrt(sum)
		if math.Abs(norm-1) > inference.NormTolerance {
			t.Fatalf("第 %d 条向量的 L2 范数是 %.8f，不是 1", i, norm)
		}
		t.Logf("第 %d 条 L2 范数 = %.8f", i, norm)
	}
	t.Logf("model=%s model_version=%s", res.Model, res.ModelVersion)
	if res.ModelVersion == "" {
		t.Fatal("真引擎没有返回 model_version")
	}
}

// M3 的验收标准在这里：**搜「连衣裙」能返回相关商品。**
//
// 三条文本，前两条都是裙子、第三条是轮胎。真引擎必须把「裙-裙」的相似度
// 拉到比「裙-轮胎」高出至少 MinSemanticMargin。替身做不到这一条
// （internal/inference/fake 里那条测试断言的正是它做不到）。
func TestRealEngineRanksSemanticNeighborsAboveUnrelated(t *testing.T) {
	c := realClient(t)
	res, err := c.Embed(context.Background(), inference.SemanticProbeTexts)
	if err != nil {
		t.Fatalf("真引擎调用失败: %v", err)
	}
	s, err := inference.SemanticMargin(res.Vectors)
	if err != nil {
		t.Fatalf("算相似度失败: %v", err)
	}
	for i, row := range s.Matrix {
		t.Logf("%-28s %7.4f %7.4f %7.4f",
			inference.SemanticProbeTexts[i], row[0], row[1], row[2])
	}
	t.Logf("近-近 = %.4f，近-远 = %.4f，margin = %.4f（下限 %.2f）",
		s.NearNear, s.NearFar, s.Margin, inference.MinSemanticMargin)

	if s.Margin < inference.MinSemanticMargin {
		t.Fatalf("真引擎没能把语义相近的两条排在无关的那条前面："+
			"cos(裙,裙)=%.4f，cos(裙,轮胎)=%.4f，margin=%.4f < %.2f。"+
			"这是 M3 的验收标准（搜「连衣裙」能返回相关商品）——"+
			"margin 塌到 0 附近就说明这里跑的其实是个伪 embedding",
			s.NearNear, s.NearFar, s.Margin, inference.MinSemanticMargin)
	}
}
