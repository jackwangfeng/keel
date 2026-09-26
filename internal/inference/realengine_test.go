//go:build keel_real_engine

// 这个文件要**真的**推理引擎跑着，而且它对**两条腿都成立** ——
// 这是「引擎方言」配置化之后这个文件最重要的一条性质。
//
//	# infero（GPU），起法见 scripts/infero-up.sh
//	KEEL_EMBED_DIALECT=infero //	KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 make test-engine
//
//	# services/inference/（Python + BGE-M3，CPU，无 GPU 的机器走这条）
//	KEEL_EMBED_DIALECT=keel-python //	KEEL_EMBED_ENDPOINT=http://127.0.0.1:8001 make test-engine
//
// **为什么是同一组测试跑两遍，而不是给 Python 那条腿另写一份。**
// 判据本来就与方言无关：向量要归一化、语义相近的要排在无关的前面、引擎挂了
// 要报错、超批要拒。给第二条腿另写一份的话，两份会各自漂移，而漂移的方向是
// 可预测的 —— 没人跑的那一份先烂掉。上一轮 services/inference/ 之所以变成
// 「一次都没被跑过」，正是因为它一条执行者都没有。
//
// 与方言有关的只有三样（路径、model 名、池化哨兵），它们从
// inference.LookupDialect 里读，测试自己不复述 —— 复述一遍就又多了一份会漂移
// 的真相。下面那条阳性对照尤其如此：它以前硬写 `"model":"x"`，那在 infero 上
// 能过（infero 压根不读这个字段），在 Python 那条腿上是 400 —— 也就是说
// 那一行本身就是一处方言假设。
//
// 它不在 make test-db 里，理由是它要 1.2 GB 以上的权重和一个另外起着的进程
// （infero 还要一块显卡）。但它**必须存在并且真的被跑过**：这个包别的所有测试
// 用的都是假引擎，它们能证明客户端的判断力，证明不了「这套东西真的能算出
// 语义相近」。
//
// 刻意不 Skip：带上这个标签就是在声明「引擎在」。没配 KEEL_EMBED_ENDPOINT 或
// KEEL_EMBED_DIALECT 时它 Fatal 而不是 Skip——一条会自己跳过的测试，
// 在它该报警的时候是静默的。

package inference_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/inference"
)

// realEnv 取出这次跑对着的是哪个地址、哪条腿。两样都没有默认值。
func realEnv(t *testing.T) (string, inference.Dialect) {
	t.Helper()
	ep := os.Getenv(inference.EnvEndpoint)
	if ep == "" {
		t.Fatalf("带了 keel_real_engine 标签却没配 %s。"+
			"这个标签的意思是「真引擎在」，没配就是这次跑没有验证任何东西——"+
			"所以这里是 Fatal 而不是 Skip", inference.EnvEndpoint)
	}
	d, err := inference.LookupDialect(os.Getenv(inference.EnvDialect))
	if err != nil {
		t.Fatalf("%v\n（这个文件对两条腿都成立，但它猜不出对面是哪一条："+
			"路径、model 名、池化哨兵三样全靠它）", err)
	}
	t.Logf("方言 %s：path=%s model=%s sentinel=%q", d.Name, d.EmbedPath, d.ModelName,
		d.PoolingSentinel)
	return ep, d
}

