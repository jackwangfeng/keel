package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/keel/keel/internal/inference"
)

// 这一组测试说的是哪种方言。
//
// 绝大多数断言（归一化、批量、超时分类、条数对齐）与方言无关，它们守的是
// 客户端的判断力。取 infero 只是因为那是今天的默认部署 —— 真正与方言有关的
// 那几条各自显式建客户端，见文件末尾「两条腿」那一节。
var testDialect = inference.MustDialect(inference.DialectInfero)

// ---------------------------------------------------------------------------
// 一个可编程的假引擎。它只负责**回什么**，不负责算什么——
// 这些测试守的是客户端的判断力，不是模型的效果。
// 模型效果由 realengine_test.go（keel_real_engine 标签）用真模型守。
// ---------------------------------------------------------------------------

type recordedRequest struct {
	Texts     []string
	Normalize bool
	Model     string
}

type stubEngine struct {
	mu       sync.Mutex
	requests []recordedRequest

	// respond 决定每一次请求回什么。nil 时回一批合法的归一化向量。
	respond func(w http.ResponseWriter, r *http.Request, texts []string)
}

func (s *stubEngine) calls() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedRequest(nil), s.requests...)
}

func newStub(t *testing.T, respond func(w http.ResponseWriter, r *http.Request, texts []string)) (*stubEngine, string) {
	t.Helper()
	s := &stubEngine{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testDialect.EmbedPath {
			t.Errorf("客户端打的是 %s，%s 方言的路径是 %s",
				r.URL.Path, testDialect.Name, testDialect.EmbedPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Model     string   `json:"model"`
			Texts     []string `json:"texts"`
			Normalize bool     `json:"normalize"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("请求体解不开: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			Texts: req.Texts, Normalize: req.Normalize, Model: req.Model})
		s.mu.Unlock()
		if s.respond != nil {
			s.respond(w, r, req.Texts)
			return
		}
		writeVectors(w, unitVectors(len(req.Texts)))
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// unitVectors 造 n 个合法的（1024 维、L2 范数为 1）向量。
func unitVectors(n int) [][]float32 {
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, inference.Dim)
		// 每一维相等的单位向量：每维 1/sqrt(1024) = 1/32。
		for k := range v {
			v[k] = 1.0 / 32.0
		}
		out[i] = v
	}
	return out
}

func writeVectors(w http.ResponseWriter, vecs [][]float32) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"embeddings":    vecs,
		"dim":           inference.Dim,
		"model":         testDialect.ModelName,
		"model_version": "stub-0001",
	})
}

func mustClient(t *testing.T, cfg inference.Config) *inference.Client {
	t.Helper()
	c, err := inference.New(cfg)
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// 归一化：这个包存在的头号理由
// ---------------------------------------------------------------------------

// 语义检索层 §2.3：
//
//	向量必须 L2 归一化后写入，配合 vector_cosine_ops 使用……
//	写入未归一化的向量会让距离计算失真，**且这种错误不会报错、
//	只会悄悄拉低召回质量**。
//
// 这条测试守的就是那句「不会报错」。请求里带着 normalize: true，
// 但那只是一句请求——引擎把参数读丢了、换了个不带归一化池化的模型、
// 中间有人加了一层做后处理的代理，三种都不会让任何一方报错。
// 所以客户端自己算范数，算出来不是 1 就拒收。
func TestEmbedRejectsUnnormalizedVectors(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		vecs := unitVectors(len(texts))
		// 把第一条放大 3 倍：方向没变（余弦检索「看起来」还对），
		// 范数变成 3。这正是未归一化向量的样子。
		for k := range vecs[0] {
			vecs[0][k] *= 3
		}
		writeVectors(w, vecs)
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})

	res, err := c.Embed(context.Background(), []string{"连衣裙"})
	if err == nil {
		t.Fatalf("引擎回了一个 L2 范数为 3 的向量，客户端收下了。"+
			"它会被写进 product_text_vectors 并配 vector_cosine_ops 使用——"+
			"距离计算从此失真，而不会有任何东西报错（语义检索层 §2.3）。res=%+v", res)
	}
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("要 ErrProtocol（重试没用，是引擎和 DDL 对不上），拿到 %v", err)
	}
	if !strings.Contains(err.Error(), "L2 范数") {
		t.Fatalf("错误信息没说清是归一化出的问题，排查会跑偏: %v", err)
	}
	if res != nil {
		t.Fatalf("出错时还返回了向量，它们会被写进库: %+v", res)
	}
}

// 零向量是未归一化的极端形态，单独立一条：它的范数是 0，
// 而余弦距离对零向量恒等于 1——它会出现在**每一次**检索的结果里。
func TestEmbedRejectsZeroVector(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		vecs := make([][]float32, len(texts))
		for i := range vecs {
			vecs[i] = make([]float32, inference.Dim) // 全 0
		}
		writeVectors(w, vecs)
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})

	if _, err := c.Embed(context.Background(), []string{"连衣裙"}); err == nil {
		t.Fatal("零向量被收下了。它入库之后余弦距离恒等于 1，" +
			"会污染每一次检索的结果，而没有任何东西会报错")
	} else if !strings.Contains(err.Error(), "L2 范数") {
		t.Fatalf("错误信息没指向归一化: %v", err)
	}
}

// 正向：合法的归一化向量要能过。
// 没有这一条的话，把 validate 改成「无条件报错」也能让上面两条绿。
func TestEmbedAcceptsNormalizedVectors(t *testing.T) {
	_, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})

	res, err := c.Embed(context.Background(), []string{"连衣裙", "长裙"})
	if err != nil {
		t.Fatalf("合法的归一化向量被拒了: %v", err)
	}
	if len(res.Vectors) != 2 {
		t.Fatalf("要 2 个向量，拿到 %d 个", len(res.Vectors))
	}
	for i, v := range res.Vectors {
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if n := math.Sqrt(sum); math.Abs(n-1) > inference.NormTolerance {
			t.Fatalf("第 %d 个向量的范数是 %v", i, n)
		}
	}
	// §10：模型名与版本随响应返回，它们的去处是
	// product_text_vectors.model_name / model_version。
	if res.Model != testDialect.ModelName || res.ModelVersion != "stub-0001" {
		t.Fatalf("模型名/版本没有透出来: %q / %q", res.Model, res.ModelVersion)
	}
}

// 请求里必须真的带 normalize: true。客户端自己核范数是第二道闸，
// 不是替代品——第一道还是要让引擎知道该归一化。
func TestEmbedAsksEngineToNormalize(t *testing.T) {
	stub, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	if _, err := c.Embed(context.Background(), []string{"连衣裙"}); err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("要 1 次请求，拿到 %d 次", len(calls))
	}
	if !calls[0].Normalize {
		t.Fatal("请求里 normalize 不是 true（语义检索层 §10 的报文）")
	}
	if calls[0].Model != testDialect.ModelName {
		t.Fatalf("请求里 model 是 %q，要 %q", calls[0].Model, testDialect.ModelName)
	}
}

// 每一条送出去的文本都必须带着池化哨兵，而且**是每一条**。
//
// 这一条守的不是「哨兵这个常量还在不在」，是「它有没有真的被拼到每条文本尾巴上」。
// 漏一条的后果不会报错：那一条的向量取自正文末字而不是 EOS，于是它和同一批
// 别的向量根本不在一个空间里，余弦距离照算不误，只是算出来的东西没有意义。
//
// 送两条不同长度的文本，是为了排除「只给第一条拼了」这种写法（那种写法在
// 单条输入的测试里是绿的）。原文用 strings.CutSuffix 还原并逐字比对，
// 是为了排除「拼错位置」与「顺手改了正文」。
func TestEmbedAppendsPoolingSentinelToEveryText(t *testing.T) {
	stub, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	want := []string{"红色碎花连衣裙 女装 夏季新款", "轮胎"}
	if _, err := c.Embed(context.Background(), want); err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	got := stub.calls()[0].Texts
	if len(got) != len(want) {
		t.Fatalf("送了 %d 条，引擎收到 %d 条", len(want), len(got))
	}
	for i, sent := range got {
		stripped, ok := strings.CutSuffix(sent, testDialect.PoolingSentinel)
		if !ok {
			t.Errorf("第 %d 条 %q 末尾没有池化哨兵 %q —— Qwen3-Embedding 取的是"+
				"最后一个 token 的 hidden state，而 infero 不执行 checkpoint 自己的"+
				"post_processor，少了这个哨兵池化就取到了正文末字（实测 margin "+
				"从 +0.2917 塌到 +0.0960，低于 MinSemanticMargin）",
				i, sent, testDialect.PoolingSentinel)
			continue
		}
		if stripped != want[i] {
			t.Errorf("第 %d 条剥掉哨兵之后是 %q，原文是 %q —— 正文被改动了",
				i, stripped, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 超时与降级：引擎挂了要拿到一个**明确的**错误
// ---------------------------------------------------------------------------

// §8 的降级链：「向量服务不可用 → 纯关键词 + 业务重排」。
// 那条链的入口是一个 error。返回空向量或零向量的话，降级永远不会被触发，
// 而那些值会被当成正常结果写进库。
func TestEmbedTimesOutWithExplicitError(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, r *http.Request, texts []string) {
		<-r.Context().Done() // 永远不回，直到客户端放弃
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name, Timeout: 80 * time.Millisecond})

	start := time.Now()
	res, err := c.Embed(context.Background(), []string{"连衣裙"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("引擎一直不回，而 Embed 返回了成功。res=%+v", res)
	}
	if res != nil {
		t.Fatalf("超时了还返回向量，它们会被写进库: %+v", res)
	}
	if !errors.Is(err, inference.ErrUnavailable) {
		t.Fatalf("要 ErrUnavailable（§8 降级链的入口），拿到 %v", err)
	}
	// 区分「超时」与「别的地方也坏了」：只有 context.DeadlineExceeded
	// 能证明红的是超时这条路径，而不是顺带被别的失败带红的。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误里没有 context.DeadlineExceeded，说明它不是超时来的: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Timeout 配的是 80ms，实际等了 %v —— 单次上限没有生效", elapsed)
	}
}

// 引擎根本没起来。和超时是两个原因、同一个降级动作，
// 所以错误里既要有 ErrUnavailable 也要留着真因。
func TestEmbedReportsConnectionRefused(t *testing.T) {
	_, url := newStub(t, nil)
	// 拿一个刚关掉的地址：端口上没人听。
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead := srv.URL
	srv.Close()
	_ = url

	c := mustClient(t, inference.Config{Endpoint: dead, Dialect: testDialect.Name, Timeout: time.Second})
	res, err := c.Embed(context.Background(), []string{"连衣裙"})
	if err == nil {
		t.Fatalf("引擎没起来而 Embed 成功了: %+v", res)
	}
	if res != nil {
		t.Fatalf("失败时返回了向量: %+v", res)
	}
	if !errors.Is(err, inference.ErrUnavailable) {
		t.Fatalf("要 ErrUnavailable，拿到 %v", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("错误里没留下 ECONNREFUSED，排查时分不清是超时还是没人听: %v", err)
	}
}

// 引擎自己超时后按 §10 立刻返回 504。那也是降级链的入口，不是「请求非法」。
func TestEmbedTreatsServerSideTimeoutAsUnavailable(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ []string) {
		w.WriteHeader(http.StatusGatewayTimeout)
		_, _ = w.Write([]byte(`{"error":"超过服务端预算 5000ms"}`))
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙"})
	if !errors.Is(err, inference.ErrUnavailable) {
		t.Fatalf("504 要按 ErrUnavailable 处理（§10：服务端超时后立刻返回，"+
			"由调用方走降级链），拿到 %v", err)
	}
}

// 引擎明确拒绝（4xx）是调用方的问题，重试没用。分开是为了让上层不要傻重试。
func TestEmbedTreatsRejectionAsPermanent(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, _ []string) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"一次最多 64 条"}`))
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙"})
	if !errors.Is(err, inference.ErrRejected) {
		t.Fatalf("4xx 要按 ErrRejected（重试没用）处理，拿到 %v", err)
	}
	if errors.Is(err, inference.ErrUnavailable) {
		t.Fatalf("4xx 不该被当成「引擎不可用」而触发重试: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 批量：§10 第一条「批量接口，禁止循环单条调用」
// ---------------------------------------------------------------------------

// 100 条文本 → 2 次请求（64 + 36），不是 100 次。
//
// 这条性质只能在**请求次数**上观察。断言「结果有 100 个向量」是看不出来的：
// 循环单条也能拼出 100 个向量，而且全都合法。
func TestEmbedSendsOneRequestPerBatch(t *testing.T) {
	stub, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})

	texts := make([]string, 100)
	for i := range texts {
		texts[i] = "商品标题"
	}
	res, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if len(res.Vectors) != 100 {
		t.Fatalf("要 100 个向量，拿到 %d 个", len(res.Vectors))
	}

	calls := stub.calls()
	if len(calls) != 2 {
		t.Fatalf("100 条文本、批大小 %d，应当发 2 次请求，实际发了 %d 次。"+
			"发 %d 次说明批量退化成了循环单条——语义检索层 §10 第一条"+
			"「批量接口，禁止循环单条调用」禁的就是它",
			inference.DefaultBatchSize, len(calls), len(calls))
	}
	if len(calls[0].Texts) != 64 || len(calls[1].Texts) != 36 {
		t.Fatalf("两批的大小是 %d / %d，要 64 / 36", len(calls[0].Texts), len(calls[1].Texts))
	}
}

// 一批之内不许拆。批大小 32 时 32 条文本必须是一次请求。
func TestEmbedSendsExactlyOneRequestWhenBatchFits(t *testing.T) {
	stub, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name, BatchSize: 32})

	texts := make([]string, 32)
	for i := range texts {
		texts[i] = "商品标题"
	}
	if _, err := c.Embed(context.Background(), texts); err != nil {
		t.Fatalf("Embed 失败: %v", err)
	}
	if n := len(stub.calls()); n != 1 {
		t.Fatalf("32 条文本、批大小 32，应当是 1 次请求，实际 %d 次", n)
	}
}

