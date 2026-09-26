package service_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/service"
)

// 派生数据入库（M3 Task 3）的行为闸门。
//
// 这一组测试要能区分的是两件长得很像的事：
//
//	· 「改了标题，向量重算了」—— 判据的触发点与判定都在工作；
//	· 「改了 sales_count，向量**没有**重算」—— 判定那一半真的在挡。
//
// 第二条是本任务判据的全部意义（00016 文件头第四节）。它最容易假绿：
// 一个「什么都没扫到」的实现也满足「没有重算」。所以那条测试同时断言
// **触发点确实捞到了这件商品**（Judged == 1）—— 也就是说它被判定过、
// 并且判定的结论是不用算。少了这半句，把整条任务注释掉也照绿。

// TestMain 保证库里有 schema。形状与理由逐字同 internal/repository/tenant_test.go：
// 无条件重建，因为跳过会让可变状态跨轮次累积，而「删掉被守护的那段逻辑、
// 看它红不红」这套方法的地基就是每一轮从同一个起点开始。
func TestMain(m *testing.M) {
	if err := ensureSchema(); err != nil {
		fmt.Fprintf(os.Stderr, "准备 schema 失败: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ensureSchema() error {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx,
		`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		return err
	}
	out, err := exec.Command("make", "-C", "../..", "migrate",
		"GOOSE_DBSTRING="+db.AdminDSN()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 替身：一个只管形状、不管语义的 embedder
// ---------------------------------------------------------------------------

// stubEmbedder 是**本文件局部**的替身，不是 internal/inference/fake。
//
// 为什么不用那个包：它带 keel_fake_embedder 编译标签（刻意的，见它的 doc.go），
// 而 Makefile 只在 ./internal/inference/... 上带那个标签跑。把它拉进来会逼着
// 这一组测试也只在那个标签下跑，于是它们默认不在闸门里 —— 一条不在闸门里的
// 测试等于没有。
//
// 一个 _test.go 里的类型在默认构建里同样够不着（测试文件不进生产二进制），
// 所以「替身不许在生产路径上被选中」这条纪律照旧成立。
//
// 它产出确定性的单位向量：维度对、范数是 1、同样输入同样输出。它**没有语义**，
// 这一组测试也不需要语义 —— 语义那一条由真引擎的 realengine_test.go 守着。
type stubEmbedder struct {
	calls   [][]string
	err     error
	version string

	// beforeReturn 在返回之前跑一次。测试用它注入「判定与写回之间商品又变了」
	// 那条竞态 —— 那段窗口的现实原因正是这一次 HTTP 调用的耗时。
	beforeReturn func()
}

func (e *stubEmbedder) Embed(_ context.Context, texts []string) (*inference.Result, error) {
	e.calls = append(e.calls, append([]string(nil), texts...))
	if e.beforeReturn != nil {
		e.beforeReturn()
	}
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = stubVector(t)
	}
	v := e.version
	if v == "" {
		v = "stub-v1"
	}
	return &inference.Result{Vectors: out, Model: inference.ModelName, ModelVersion: v}, nil
}

func stubVector(text string) []float32 {
	r := rand.New(rand.NewChaCha8(sha256.Sum256([]byte(text))))
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

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

// fixtureRepo 把 ActiveMerchants 换成夹具里那几家。
//
// 为什么不直接用真的 ActiveMerchants：它只认 status = 1 的商家，而本仓库的
// 夹具一律用 status = 2（停用）—— 理由写在 seedTwoTenants 与 semanticFixture 里：
// tenant.Preflight 断言的是**整个库**的形态（配了默认商家时活跃商家只能有一家），
// 插一家活跃商家会在别的包里表现为一次随机的断言失败。
//
// 换掉它不削弱这组测试：「定时任务的租户从枚举 merchants 来」这件事是由
// IndexRepository 这个接口的形状保证的（service 除了 WithTenant 只有它），
// 而那条真实查询由 repository 自己的测试与超时补偿任务共同覆盖。
type fixtureRepo struct {
	*repository.Repo
	merchants []int64
}

func (r fixtureRepo) ActiveMerchants(context.Context) ([]int64, error) {
	return r.merchants, nil
}

type indexFixture struct {
	admin     *pgx.Conn
	pool      *pgxpool.Pool
	repo      fixtureRepo
	emb       *stubEmbedder
	svc       *service.IndexService
	merchants []int64
	// products[merchantID] = 按插入顺序的商品 id
	products map[int64][]int64
	catID    map[int64]int64
}

// newIndexFixture 播 n 家商家，每家 perMerchant 件在架商品。
func newIndexFixture(t *testing.T, tag string, n, perMerchant int, cfg service.IndexConfig) *indexFixture {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	f := &indexFixture{
		admin:    admin,
		products: map[int64][]int64{},
		catID:    map[int64]int64{},
		emb:      &stubEmbedder{},
	}
	suffix := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
	for i := 0; i < n; i++ {
		var mid int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO merchants (code, name, status) VALUES ($1,'M',2) RETURNING id`,
			fmt.Sprintf("%s-%d", suffix, i)).Scan(&mid); err != nil {
			t.Fatal(err)
		}
		f.merchants = append(f.merchants, mid)
		var cat int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'女装','/女装/')
			 RETURNING id`, mid).Scan(&cat); err != nil {
			t.Fatal(err)
		}
		f.catID[mid] = cat
		for j := 0; j < perMerchant; j++ {
			var pid int64
			if err := admin.QueryRow(ctx,
				`INSERT INTO products (merchant_id, category_id, title, subtitle, status, published_at)
				 VALUES ($1,$2,$3,$4,1,now()) RETURNING id`,
				mid, cat, fmt.Sprintf("红色连衣裙%d", j), "夏季新款").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			f.products[mid] = append(f.products[mid], pid)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM product_text_vectors  WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_understanding WHERE merchant_id = ANY($1)`,
			`DELETE FROM products              WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories            WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants             WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, f.merchants); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	p, err := db.NewPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	f.pool = p
	f.repo = fixtureRepo{Repo: repository.New(p), merchants: f.merchants}
	svc, err := service.NewIndexService(f.repo, f.emb, cfg, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// --- 读库的小工具 ---

func (f *indexFixture) vectorRow(t *testing.T, pid int64) (content, model, version string, merchantID int64, norm float64, updatedAt time.Time, ok bool) {
	t.Helper()
	err := f.admin.QueryRow(context.Background(), `
		SELECT content, model_name, model_version, merchant_id,
		       sqrt((SELECT sum(x*x) FROM unnest(embedding::real[]) AS x)), updated_at
		  FROM product_text_vectors WHERE product_id = $1`, pid).
		Scan(&content, &model, &version, &merchantID, &norm, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", 0, 0, time.Time{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return content, model, version, merchantID, norm, updatedAt, true
}

func (f *indexFixture) searchText(t *testing.T, pid int64) *string {
	t.Helper()
	var s *string
	if err := f.admin.QueryRow(context.Background(),
		`SELECT search_text FROM products WHERE id = $1`, pid).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *indexFixture) hashes(t *testing.T, pid int64) map[string]*string {
	t.Helper()
	var te, st *string
	var status int16
	var pipeline string
	err := f.admin.QueryRow(context.Background(), `
		SELECT input_hashes ->> 'text_embedding', input_hashes ->> 'search_text',
		       status, pipeline_version
		  FROM product_understanding WHERE product_id = $1`, pid).
		Scan(&te, &st, &status, &pipeline)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*string{
		search.ProcessorTextEmbedding: te,
		search.ProcessorSearchText:    st,
		"_status":                     ptr(fmt.Sprint(status)),
		"_pipeline":                   &pipeline,
	}
}

func ptr[T any](v T) *T { return &v }

func (f *indexFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 一、两份派生数据真的进库了
// ---------------------------------------------------------------------------

// 向量真的写进去了，bigram 串真的写进去了，而且落在对的租户名下。
//
// 「不是空表也全绿」是这条测试要挡的第一件事，所以每一条断言都带着具体的值：
// content 必须等于 §2.1 模板拼出来的那一段，search_text 必须等于切分函数
// 的输出，merchant_id 必须是当前租户（那一列由 DEFAULT current_merchant() 填，
// 写入语句里根本没有它）。
func TestIndexWritesVectorsAndSearchText(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "write", 2, 1, service.IndexConfig{})

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judged != 2 || rep.Embedded != 2 || rep.SearchTextWritten != 2 {
		t.Fatalf("报告是 %+v，期望判定 2 / 重算 2 / 写串 2", rep)
	}

	want := search.ProductText{Title: "红色连衣裙0", Subtitle: "夏季新款", CategoryName: "女装"}
	for _, mid := range f.merchants {
		pid := f.products[mid][0]

		content, model, version, owner, norm, _, ok := f.vectorRow(t, pid)
		if !ok {
			t.Fatalf("商品 %d 没有向量行 —— 「写进去了」这件事是假的", pid)
		}
		if content != want.EmbedContent() {
			t.Errorf("content 是 %q，期望 %q", content, want.EmbedContent())
		}
		if model != inference.ModelName || version != "stub-v1" {
			t.Errorf("落库的模型是 %s@%s，期望 %s@stub-v1 —— "+
				"这两列是「这批向量要不要重算」的另一半依据", model, version, inference.ModelName)
		}
		if owner != mid {
			t.Errorf("向量行的 merchant_id 是 %d，期望 %d —— "+
				"写入语句里没有这一列，它必须由 DEFAULT current_merchant() 填成当前租户",
				owner, mid)
		}
		if math.Abs(norm-1) > 1e-3 {
			t.Errorf("库里那条向量的范数是 %.6f", norm)
		}

		st := f.searchText(t, pid)
		if st == nil || *st != want.SearchText() {
			t.Errorf("search_text 是 %v，期望 %q", st, want.SearchText())
		}

		h := f.hashes(t, pid)
		if h == nil {
			t.Fatalf("商品 %d 没有 product_understanding 行 —— 指纹没记，下一轮会重算一遍", pid)
		}
		if h[search.ProcessorTextEmbedding] == nil ||
			*h[search.ProcessorTextEmbedding] != want.EmbedFingerprint() {
			t.Errorf("text_embedding 指纹是 %v，期望 %s",
				h[search.ProcessorTextEmbedding], want.EmbedFingerprint())
		}
		if h[search.ProcessorSearchText] == nil ||
			*h[search.ProcessorSearchText] != want.SearchTextFingerprint() {
			t.Errorf("search_text 指纹是 %v，期望 %s",
				h[search.ProcessorSearchText], want.SearchTextFingerprint())
		}
		if *h["_status"] != "1" {
			t.Errorf("status 是 %s，期望 1（部分完成）—— 本轮只有两个 processor 落地，"+
				"写 2 会让 idx_pu_unfinished 那张「未完成」列表从第一天起就是空的", *h["_status"])
		}
		if *h["_pipeline"] != search.PipelineVersion {
			t.Errorf("pipeline_version 是 %q", *h["_pipeline"])
		}
	}

	// 关键词召回那一路真的通了：search_vector 是由 search_text 生成的，
	// 而 search_text 是切过的，所以用户搜「连衣」命中得到。
	// 这一条与 internal/db 的 TestSearchVectorIsGeneratedFromSearchText 互补：
	// 那条证明「生成列读的是 search_text」，这条证明「入库任务写进那一列的
	// 东西真的被切过」。把 search.Bigram 改成恒等函数，那条照绿，这条红。
	var hit bool
	if err := f.admin.QueryRow(ctx, `
		SELECT search_vector @@ to_tsquery('simple', '连衣')
		  FROM products WHERE id = $1`, f.products[f.merchants[0]][0]).Scan(&hit); err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Error("搜「连衣」命中不了刚索引的商品 —— 写进 search_text 的串没被切过")
	}
	// 阳性对照：未切分的整句召不回。没有它，上面那条在「这个库装了中文分词器」
	// 时也会绿，而那时 bigram 方案的前提（§3 开头）根本不成立。
	var whole bool
	if err := f.admin.QueryRow(ctx,
		`SELECT to_tsvector('simple','红色连衣裙0') @@ to_tsquery('simple','连衣')`).
		Scan(&whole); err != nil {
		t.Fatal(err)
	}
	if whole {
		t.Error("未切分的整句也能被「连衣」召回 —— 这个库有中文分词器，" +
			"bigram 方案的前提不成立，整个选型要重看")
	}
}

// 批量：一家店一次调用，不是一件商品一次（语义检索层 §10 第一条）。
func TestEmbedIsCalledOncePerTenantNotPerProduct(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "batch", 1, 5, service.IndexConfig{})

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.emb.calls) != 1 {
		t.Fatalf("打了 %d 次 /v1/embed，期望 1 次 —— 5 件商品退化成了循环单条。"+
			"一次 HTTP 往返的固定开销在循环单条时要乘 5 遍（§10：禁止循环单条调用）",
			len(f.emb.calls))
	}
	if n := len(f.emb.calls[0]); n != 5 {
		t.Fatalf("那一次调用只送了 %d 条文本，期望 5 条", n)
	}
}

// 第二轮什么都不做：水位线推上去之后，同一批商品不再是候选。
//
// 没有水位线的话它们每一轮都会被重新捞回来（触发点只比时间戳），
// 而且因为 ORDER BY updated_at 排在队首，把每租户配额占满 ——
// 队尾那件真的改了标题的商品一轮也轮不到。
func TestSecondRoundHasNothingToDo(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "idem", 1, 3, service.IndexConfig{})

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := len(f.emb.calls)

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judged != 0 {
		t.Fatalf("第二轮还捞回 %d 件商品 —— 水位线没推上去，这批商品会永远占着配额", rep.Judged)
	}
	if len(f.emb.calls) != callsAfterFirst {
		t.Fatalf("第二轮又打了引擎（%d → %d 次）", callsAfterFirst, len(f.emb.calls))
	}
}

// ---------------------------------------------------------------------------
// 二、判据：改标题要重算，改 sales_count 不许重算
// ---------------------------------------------------------------------------

func TestTitleChangeRecomputes(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "title", 1, 1, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before, _, _, _, _, _, _ := f.vectorRow(t, pid)

	f.exec(t, `UPDATE products SET title = '蓝色针织衫' WHERE id = $1`, pid)

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judged != 1 || rep.Embedded != 1 {
		t.Fatalf("报告是 %+v，期望判定 1 / 重算 1", rep)
	}
	after, _, _, _, _, _, _ := f.vectorRow(t, pid)
	if after == before {
		t.Fatalf("content 没变（还是 %q）—— 向量没跟着新标题走", before)
	}
	want := search.ProductText{Title: "蓝色针织衫", Subtitle: "夏季新款", CategoryName: "女装"}
	if after != want.EmbedContent() {
		t.Errorf("content 是 %q，期望 %q", after, want.EmbedContent())
	}
	if st := f.searchText(t, pid); st == nil || *st != want.SearchText() {
		t.Errorf("search_text 是 %v，期望 %q", st, want.SearchText())
	}
}

// **改 sales_count 不重算。这条是本任务判据的全部意义。**
//
// 它有两半，缺一半都不成立：
//
//	① 触发点确实被拨动了（Judged == 1）。products.updated_at 前进了，
//	   这件商品被重新**判定**过 —— internal/db 的 TestStalenessCriterionRawMaterial
//	   的 ② 用真实数据钉死了「改 sales_count 也会让 updated_at 前进」。
//	② 判定的结论是不用算（Embedded == 0，而且一次引擎都没打）。
//
// 没有 ①，「没有重算」这个结论对一个什么都没扫到的实现同样成立 ——
// 把整个任务注释掉，这条测试照绿。这正是本仓库反复在抓的
// 「断言存在但和被测代码没有因果关系」。
func TestSalesCountChangeDoesNotRecompute(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "sales", 1, 1, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, vecBefore, _ := f.vectorRow(t, pid)
	callsBefore := len(f.emb.calls)

	f.exec(t, `UPDATE products SET sales_count = sales_count + 1 WHERE id = $1`, pid)

	// ① 触发点真的被拨动了：products.updated_at 现在走在向量表前面。
	// 这一句是**阳性对照**，它证明下面那条「没重算」不是因为什么都没看见。
	var ahead bool
	if err := f.admin.QueryRow(ctx, `
		SELECT p.updated_at > v.updated_at
		  FROM products p JOIN product_text_vectors v ON v.product_id = p.id
		 WHERE p.id = $1`, pid).Scan(&ahead); err != nil {
		t.Fatal(err)
	}
	if !ahead {
		t.Fatal("改 sales_count 之后 products.updated_at 没有走在向量表前面 —— " +
			"触发点的前提变了，下面那条断言证明不了任何事（00016 文件头第四节要重写）")
	}

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judged != 1 {
		t.Fatalf("这件商品没有被判定（Judged=%d）—— 触发点漏了它。"+
			"下面那条「没重算」因此不成立：它可能只是没被看见", rep.Judged)
	}
	// ② 判定的结论：不用算。
	if rep.Embedded != 0 {
		t.Errorf("重算了 %d 条向量 —— 一次下单（sales_count / total_stock）就会把"+
			"全店商品重算一遍，而重算 embedding 的钱是真花出去的（00016 文件头第四节）",
			rep.Embedded)
	}
	if rep.SearchTextWritten != 0 {
		t.Errorf("重写了 %d 条 bigram 串 —— 标题一个字都没变", rep.SearchTextWritten)
	}
	if rep.Skipped != 1 {
		t.Errorf("Skipped=%d，期望 1", rep.Skipped)
	}
	if len(f.emb.calls) != callsBefore {
		t.Fatalf("打了引擎（%d → %d 次）—— 判定那一半没起作用",
			callsBefore, len(f.emb.calls))
	}
	if _, _, _, _, _, vecAfter, _ := f.vectorRow(t, pid); !vecAfter.Equal(vecBefore) {
		t.Errorf("向量行的 updated_at 动了（%v → %v）—— 它被重写了", vecBefore, vecAfter)
	}
}

// 两格指纹互相独立：换类目只重算向量，不重写 bigram 串。
//
// 00016 文件头第四节点名要的粒度。做错了不报错：两格绑在一起时，
// 一次类目调整会把全类目商品的 search_text 重写一遍 —— 结果完全正确，
// 只是白写，而且 products.updated_at 跟着前进，把下一轮的候选集撑满。
func TestCategoryChangeTouchesOnlyTheEmbeddingFingerprint(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "cat", 1, 1, service.IndexConfig{})
	mid := f.merchants[0]
	pid := f.products[mid][0]

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before := f.hashes(t, pid)
	stBefore := f.searchText(t, pid)

	// 换类目名（等价于把商品挪到另一个类目 —— 进 embedding 的是类目**名**）。
	//
	// 这一句**不碰任何 products 行**，所以它同时依赖触发点里 c.updated_at
	// 那一支（db/queries/semantic.sql）。这里刻意不补一句「等价的触碰」去
	// UPDATE products —— 那是现实不会提供的因果链，补上之后这条测试测的就
	// 不再是「改类目名之后会发生什么」，而是「假设有人替我拨了触发点之后
	// 会发生什么」。触发点漏算这件事因此会完全绕开它。
	// 直接针对触发点的那一条是 TestCategoryRenameRefreshesEveryProductInThatCategory。
	f.exec(t, `UPDATE categories SET name = '裙装' WHERE id = $1`, f.catID[mid])

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Embedded != 1 {
		t.Fatalf("换类目之后没有重算向量（Embedded=%d）—— 类目在 §2.1 的模板里，"+
			"不重算的话向量永远停在旧类目上", rep.Embedded)
	}
	if rep.SearchTextWritten != 0 {
		t.Errorf("换类目却重写了 %d 条 bigram 串 —— search_text 的输入只有标题与副标题，"+
			"两格指纹没有分开", rep.SearchTextWritten)
	}

	after := f.hashes(t, pid)
	if *after[search.ProcessorTextEmbedding] == *before[search.ProcessorTextEmbedding] {
		t.Error("text_embedding 那一格的指纹没变")
	}
	if *after[search.ProcessorSearchText] != *before[search.ProcessorSearchText] {
		t.Error("search_text 那一格的指纹变了 —— 换类目不该动它")
	}
	if stAfter := f.searchText(t, pid); *stAfter != *stBefore {
		t.Errorf("search_text 列被重写了：%q → %q", *stBefore, *stAfter)
	}
}

// 改一次类目名，该类目下**每一件**商品的向量都要跟上。
//
// 这条与上面那条的区别不是「又测一遍」：上面那条测的是**判定**的粒度
// （两格指纹分得开），这一条测的是**触发点**本身 —— 一次
// `UPDATE categories SET name = ...` 一行 products 都不碰，少了
// ListStaleProductsForIndex 里 c.updated_at 那一支，这批商品根本不会被捞回来，
// 判定那一半连跑的机会都没有。
//
// 这条测试的三处形状是刻意的：
//
//	① **断言看的是库里真实的 content，不是 IndexReport 的计数。**
//	   计数来自应用自己的记账，它对「写回去的到底是什么」一无所知 ——
//	   一个把旧文本又写了一遍的实现照样报 Embedded=3。
//	② **每一件都查**，不是抽一件。触发点漏算的形态是「整类目一起漏」，
//	   但也可能是「只捞回了 LIMIT 里靠前的那几件」，抽查看不出后者。
//	③ **改两次名**。验收实测到的症状正是「向量落后了两代」：第一次改名之后
//	   如果有什么东西碰巧拨动过触发点（比如同一轮里商品被别的写入摸过），
//	   只改一次可能侥幸绿。第二次改名之后再核一遍，那条侥幸就不存在了。
//
// 最后一段是**判定那一半的阳性对照**：类目名不再变之后，下一轮必须
// 一条向量都不重算。少了它，一个「每轮无条件重算全部商品」的实现
// 也能让上面每一条断言变绿，而那正好是判据存在的全部理由被删掉的样子。
func TestCategoryRenameRefreshesEveryProductInThatCategory(t *testing.T) {
	ctx := context.Background()
	const n = 3
	f := newIndexFixture(t, "catrename", 1, n, service.IndexConfig{})
	mid := f.merchants[0]
	pids := f.products[mid]

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 起点：三件商品的 content 里都是建夹具时那个类目名。
	for _, pid := range pids {
		assertVectorContent(t, f, pid, "女装")
	}

	for _, name := range []string{"裙装", "连衣裙专区"} {
		f.exec(t, `UPDATE categories SET name = $1 WHERE id = $2`, name, f.catID[mid])

		rep, err := f.svc.IndexOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Judged < n {
			t.Fatalf("改类目名为 %q 之后只判定了 %d 件（共 %d 件）—— "+
				"触发点漏了这一类目下的商品。UPDATE categories SET name = ... "+
				"一行 products 都不碰，ListStaleProductsForIndex 里必须有一支看 "+
				"categories.updated_at，否则这批向量的 content 永远停在旧类目名上，"+
				"而且不会有任何东西报错。报告：%+v", name, rep.Judged, n, rep)
		}
		// 库里真实的 content —— 不看 rep.Embedded。
		for _, pid := range pids {
			assertVectorContent(t, f, pid, name)
		}
	}

	// 判定那一半的阳性对照，也是这一支新增的**代价**的落点。
	//
	// 动一下类目里与文本无关的列：触发点（c.updated_at）照样前进，该类目下
	// 三件商品**都会被重新判定**（Judged == n，这一半证明下面那句不是空的），
	// 而判定的结论必须是「指纹没变，一条都不用算」（Embedded == 0）。
	//
	// 少了这一段，一个「每一轮无条件重算全部商品」的实现能让上面每一条断言
	// 都变绿 —— 那正好是判据存在的全部理由被删掉的样子。
	f.exec(t, `UPDATE categories SET sort_order = sort_order + 1 WHERE id = $1`, f.catID[mid])
	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Judged != n {
		t.Fatalf("动了类目的 sort_order 之后只判定了 %d 件（共 %d 件）—— "+
			"下面那条「没重算」因此证明不了任何事：它们可能只是没被看见。报告：%+v",
			rep.Judged, n, rep)
	}
	if rep.Embedded != 0 {
		t.Errorf("类目名一个字都没变，这一轮却重算了 %d 条向量 —— "+
			"触发点那一支把判定也一起绕过去了。改一次类目排序就把全类目商品"+
			"重算一遍，钱是真花出去的，而检索结果看上去完全正常。报告：%+v",
			rep.Embedded, rep)
	}
	if rep.SearchTextWritten != 0 {
		t.Errorf("动类目的 sort_order 却重写了 %d 条 bigram 串 —— "+
			"search_text 的输入只有标题与副标题", rep.SearchTextWritten)
	}
}

// assertVectorContent 核对库里那条向量的 content 就是按当前类目名拼出来的那一段。
//
// 它比「content 里含有新类目名」严格：拼接模板（search.ProductText.EmbedContent）
// 是送进模型的那段文本本身，只要求「含有」的话，一个把新旧类目名拼在一起的
// 实现也算过。
func assertVectorContent(t *testing.T, f *indexFixture, pid int64, categoryName string) {
	t.Helper()
	var title, subtitle string
	if err := f.admin.QueryRow(context.Background(),
		`SELECT title, coalesce(subtitle,'') FROM products WHERE id = $1`, pid).
		Scan(&title, &subtitle); err != nil {
		t.Fatal(err)
	}
	want := search.ProductText{Title: title, Subtitle: subtitle, CategoryName: categoryName}.
		EmbedContent()
	got, _, _, _, _, _, ok := f.vectorRow(t, pid)
	if !ok {
		t.Fatalf("商品 %d 根本没有向量行", pid)
	}
	if got != want {
		t.Errorf("商品 %d 的向量 content 是 %q，期望 %q —— "+
			"库里这条向量落后于当前的商品文本", pid, got, want)
	}
}

// ---------------------------------------------------------------------------
// 三、引擎挂了
// ---------------------------------------------------------------------------

// 引擎不可用时**一个向量都不写**，尤其不写零向量。
//
// 零向量对任何查询向量的余弦距离都是 1，它会出现在**每一次**检索的结果里，
// 而没有任何东西会报错 —— 商品照常索引、向量照常入库、搜索照常返回结果。
func TestEngineFailureWritesNoVectorAtAll(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "down", 1, 2, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	f.emb.err = fmt.Errorf("%w: 连不上", inference.ErrUnavailable)
	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.EngineDown == 0 {
		t.Fatalf("报告里没有引擎故障：%+v —— 错误被吞掉了", rep)
	}
	if rep.Embedded != 0 {
		t.Fatalf("引擎挂着却写了 %d 条向量", rep.Embedded)
	}
	var n int
	if err := f.admin.QueryRow(ctx,
		`SELECT count(*) FROM product_text_vectors WHERE merchant_id = ANY($1)`,
		f.merchants).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("库里有 %d 行向量 —— 引擎挂着的时候它们只能是零向量或者垃圾", n)
	}
	// 指纹也不许记：记了的话水位线推上去，这批商品从此不再是候选，
	// 引擎恢复之后它们永远拿不到向量。
	if h := f.hashes(t, pid); h != nil {
		t.Fatalf("引擎挂着却记了指纹 %v —— 水位线推上去了，"+
			"这件商品从此不再是候选，引擎恢复之后也不会被重算", h)
	}

	// 阳性对照：引擎好了之后同一批商品照常入库。
	// 没有它，上面几条在「这批商品根本没被扫到」时同样全绿。
	f.emb.err = nil
	rep2, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Embedded != 2 {
		t.Fatalf("引擎恢复之后只重算了 %d 条，期望 2 —— "+
			"上面那几条断言证明不了「因为引擎挂了才没写」", rep2.Embedded)
	}
	if _, _, _, _, _, _, ok := f.vectorRow(t, pid); !ok {
		t.Fatal("引擎恢复之后还是没有向量行")
	}
}

// 引擎挂着时，那些**不需要引擎**的商品照常处理。
//
// 不这么做的话，一次引擎故障会连带把关键词召回的维护也停掉；更糟的是
// 那些商品的水位线推不上去，于是它们一直占着配额，引擎恢复之后队列还堵着。
func TestEngineFailureDoesNotBlockSearchTextOnlyWork(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "partial", 1, 1, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	// 先正常索引一遍，让这件商品有向量、有指纹。
	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 再把 search_text 抹掉（模拟「只有 bigram 串这一半要补」）。
	f.exec(t, `UPDATE products SET search_text = NULL WHERE id = $1`, pid)

	f.emb.err = fmt.Errorf("%w: 连不上", inference.ErrUnavailable)
	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SearchTextWritten != 1 {
		t.Fatalf("引擎挂着时连不依赖引擎的 bigram 串也没写（%+v）—— "+
			"一次引擎故障把关键词召回的维护一起停掉了", rep)
	}
	if rep.EngineDown != 0 {
		t.Errorf("这一轮根本不需要打引擎（向量是新的），却记了 %d 次引擎故障",
			rep.EngineDown)
	}
	if st := f.searchText(t, pid); st == nil {
		t.Fatal("search_text 还是 NULL")
	}
}

// ---------------------------------------------------------------------------
// 四、判定与写回之间商品又变了
// ---------------------------------------------------------------------------

// 这段窗口是真的：判定之后要打一次 HTTP 去算向量，秒级。
// 期间商家又改了一次标题的话，写回去的是上一个版本算出的向量，
// 而水位线会被推到当前 —— 这件商品从此不再是候选，库里那条向量
// 永远停在旧标题上，且不报错。
//
// 替身在返回之前改一次标题，把这条竞态变成确定性的。
func TestProductChangedDuringEmbedIsLeftForNextRound(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "race", 1, 1, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	f.emb.beforeReturn = func() {
		if _, err := f.admin.Exec(context.Background(),
			`UPDATE products SET title = '半路改掉的标题' WHERE id = $1`, pid); err != nil {
			t.Error(err)
		}
	}
	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Raced != 1 {
		t.Fatalf("报告是 %+v，期望 Raced=1 —— 判定与写回之间商品变了，整条该跳过", rep)
	}
	if _, _, _, _, _, _, ok := f.vectorRow(t, pid); ok {
		t.Fatal("按旧标题算出来的向量还是写进去了")
	}
	if h := f.hashes(t, pid); h != nil {
		t.Fatal("水位线被推上去了 —— 这件商品从此不再是候选，" +
			"而它的向量停在一个从没被写进去的版本上")
	}

	// 阳性对照：下一轮（没有竞态了）照常入库，而且按**新**标题。
	f.emb.beforeReturn = nil
	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	content, _, _, _, _, _, ok := f.vectorRow(t, pid)
	if !ok {
		t.Fatal("下一轮也没写进去 —— 上面那条红的原因不是竞态")
	}
	want := search.ProductText{Title: "半路改掉的标题", Subtitle: "夏季新款", CategoryName: "女装"}
	if content != want.EmbedContent() {
		t.Errorf("content 是 %q，期望按新标题算的 %q", content, want.EmbedContent())
	}
}

// ---------------------------------------------------------------------------
// 五、00016 文件头第四节那条可机械检查的闸门
// ---------------------------------------------------------------------------

// 原话：
//
//	对每一个 products 行，若 products.updated_at > product_text_vectors.updated_at，
//	则 input_hashes ->> 'text_embedding' 必须等于「按当前 products 行算出的文本指纹」。
//	等于 ⇒ 这次变更不涉及文本，向量没过期；不等 ⇒ 向量过期了而它还没被重算，闸门红。
//
// 这条测试先制造出那个前提（跑完一轮索引，再改一次 sales_count 并再跑一轮），
// 然后把闸门跑一遍。**它自带阳性对照**：如果一行都不满足
// `products.updated_at > 向量表.updated_at`，闸门是空转的，那时要红在这里。
func TestStalenessGateHoldsAfterIndexing(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "gate", 2, 2, service.IndexConfig{})

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// 制造出「时间戳走在前面、但文本没变」的那批行。
	f.exec(t, `UPDATE products SET sales_count = sales_count + 1 WHERE merchant_id = ANY($1)`,
		f.merchants)
	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := f.admin.Query(ctx, `
		SELECT p.id, p.title, coalesce(p.subtitle,''), c.name,
		       pu.input_hashes ->> 'text_embedding'
		  FROM products p
		  JOIN categories c ON c.id = p.category_id
		  JOIN product_text_vectors v ON v.product_id = p.id
		  LEFT JOIN product_understanding pu ON pu.product_id = p.id
		 WHERE p.merchant_id = ANY($1)
		   AND p.updated_at > v.updated_at`, f.merchants)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var id int64
		var title, subtitle, category string
		var stored *string
		if err := rows.Scan(&id, &title, &subtitle, &category, &stored); err != nil {
			t.Fatal(err)
		}
		checked++
		want := search.ProductText{Title: title, Subtitle: subtitle, CategoryName: category}.
			EmbedFingerprint()
		if stored == nil {
			t.Errorf("商品 %d：products.updated_at 走在向量表前面，而 text_embedding "+
				"那一格是 NULL（从没算过）—— 向量过期了且没被重算", id)
			continue
		}
		if *stored != want {
			t.Errorf("商品 %d：指纹是 %s，按当前文本算出来是 %s —— "+
				"向量过期了而它还没被重算（00016 文件头第四节的闸门）", id, *stored, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// 阳性对照：闸门必须真的检查到了东西。一行都没有的话它恒绿 ——
	// 包括在「整个索引任务被删掉」的那一天。
	if checked == 0 {
		t.Fatal("一行都不满足 products.updated_at > 向量表.updated_at —— " +
			"闸门在空转。要么夹具没造出那个前提，要么有人给向量表挂了一个" +
			"跟随 products 的级联触发器（那会让这个先后关系变成恒等式，" +
			"判据从此永远说「没过期」，00016 文件头点名不许）")
	}
	t.Logf("闸门检查了 %d 行「时间戳走在前面」的商品，指纹全部与当前文本一致", checked)
}

// ---------------------------------------------------------------------------
// 六、公平：大商家的积压不能把小商家饿死
// ---------------------------------------------------------------------------

// 每租户上限 + 每轮总预算 + 轮转起点，形状照 sweep.go。
//
// 造的场景：两家店各有积压，而一轮的总预算只够一家。固定起点的话，
// 预算永远从 id 最小的那家开始花，另一家一轮也轮不到。
func TestFairSchedulingRotatesAcrossTenants(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "fair", 2, 3, service.IndexConfig{
		PerTenantCap: 1,
		RoundBudget:  1,
	})

	served := map[int64]int{}
	countVectors := func(mid int64) int {
		var n int
		if err := f.admin.QueryRow(ctx,
			`SELECT count(*) FROM product_text_vectors WHERE merchant_id = $1`, mid).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for round := 0; round < 2; round++ {
		rep, err := f.svc.IndexOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Judged != 1 {
			t.Fatalf("第 %d 轮判定了 %d 件，期望 1 件（总预算就是 1）", round, rep.Judged)
		}
	}
	for _, mid := range f.merchants {
		served[mid] = countVectors(mid)
	}
	for _, mid := range f.merchants {
		if served[mid] == 0 {
			t.Fatalf("两轮之后商户 %d 一件也没轮到（各家进度 %v）—— "+
				"轮转起点没生效，预算永远从同一家开始花，尾部的店在预算紧张时"+
				"一次也轮不到，那正是公平调度要防的饿死", mid, served)
		}
	}
	t.Logf("两轮之后各家的进度：%v", served)
}

// ---------------------------------------------------------------------------
// 七、全量与 -force
// ---------------------------------------------------------------------------

// Backfill 翻页把全部商品过一遍；-force 跳过判定无条件重算（换模型的杠杆）。
func TestBackfillPagesThroughEverythingAndForceRecomputes(t *testing.T) {
	ctx := context.Background()
	// PerTenantCap = 2，商品 5 件 —— 逼它真的翻三页。
	f := newIndexFixture(t, "full", 1, 5, service.IndexConfig{PerTenantCap: 2})
	mid := f.merchants[0]

	rep, err := f.svc.Backfill(ctx, mid, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Embedded != 5 {
		t.Fatalf("全量只重算了 %d 条，期望 5 条（%+v）—— 翻页漏了", rep.Embedded, rep)
	}
	if len(f.emb.calls) != 3 {
		t.Fatalf("打了 %d 次引擎，期望 3 次（5 件 ÷ 每页 2 件）", len(f.emb.calls))
	}

	// 再跑一次非 force 的全量：判定生效，一条也不该重算。
	rep2, err := f.svc.Backfill(ctx, mid, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Judged != 5 {
		t.Fatalf("第二次全量只判定了 %d 件，期望 5 件 —— 全量那条查询不看触发点，"+
			"它应该每次都把全部在架商品过一遍", rep2.Judged)
	}
	if rep2.Embedded != 0 {
		t.Fatalf("第二次全量又重算了 %d 条 —— 全量也要走判定，"+
			"否则每跑一次全量就是一次全额的 embedding 账单", rep2.Embedded)
	}

	// -force：无条件重算。换模型、改拼接模板之后的那条路。
	rep3, err := f.svc.Backfill(ctx, mid, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Embedded != 5 {
		t.Fatalf("-force 只重算了 %d 条，期望 5 条 —— 换模型之后全量重建靠它",
			rep3.Embedded)
	}
}

// 换模型 ⇒ 重算。model_name 是「要不要重算」的另一半依据（语义检索层 §2.2）。
//
// 造法：把库里那一行的 model_name 改成别的模型（相当于这条向量是上一个模型
// 算的），文本一个字不改，再拨一下触发点。判定必须看出来。
func TestModelChangeForcesRecompute(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "model", 1, 1, service.IndexConfig{})
	pid := f.products[f.merchants[0]][0]

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE product_text_vectors SET model_name = 'some-older-model'
	            WHERE product_id = $1`, pid)
	// 触发点要被拨动它才会被重新判定 —— 换模型这件事本身不会碰 products。
	// 真实世界里对应的动作是 cmd/keel-index -force，那条路不依赖触发点；
	// 这里只验判定那一半认不认得 model_name。
	f.exec(t, `UPDATE products SET sales_count = sales_count + 1 WHERE id = $1`, pid)

	rep, err := f.svc.IndexOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Embedded != 1 {
		t.Fatalf("库里那条向量来自另一个模型，却没有被重算（%+v）—— "+
			"两个模型的向量不在同一个空间里，余弦距离算得出来、毫无意义，而且不报错", rep)
	}
	_, model, _, _, _, _, _ := f.vectorRow(t, pid)
	if model != inference.ModelName {
		t.Errorf("重算之后 model_name 还是 %q", model)
	}
}