func realClient(t *testing.T) *inference.Client {
	t.Helper()
	ep, d := realEnv(t)
	c, err := inference.New(inference.Config{
		Endpoint: ep, Dialect: d.Name, Timeout: 60 * time.Second})
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
// 这一组守的是**引擎进程**的行为，不是 Go 客户端的判断力 —— 那两件事此前分得很
// 清楚，唯独这一半从来没有执行者：M3 独立验收实测过，把服务端里任何一条拒绝
// 删掉，整个仓库没有一个测试会红。
//
// 为什么落在这个文件而不是写一组引擎侧的测试：这些拒绝要在**模型加载之后**
// 才走得到（它们在 /v1/embeddings 的处理函数里），所以测它们本来就需要一个真的
// 跑起来的引擎。
//
// ## 换到 infero 之后这一组改了什么，以及为什么
//
// 原来这里有三条：model 名不符要拒、normalize=false 要拒、超批要拒。
// 前两条**在 infero 上不成立，而且不是 infero 的缺陷**，所以删掉而不是留着假绿：
//
//   - **model 名不符**：infero 的 EmbeddingsRequest 把 `model` 收成
//     `Option<String>` 且标了 `#[allow(dead_code)]` —— 它压根不读这个字段，
//     一个进程只加载一个模型，请求说自己想要哪个模型没有意义。
//     Keel 这边真正需要的那道闸门因此挪到了**响应**上：client.validate 逐字比对
//     resp.Model 与 ModelName，对不上就 ErrProtocol。守的是同一件事
//     （落库的 model_name 必须真的是算这批向量的那个模型），而且守得更靠谱——
//     它核的是引擎实际加载了什么，不是请求里写了什么。那条断言在
//     client_test.go 的 TestEmbedRejectsWrongModelName，每个 PR 都跑。
//   - **normalize=false**：infero 收下这个字段但**永远归一化**，没有开关。
//     这是 keel-integration.md 明确要求的形状（「L2 normalization owned by the
//     server」），理由和 Keel 自己的理由同构：BGE-M3 时代 normalize 这个参数
//     其实一直是空转的（归一化藏在 modules.json 里），而一个「藏在上游配置里的
//     保证」在换模型那天会悄悄失效。既然引擎不提供关掉的途径，「要求它对
//     normalize=false 报 400」就是在要求一个没有意义的错误码。
//     真正要守的是「回来的向量范数是 1」，那条在下面
//     TestRealEngineReturnsNormalizedVectors，以及从数据库里读回来重算的那条
//     （internal/repository 的 TestRealEngineVectorStaysNormalizedThroughPostgres）。
//
// 剩下的两条是真的、也真的被 infero 执行：超批要拒、空输入要拒。
func TestRealEngineRefusesWhatTheProtocolSaysItShould(t *testing.T) {
	ep, dialect := realEnv(t)

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(ep, "/")+dialect.EmbedPath, strings.NewReader(body))
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

	// 阳性对照。没有它，下面两条在「引擎对任何请求都回 400」时也是绿的，
	// 而那是这个仓库反复抓到的那种假绿。
	t.Run("合法请求确实被接受", func(t *testing.T) {
		code, body := post(t, fmt.Sprintf(
			`{"model":%q,"texts":["连衣裙"],"normalize":true}`, dialect.ModelName))
		if code != http.StatusOK {
			t.Fatalf("一条合法请求回了 %d —— 下面两条拒绝断言因此没有区分力。响应:%s",
				code, body)
		}
	})

	// 正好压在上限上。这一条是上面那条阳性对照的**批量版**：没有它，
	// 「65 条要拒」在一个「32 条以上就 500」的引擎上照样是绿的，
	// 而那种引擎会让索引侧每一轮都整批失败（DefaultBatchSize 就是 64）。
	//
	// 这不是假想：infero 的 embedding 批受 --max-seqs 约束（实测上限是
	// 2 × max-seqs），--max-seqs 8 起的进程在 32 条就回
	// 「32 sequences want logits, the limit is 16」的 500。
	// 也就是说 keel-integration.md 承诺的 64 条上限，只有在
	// --max-seqs ≥ 32 时才真的兑现 —— compose.infero.yaml 里那个 --max-seqs 32
	// 不是随手填的，就是这条断言在守。
	t.Run("正好 64 条要能算完", func(t *testing.T) {
		texts := make([]string, 0, inference.DefaultBatchSize)
		for i := 0; i < inference.DefaultBatchSize; i++ {
			texts = append(texts, `"连衣裙"`)
		}
		code, body := post(t, fmt.Sprintf(`{"model":%q,"texts":[`,
			dialect.ModelName)+strings.Join(texts, ",")+`],"normalize":true}`)
		if code != http.StatusOK {
			t.Errorf("正好 %d 条（DefaultBatchSize）回了 %d，期望 200 —— "+
				"索引侧每一轮打的就是这个大小，它失败意味着整批商品都索引不上。"+
				"infero 上最可能的原因是 --max-seqs 配小了（实测批上限 = 2 × max-seqs）。响应:%s",
				inference.DefaultBatchSize, code, body)
		}
	})

	t.Run("超过批上限要拒而不是截断", func(t *testing.T) {
		texts := make([]string, 0, inference.DefaultBatchSize+1)
		for i := 0; i < inference.DefaultBatchSize+1; i++ {
			texts = append(texts, `"x"`)
		}
		code, body := post(t, fmt.Sprintf(`{"model":%q,"texts":[`,
			dialect.ModelName)+strings.Join(texts, ",")+`],"normalize":true}`)
		if code != http.StatusBadRequest {
			t.Errorf("%d 条（上限 %d）回了 %d，期望 400 —— 悄悄截断意味着调用方以为"+
				"全部算过了，而少掉的那几条会永远停在旧向量上。响应:%s",
				inference.DefaultBatchSize+1, inference.DefaultBatchSize, code, body)
		}
	})

	t.Run("空输入要拒", func(t *testing.T) {
		code, body := post(t, fmt.Sprintf(
			`{"model":%q,"texts":[],"normalize":true}`, dialect.ModelName))
		if code != http.StatusBadRequest {
			t.Errorf("空 texts 回了 %d，期望 400。响应:%s", code, body)
		}
	})
}