// 批大小不许超过 §10 的上限。
func TestNewRejectsOversizedBatch(t *testing.T) {
	if _, err := inference.New(inference.Config{
		Endpoint: "http://x", Dialect: testDialect.Name,
		BatchSize: inference.DefaultBatchSize + 1,
	}); err == nil {
		t.Fatal("批大小超过 64 被放行了（§10：索引侧批大小 32–64）")
	}
}

// ---------------------------------------------------------------------------
// 别的几条「不会报错的错误」
// ---------------------------------------------------------------------------

func TestEmbedRejectsWrongDimension(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		// 768 维（图像那张表的维度）配 1024 的列，INSERT 会被 Postgres 拒掉，
		// 但那要等到入库那一步，报错指向写入代码而不是这里。
		v := make([]float32, 768)
		for k := range v {
			v[k] = 1.0 / float32(math.Sqrt(768))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{v}, "dim": 768,
			"model": testDialect.ModelName, "model_version": "stub-0001",
		})
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙"})
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("768 维被放行了（product_text_vectors.embedding 是 vector(1024)）: %v", err)
	}
}

func TestEmbedRejectsCountMismatch(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		writeVectors(w, unitVectors(len(texts)-1)) // 少回一个
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙", "长裙"})
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("条数对不上被放行了，按下标对齐就是一批张冠李戴的向量: %v", err)
	}
}

