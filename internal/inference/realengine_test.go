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
	"io"
	"math"
	"net/http"
	"os"
	"strings"
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

// 引擎自己那几条「当场拒绝」必须真的在。
//
// 这一组守的是 services/inference/app.py 的行为，而不是 Go 客户端的判断力 ——
// 那两件事此前分得很清楚，唯独这一半从来没有执行者:M3 独立验收实测过,
// 把 app.py 里任何一条拒绝删掉,整个仓库没有一个测试会红。
//
// 为什么落在这个文件而不是写一组 Python 测试:那些拒绝要在**模型加载之后**
// 才走得到(它们在 /v1/embed 的处理函数里),所以测它们本来就需要一个真的
// 跑起来的引擎。而一个「带桩模型的 app.py」意味着要在生产服务里开一个替身开关,
// 那正是这个仓库反复拒绝的形状——Go 侧用构建标签把替身关在门外,Python 侧
// 没有等价物,开了就是一个能在生产路径上被选中的开关。
//
// 这几条都是**协议层**的断言,不需要算向量,所以即使模型很慢它们也很快。
func TestRealEngineRefusesWhatTheProtocolSaysItShould(t *testing.T) {
	ep := os.Getenv(inference.EnvEndpoint)
	if ep == "" {
		t.Fatalf("带了 keel_real_engine 标签却没配 %s", inference.EnvEndpoint)
	}

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(ep, "/")+"/v1/embed", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	// 阳性对照。没有它,下面三条在「引擎对任何请求都回 400」时也是绿的,
	// 而那是这个仓库反复抓到的那种假绿。
	t.Run("合法请求确实被接受", func(t *testing.T) {
		code, body := post(t, `{"model":"bge-m3","texts":["连衣裙"],"normalize":true}`)
		if code != http.StatusOK {
			t.Fatalf("一条合法请求回了 %d —— 下面三条拒绝断言因此没有区分力。响应:%s",
				code, body)
		}
	})

	t.Run("model 名不符要拒", func(t *testing.T) {
		code, body := post(t, `{"model":"not-the-model","texts":["x"],"normalize":true}`)
		if code != http.StatusBadRequest {
			t.Errorf("model 名不符回了 %d,期望 400 —— 服务端一旦接受任意 model 名,"+
				"落库的 model_name 就不再能回答「这批向量要不要重算」。响应:%s", code, body)
		}
	})

	t.Run("normalize=false 要拒", func(t *testing.T) {
		code, body := post(t, `{"model":"bge-m3","texts":["x"],"normalize":false}`)
		if code != http.StatusBadRequest {
			t.Errorf("normalize=false 回了 %d,期望 400 —— 语义检索层 §2.3:"+
				"未归一化的向量入库**不会报错,只会悄悄拉低召回质量**。响应:%s", code, body)
		}
	})

	t.Run("超过批上限要拒而不是截断", func(t *testing.T) {
		texts := make([]string, 0, 65)
		for i := 0; i < 65; i++ {
			texts = append(texts, `"x"`)
		}
		code, body := post(t,
			`{"model":"bge-m3","texts":[`+strings.Join(texts, ",")+`],"normalize":true}`)
		if code != http.StatusBadRequest {
			t.Errorf("65 条(上限 64)回了 %d,期望 400 —— 悄悄截断意味着调用方以为"+
				"全部算过了,而少掉的那几条会永远停在旧向量上。响应:%s", code, body)
		}
	})
}