// 判据三：**引擎挂了必须是一个明确的错误，不能是零向量。**
//
// keel-integration.md 把这一条的后果写得很直白：一个零向量过得了形状检查、
// 进得了库，然后余弦距离对它恒等于 1 —— 它会出现在**每一次**检索的结果里，
// 而没有任何东西会报错。
//
// client_test.go 里的 TestEmbedReportsConnectionRefused 用 httptest 守过同一件事，
// 但那是对着一个「客户端自己关掉的端口」。这一条刻意放在真引擎这一组里，
// 守的是**部署形态**下的同一件事：引擎地址配了、进程没起来（或者挂了、
// 或者端口写错了），客户端必须当场报错。
//
// 端口取一个几乎不可能被占用的高位端口并先确认它真的没人听 —— 否则这条测试
// 在那个端口恰好有人监听时会变成「打了一个陌生服务」，而不是它想测的那件事。
func TestRealEngineDownIsAnErrorNotAZeroVector(t *testing.T) {
	const deadPort = "127.0.0.1:59417"
	// 先确认这个端口真的没人听。它是这条测试的前提，不是它的断言。
	if conn, err := net.DialTimeout("tcp", deadPort, 2*time.Second); err == nil {
		conn.Close()
		t.Skipf("%s 上有人在听，这条测试需要一个确实关着的端口", deadPort)
	}

	_, dialect := realEnv(t)
	c, err := inference.New(inference.Config{
		Endpoint: "http://" + deadPort,
		Dialect:  dialect.Name,
		Timeout:  3 * time.Second,
	})
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	res, err := c.Embed(context.Background(), inference.SemanticProbeTexts)

	if err == nil {
		t.Fatalf("引擎不在，Embed 却成功了，返回 %d 个向量 —— "+
			"零向量或空结果写进 product_text_vectors 之后，"+
			"余弦距离对它恒等于 1，它会出现在每一次检索里，而没有任何东西会报错",
			len(res.Vectors))
	}
	// 必须是 ErrUnavailable：§8 的降级链（退回纯关键词召回）只认这一类。
	// 归到 ErrProtocol 或 ErrRejected 的话，调用方会把它当成「重试没用」，
	// 于是引擎重启之后索引也不会自己恢复。
	if !errors.Is(err, inference.ErrUnavailable) {
		t.Fatalf("引擎不在时拿到的错误是 %v，但它不是 ErrUnavailable —— "+
			"§8 的降级链只认这一类，归错类的话调用方不会重试，"+
			"引擎重启之后索引也不会自己恢复", err)
	}
	if res != nil {
		t.Fatalf("出错时还返回了结果（%d 个向量）。半截结果的调用方要么按下标对齐"+
			"——那是一批张冠李戴的向量，要么补零——那是每次检索都会命中的零向量",
			len(res.Vectors))
	}
}