// input_hashes 是四个 processor 共用的一列，写它必须**合并**而不是覆盖。
//
// 整体覆盖会把别人那几格抹掉，而抹掉的后果是「从没算过」—— 下一轮图像向量
// 重算一遍。那笔钱同样是真花出去的，而且没有任何东西会报错：
// 图像向量重算出来的值和原来一模一样。
func TestIndexKeepsOtherProcessorsFingerprints(t *testing.T) {
	ctx := context.Background()
	f := newIndexFixture(t, "merge", 1, 1, service.IndexConfig{})
	mid := f.merchants[0]
	pid := f.products[mid][0]

	// 假装图像那一路先跑过了。
	f.exec(t, `INSERT INTO product_understanding
	             (product_id, merchant_id, pipeline_version, input_hashes)
	           VALUES ($1, $2, 'image-v1', '{"image_embedding":"IMGHASH"}'::jsonb)`, pid, mid)

	if _, err := f.svc.IndexOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var img, te *string
	if err := f.admin.QueryRow(ctx, `
		SELECT input_hashes ->> 'image_embedding', input_hashes ->> 'text_embedding'
		  FROM product_understanding WHERE product_id = $1`, pid).Scan(&img, &te); err != nil {
		t.Fatal(err)
	}
	if img == nil || *img != "IMGHASH" {
		t.Errorf("image_embedding 那一格被抹掉了（%v）—— 写 input_hashes 用的是整体覆盖，"+
			"下一轮图像向量会白重算一遍，而且不报错", img)
	}
	// 阳性对照：本轮该写的那一格真的写进去了。没有它，一个「什么都没写」的
	// 实现也能让上面那条绿。
	if te == nil {
		t.Error("text_embedding 那一格没写进去 —— 上面那条断言证明不了合并在工作")
	}
}