func TestEmbedRejectsMissingModelVersion(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": unitVectors(len(texts)), "dim": inference.Dim,
			"model": testDialect.ModelName, // 没有 model_version
		})
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙"})
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("没有 model_version 被放行了。§10 要求模型名与版本随响应返回，"+
			"它是 product_text_vectors.model_version 的来源，"+
			"没有它就没法回答「这批向量要不要重算」: %v", err)
	}
}

func TestEmbedRejectsWrongModelName(t *testing.T) {
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": unitVectors(len(texts)), "dim": inference.Dim,
			"model": "text-embedding-3-small", "model_version": "x",
		})
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	_, err := c.Embed(context.Background(), []string{"连衣裙"})
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("别的模型的 1024 维向量被放行了。它们混进同一张表之后余弦距离"+
			"照算不误，没有任何东西会报错: %v", err)
	}
}

// 跨批换模型：这一批向量来自两个模型，而它们会被记成同一个 model_version。
func TestEmbedRejectsModelChangeBetweenBatches(t *testing.T) {
	var n int
	var mu sync.Mutex
	_, url := newStub(t, func(w http.ResponseWriter, _ *http.Request, texts []string) {
		mu.Lock()
		n++
		version := "stub-0001"
		if n > 1 {
			version = "stub-0002" // 第二批时引擎被重新部署了
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": unitVectors(len(texts)), "dim": inference.Dim,
			"model": testDialect.ModelName, "model_version": version,
		})
	})
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name, BatchSize: 2})
	_, err := c.Embed(context.Background(), []string{"a", "b", "c"})
	if !errors.Is(err, inference.ErrProtocol) {
		t.Fatalf("同一次调用里换了模型版本却放行了，"+
			"前半截向量从此没人知道该重算: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 引擎地址没有默认值
// ---------------------------------------------------------------------------

// 与 KEEL_DTM_DSN 同构（见 internal/app 的 EnvDTMDSN）：
// 一个「反正能跑起来」的默认值，会让忘了配它的部署安静地拿到坏结局。
func TestNewRefusesEmptyEndpoint(t *testing.T) {
	c, err := inference.New(inference.Config{})
	if err == nil {
		t.Fatalf("空 Endpoint 建出了一个客户端 %+v —— "+
			"那意味着某个默认地址正在生效，而它不属于任何一次部署的决定", c)
	}
	if !strings.Contains(err.Error(), inference.EnvEndpoint) {
		t.Fatalf("错误信息没提 %s，运维不知道该配什么: %v", inference.EnvEndpoint, err)
	}
}

func TestFromEnvRefusesWhenUnset(t *testing.T) {
	t.Setenv(inference.EnvDialect, inference.DialectInfero)
	t.Setenv(inference.EnvEndpoint, "")
	if _, err := inference.FromEnv(); err == nil {
		t.Fatalf("%s 没配而 FromEnv 成功了", inference.EnvEndpoint)
	}
	t.Setenv(inference.EnvEndpoint, "http://inference:8000")
	t.Setenv(inference.EnvDialect, inference.DialectInfero)
	if _, err := inference.FromEnv(); err != nil {
		t.Fatalf("配了 %s 反而失败: %v", inference.EnvEndpoint, err)
	}
}

func TestEmbedRejectsEmptyInput(t *testing.T) {
	_, url := newStub(t, nil)
	c := mustClient(t, inference.Config{Endpoint: url, Dialect: testDialect.Name})
	if _, err := c.Embed(context.Background(), nil); !errors.Is(err, inference.ErrRejected) {
		t.Fatalf("空输入应当直接拒绝而不是打一次空请求: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 两条腿：引擎方言是配置，不是常量
// ---------------------------------------------------------------------------

// newDialectStub 起一个**只认某一条方言**的假引擎：路径不对回 404，
// model 不对回 400 —— 两条都照着 services/inference/app.py 的真实行为来。
//
// 「路径不对回 404」这一条是刻意的：它复现了方言配错时**生产上真正会发生的
// 那件事**。上面那个 newStub 在路径不对时 t.Errorf，那是测试基建的自检；
// 这里要的是引擎的回答，因为被测的正是「客户端有没有按配置去打对的地方」。
func newDialectStub(t *testing.T, d inference.Dialect) (*stubEngine, string) {
	t.Helper()
	s := &stubEngine{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != d.EmbedPath {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
			return
		}
		var req struct {
			Model     string   `json:"model"`
			Texts     []string `json:"texts"`
			Normalize bool     `json:"normalize"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{
			Texts: req.Texts, Normalize: req.Normalize, Model: req.Model})
		s.mu.Unlock()
		if req.Model != d.ModelName {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"本服务只提供别的模型"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings":    unitVectors(len(req.Texts)),
			"dim":           inference.Dim,
			"model":         d.ModelName,
			"model_version": "stub-0001",
		})
	}))
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// 每一条已知方言，配上它自己那个引擎，都必须能算出向量。
//
// 这是 keel-python 那条腿在这个仓库里的**第一条执行者**：上一轮把客户端换成
// 只会说 infero 的方言之后，services/inference/ 一个调用方、一条测试都没有了。
// 这一条不需要模型（假引擎回的是形状合法的单位向量），所以它每个 PR 都跑；
// 「那条腿真的能算出有语义的向量」由 realengine_test.go 对着真服务跑，
// 两条一起才完整。
func TestEveryDialectTalksToItsOwnEngine(t *testing.T) {
	for _, name := range inference.DialectNames() {
		t.Run(name, func(t *testing.T) {
			d := inference.MustDialect(name)
			stub, url := newDialectStub(t, d)
			c := mustClient(t, inference.Config{Endpoint: url, Dialect: name})

			res, err := c.Embed(context.Background(), []string{"红色碎花连衣裙"})
			if err != nil {
				t.Fatalf("%s 方言配着它自己的引擎却算不出向量: %v", name, err)
			}
			if len(res.Vectors) != 1 {
				t.Fatalf("要 1 个向量，拿到 %d 个", len(res.Vectors))
			}
			if res.Model != d.ModelName {
				t.Fatalf("model 是 %q，方言说是 %q", res.Model, d.ModelName)
			}
			if got := c.ModelName(); got != d.ModelName {
				t.Fatalf("ModelName() 报 %q，而落库的是 %q —— "+
					"重算判定（service/index.go 的 decide）用的是前者", got, d.ModelName)
			}

			// 送出去的正文：**只有方言说要补的时候才补**。
			sent := stub.calls()[0].Texts[0]
			want := "红色碎花连衣裙" + d.PoolingSentinel
			if sent != want {
				t.Fatalf("%s 方言送出去的是 %q，期望 %q", name, sent, want)
			}
		})
	}
}

// 方言配错了要打不通，而不是「碰巧也能跑」。
//
// 这一条守的是方言这件事**真的在起作用**。没有它，把 EmbedPath / ModelName
// 从方言表里读改回硬编码，上面那条参数化测试在 infero 那一格照样绿
// （因为硬编码的就是 infero 的值），只有 keel-python 那一格会红 ——
// 而那时最自然的「修法」是把 keel-python 从表里删掉。
func TestWrongDialectDoesNotSilentlyWork(t *testing.T) {
	names := inference.DialectNames()
	for _, engine := range names {
		for _, configured := range names {
			if engine == configured {
				continue
			}
			t.Run(engine+"/配成"+configured, func(t *testing.T) {
				_, url := newDialectStub(t, inference.MustDialect(engine))
				c := mustClient(t, inference.Config{Endpoint: url, Dialect: configured})
				res, err := c.Embed(context.Background(), []string{"连衣裙"})
				if err == nil {
					t.Fatalf("对着 %s 的引擎配了 %s 的方言，却拿到了 %d 个向量 —— "+
						"那批向量会被记成 %q 写进 product_text_vectors，"+
						"而算它们的是另一个模型",
						engine, configured, len(res.Vectors), c.ModelName())
				}
			})
		}
	}
}

// 方言表自己的一致性：Name 那一格必须等于它在表里的键。
//
// 它们对不上时不会有任何东西报错 —— 只是错误信息里会印出另一条腿的名字，
// 而那是排查方言问题时唯一的线索。
func TestDialectTableIsSelfConsistent(t *testing.T) {
	for _, name := range inference.DialectNames() {
		d := inference.MustDialect(name)
		if d.Name != name {
			t.Errorf("方言 %q 的 Name 那一格写的是 %q", name, d.Name)
		}
		if d.EmbedPath == "" || d.ModelName == "" {
			t.Errorf("方言 %q 缺路径或模型名: %+v", name, d)
		}
		if d.EmbedPath[0] != '/' {
			t.Errorf("方言 %q 的路径 %q 不是以 / 开头 —— 它是直接拼在 endpoint "+
				"后面的", name, d.EmbedPath)
		}
	}
	// 自证：表里至少有两条，而且 infero 与 keel-python 都在。
	// 只剩一条时上面那个循环仍然全绿，而这个仓库刚刚才因为「只剩一条」
	// 丢掉了无 GPU 部署的语义检索。
	if n := len(inference.DialectNames()); n < 2 {
		t.Fatalf("方言表里只有 %d 条。两条腿（GPU / 无 GPU）都要活着，"+
			"这是架构 §6 形态 A 的承诺", n)
	}
	for _, want := range []string{inference.DialectInfero, inference.DialectKeelPython} {
		if !slices.Contains(inference.DialectNames(), want) {
			t.Fatalf("方言表里没有 %q。已知的：%v", want, inference.DialectNames())
		}
	}
}

// 两条腿的池化哨兵必须**不同**，而且 keel-python 那条必须是空的。
//
// 这条断言看起来很怪（为什么要求两个常量不相等？），但它守的正是
// dialects 那张表里论证最长的那一格：`<|endoftext|>` 不在 XLM-R 的词表里，
// 补给 BGE-M3 等于给每条商品文本尾部拼一段固定噪声 —— 维度对、范数是 1、
// model 名对、HTTP 200，这个包的每一道闸门都绿，只有召回质量安静地掉一截。
// 「顺手让两条腿共用一个哨兵」是一次非常自然的清理，而它不会被别的任何东西抓住。
func TestPoolingSentinelIsPerDialect(t *testing.T) {
	infero := inference.MustDialect(inference.DialectInfero)
	python := inference.MustDialect(inference.DialectKeelPython)
	if python.PoolingSentinel != "" {
		t.Errorf("keel-python 那条腿的哨兵是 %q，应当是空串："+
			"BGE-M3 用 CLS 池化（向量取自序列第一个位置），往尾巴上补什么都改变"+
			"不了它取到的那一行，而 %q 不在 XLM-R 的词表里，会被切成几个普通 "+
			"subword 跟在正文后面 —— 相当于给每条商品文本拼一段固定噪声",
			python.PoolingSentinel, infero.PoolingSentinel)
	}
	if infero.PoolingSentinel == "" {
		t.Errorf("infero 那条腿的哨兵是空的。Qwen3-Embedding 是 last-token 池化，"+
			"而 infero 不执行 checkpoint 自己的 post_processor —— "+
			"不补哨兵实测 margin 从 +0.2917 塌到 +0.0960，低于 %.2f 的下限",
			inference.MinSemanticMargin)
	}
}

// ---------------------------------------------------------------------------
// 方言没有默认值
// ---------------------------------------------------------------------------

// 与 Endpoint 同一条纪律，而且理由更硬：地址配错是 connection refused（吵），
// 方言**配漏**要是有默认值，症状会是「向量算出来了、入库了、检索照常返回
// 结果，只是结果和搜的词无关」。
func TestNewRefusesEmptyDialect(t *testing.T) {
	c, err := inference.New(inference.Config{Endpoint: "http://engine:8000"})
	if err == nil {
		t.Fatalf("没配方言却建出了一个客户端 %+v —— "+
			"那意味着某条腿正在被当成默认，而选错了不会报错", c)
	}
	if !errors.Is(err, inference.ErrDialectUnset) {
		t.Fatalf("要 ErrDialectUnset（装配方靠它把「一个字都没配」和「配漏了」"+
			"分开处置，见 internal/app 的 Run），拿到 %v", err)
	}
	if !strings.Contains(err.Error(), inference.EnvDialect) {
		t.Fatalf("错误信息没提 %s，运维不知道该配什么: %v", inference.EnvDialect, err)
	}
	for _, name := range inference.DialectNames() {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("错误信息里没有已知方言 %q，运维不知道能填什么: %v", name, err)
		}
	}
}

func TestNewRefusesUnknownDialect(t *testing.T) {
	_, err := inference.New(inference.Config{
		Endpoint: "http://engine:8000", Dialect: "openai"})
	if err == nil {
		t.Fatal("不认识的方言被放行了 —— 那会在运行期变成一次 404，" +
			"而索引是异步的，没有人在等它的返回码")
	}
	if !errors.Is(err, inference.ErrRejected) {
		t.Fatalf("要 ErrRejected（是调用方写错了，重试没用），拿到 %v", err)
	}
	if !strings.Contains(err.Error(), inference.DialectKeelPython) {
		t.Fatalf("错误信息没列出已知方言: %v", err)
	}
}

// 「地址配了、方言没配」必须能被装配方单独认出来 —— 它与「一个字都没配」
// 处置不同：前者拒绝启动，后者打一条 WARN 继续起（§8 的纯关键词降级链）。
func TestFromEnvSeparatesMissingEndpointFromMissingDialect(t *testing.T) {
	t.Run("一个字都没配", func(t *testing.T) {
		t.Setenv(inference.EnvEndpoint, "")
		t.Setenv(inference.EnvDialect, "")
		_, err := inference.FromEnv()
		if !errors.Is(err, inference.ErrEndpointUnset) {
			t.Fatalf("要 ErrEndpointUnset，拿到 %v", err)
		}
		if errors.Is(err, inference.ErrDialectUnset) {
			t.Fatal("同时被归成「方言没配」了 —— 装配方会因此拒绝启动，" +
				"而「没有引擎」是一种受支持的部署（README「还没在盒子里的」）")
		}
	})
	t.Run("地址配了方言没配", func(t *testing.T) {
		t.Setenv(inference.EnvEndpoint, "http://engine:8000")
		t.Setenv(inference.EnvDialect, "")
		_, err := inference.FromEnv()
		if !errors.Is(err, inference.ErrDialectUnset) {
			t.Fatalf("要 ErrDialectUnset，拿到 %v", err)
		}
		if errors.Is(err, inference.ErrEndpointUnset) {
			t.Fatal("被归成「地址没配」了 —— 装配方会只打一条 WARN 继续起，" +
				"而这次部署明明想要语义检索")
		}
	})
	t.Run("两样都配了", func(t *testing.T) {
		t.Setenv(inference.EnvEndpoint, "http://engine:8000")
		t.Setenv(inference.EnvDialect, inference.DialectKeelPython)
		c, err := inference.FromEnv()
		if err != nil {
			t.Fatalf("两样都配了反而失败: %v", err)
		}
		if c.Dialect().Name != inference.DialectKeelPython {
			t.Fatalf("FromEnv 建出来的客户端说的是 %q 方言", c.Dialect().Name)
		}
	})
}
