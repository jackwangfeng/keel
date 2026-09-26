package handler_test

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/tenant"
)

// 检索测试的夹具：一个**语义可控**的引擎替身，加两家店的真实商品数据。
//
// ===========================================================================
// 为什么替身必须「语义可控」，而不是「确定性就够」
// ===========================================================================
//
// internal/inference/fake 那个替身产出的是确定性的**伪随机**单位向量：维度对、
// 范数是 1、同样输入同样输出 —— 它能通过本仓库为向量设的每一道形状闸门，
// 唯独没有语义（那个包的 doc.go 里写着这件事，还有一条测试专门证明它没有）。
//
// 拿它来测检索，能测的只有「两路都返回了行」。测不出来的恰恰是混合检索
// 存在的理由：
//
//	· 向量那一路捞回来的东西**和查询词有关系吗**；
//	· 向量那一路捞得到、关键词那一路捞不到的那批（「连衣裙」→「长裙」，
//	  两者只共享一个「裙」字，二元组完全不相交）真的存在吗；
//	· recall_source 里的 vector / keyword / both 三个值真的分得开吗。
//
// 伪随机向量下，这三件事的断言全都只能写成「返回了非空列表」，
// 而那种断言在实现彻底坏掉时照样绿。
//
// 所以这里的替身按**概念轴**给向量：文本里出现「裙」就落在裙那根轴上，
// 出现「咖啡 / 杯 / 壶」就落在咖啡那根轴上，各自再加一点由文本哈希决定的
// 微小扰动（让同一概念下的不同商品不至于完全重合、排序有确定的先后）。
// 它仍然是确定性的、归一化的、1024 维的 —— 该过的闸门一道不少。
//
// 它**不是**在假装自己是 BGE-M3。真模型的语义由
// `make test-engine`（keel_real_engine 标签，对真的跑着的引擎）与
// 端到端那次 docker compose 验；这里验的是**检索这条链路**在一个语义可预测的
// 向量空间里行为正确。两者缺一不可：只有前者，链路的分支覆盖不到；
// 只有后者，「语义」这两个字是自己编的。
type conceptEmbedder struct{}

var _ inference.Embedder = conceptEmbedder{}

// conceptAxes 把一个概念词映射到一根基向量的维度下标。
//
// 下标取得很分散（0 / 200 / 400），是为了让不同概念的基向量严格正交 ——
// 正交意味着「裙」和「咖啡」的余弦相似度是 0 加扰动，
// 而同概念的两件商品是 0.99 上下。断言因此有一条宽得离谱的安全边界，
// 不会因为扰动系数微调就翻。
var conceptAxes = []struct {
	dim   int
	words []string
}{
	{dim: 0, words: []string{"裙", "女装", "雪纺", "真丝"}},
	{dim: 200, words: []string{"咖啡", "杯", "壶", "烘焙"}},
	{dim: 400, words: []string{"轮胎", "汽车"}},
}

// conceptNoise 是扰动的幅度。
//
// 0.05 远小于 1（概念轴的权重），所以「同概念」永远比「跨概念」近；
// 又远大于 0，所以同概念的不同商品有确定且不同的距离 —— 没有它，
// 同概念商品的向量完全相同，top-k 的顺序就由数据库的物理顺序决定，
// 而那会让「向量路的排序」这件事变得测不出来。
const conceptNoise = 0.05

func (conceptEmbedder) Embed(ctx context.Context, texts []string) (*inference.Result, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w：一条文本都没有", inference.ErrRejected)
	}
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		out = append(out, conceptVector(t))
	}
	return &inference.Result{
		Vectors:      out,
		Model:        inference.ModelName,
		ModelVersion: "concept-fixture",
	}, nil
}

// conceptVector 造一条 inference.Dim 维的单位向量。
func conceptVector(text string) []float32 {
	v := make([]float64, inference.Dim)
	for _, c := range conceptAxes {
		for _, w := range c.words {
			if strings.Contains(text, w) {
				v[c.dim] += 1
				break
			}
		}
	}
	// 扰动：由文本哈希决定，落在概念轴够不着的高位上。
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	seed := h.Sum64()
	for i := 0; i < 16; i++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		idx := 600 + int(seed>>33)%(inference.Dim-600)
		v[idx] += conceptNoise * (float64(int64(seed)%2001)/1000 - 1)
	}
	// 一个概念词都没命中时 v 可能全是扰动，甚至（极小概率）全零。
	// 全零向量归一化会得到 NaN，而 NaN 能写进 vector 列并让这一行
	// 从每一次检索里静默消失 —— 入库那一层会拒绝它（semantic.go），
	// 但那时错误指向的是「夹具坏了」而不是「夹具没考虑这种输入」。
	// 所以这里兜一笔：给最后一维一个固定的底，保证范数非零。
	v[inference.Dim-1] += 0.01

	var sum float64
	for _, x := range v {
		sum += x * x
	}
	norm := math.Sqrt(sum)
	out := make([]float32, inference.Dim)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}

// searchProduct 是夹具里的一件商品。
type searchProduct struct {
	Title    string
	Subtitle string
	Cents    int64
	Stock    int32

	// Draft / Deleted 是**反例**：这两件商品要带着完整的派生数据
	// （search_text + 向量）躺在库里，然后才被改成草稿 / 软删除。
	//
	// 顺序是刻意的。先写派生数据再翻状态，它们才真的进得了两路召回的
	// 候选面 —— 一件从来没有 search_text、也没有向量行的商品，是被
	// 「没有数据」挡在外面的，不是被 status / deleted_at 挡在外面的，
	// 而那时「它没出现在结果里」这条断言就是空转的：把两条召回查询的
	// `AND p.status = 1` 整个删掉，它照样绿。
	Draft   bool
	Deleted bool
}

// searchFixture 是一次检索测试要用的两家店。
type searchFixture struct {
	HostA, HostB    string
	MerchantA       int64
	MerchantB       int64
	IDsA            map[string]int64 // title -> product_id（A 店）
	IDsB            map[string]int64
	CategoryDressA  int64
	CategoryCoffeeA int64
}

// 夹具商品。三件在 A 店，其中两件是裙子、一件是咖啡器具；
// **B 店有一件与 A 店标题完全相同的裙子** —— 跨租户那条断言要靠它：
// 标题不同的话，「A 搜不到 B 的东西」既可能是 RLS 挡住了，
// 也可能只是 B 那件本来就不匹配这个查询词。
var (
	fxDress   = searchProduct{Title: "雪纺碎花连衣裙", Subtitle: "夏季新款 显瘦", Cents: 19900, Stock: 7}
	fxSkirt   = searchProduct{Title: "真丝吊带长裙", Subtitle: "法式复古", Cents: 45900, Stock: 4}
	fxCoffee  = searchProduct{Title: "手冲咖啡壶", Subtitle: "600ml 玻璃", Cents: 12900, Stock: 9}
	fxSoldOut = searchProduct{Title: "亚麻直筒连衣裙", Subtitle: "断货款", Cents: 22900, Stock: 0}

	// 两件**反例**，A 店，女装类目，有货，价格与 fxDress 同档 ——
	// 也就是说除了 status / deleted_at 之外，没有任何一个筛选条件会挡住它们。
	//
	// db/queries/search.sql 的文件头写着「两条查询的过滤条件必须逐字一致……
	// 且没有任何东西会红」。那句自陈在此之前是准确的：夹具里五件商品
	// 全是 status = 1、deleted_at 为 NULL，**一件反例都没有**，于是把
	// SearchProductsByKeyword 的 `AND p.status = 1` 改成 `>= 0`（源与 sqlc
	// 产物一起改，漂移闸门也不红）之后四个包全绿。
	//
	// 对照 db/queries/products.sql 那条同名纪律 —— 那边有
	// TestDraftAndDeletedProductsAreInvisible 当执行者，注释里还写着
	// 「这条测试是那句话唯一的执行者」。检索这边此前没有执行者。
	fxDraft   = searchProduct{Title: "草稿款连衣裙", Subtitle: "还没上架", Cents: 19900, Stock: 5, Draft: true}
	fxDeleted = searchProduct{Title: "已删除连衣裙", Subtitle: "软删待清理", Cents: 19900, Stock: 5, Deleted: true}
)

// newSearchFixture 播两家真实可解析的店，带商品、SKU、库存，
// 以及**由生产代码算出来的**两份派生数据（bigram 串与文本向量）。
//
// 派生数据走的是 repository 的真实写入路径（SetProductSearchText /
// UpsertProductTextVector），不是直接往表里塞：
//
//	· search_text 由 search.ProductText.SearchText() 算 —— 与索引任务
//	  （service/index.go 的 decide）调的是同一个函数。查询侧另写一份切分的话，
//	  「搜得到」这件事就会在这些测试上直接红，而那正是 internal/search
//	  包注释里那条「两侧必须切得一模一样」要守的东西。
//	· 向量过 UpsertProductTextVector 的维度与范数两道闸门。夹具自己绕过闸门的话，
//	  一个坏掉的替身会先毁掉测试的可信度，而不是当场报错。
func newSearchFixture(t *testing.T) searchFixture {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	tag := fmt.Sprintf("srch%d", time.Now().UnixNano()%1_000_000_000)
	codeA, codeB := tag+"a", tag+"b"

	// status = 1 且有域名：这两家要**真的能被 Host 解析出来**，
	// 因为检索是经 HTTP 打进去的，租户只能从 Host 来。
	// 别处的夹具用 status = 2 是因为它们只在 repository 层被直接使用。
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'检索店 A',1), ($2,'检索店 B',1)
		 RETURNING id`, codeA, codeB).Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, codeB).Scan(&idB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{idA, idB}
		for _, stmt := range []string{
			`DELETE FROM product_text_vectors  WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_understanding WHERE merchant_id = ANY($1)`,
			`DELETE FROM inventories WHERE sku_id IN (SELECT id FROM skus WHERE merchant_id = ANY($1))`,
			`DELETE FROM skus       WHERE merchant_id = ANY($1)`,
			`DELETE FROM products   WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, ids); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	fx := searchFixture{
		HostA:     codeA + "." + baseDomain,
		HostB:     codeB + "." + baseDomain,
		MerchantA: idA, MerchantB: idB,
		IDsA: map[string]int64{}, IDsB: map[string]int64{},
	}

	type plan struct {
		merchant int64
		category string
		p        searchProduct
		ids      map[string]int64
	}
	plans := []plan{
		{idA, "女装", fxDress, fx.IDsA},
		{idA, "女装", fxSkirt, fx.IDsA},
		{idA, "女装", fxSoldOut, fx.IDsA},
		{idA, "女装", fxDraft, fx.IDsA},
		{idA, "女装", fxDeleted, fx.IDsA},
		{idA, "咖啡器具", fxCoffee, fx.IDsA},
		{idB, "女装", fxDress, fx.IDsB},
	}

	cats := map[string]int64{}
	catKey := func(m int64, name string) string { return fmt.Sprintf("%d/%s", m, name) }
	for _, pl := range plans {
		k := catKey(pl.merchant, pl.category)
		if _, ok := cats[k]; ok {
			continue
		}
		var cid int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path, status)
			 VALUES ($1,$2,'/'||$2||'/',1) RETURNING id`, pl.merchant, pl.category).Scan(&cid); err != nil {
			t.Fatal(err)
		}
		cats[k] = cid
	}
	fx.CategoryDressA = cats[catKey(idA, "女装")]
	fx.CategoryCoffeeA = cats[catKey(idA, "咖啡器具")]

	for _, pl := range plans {
		cid := cats[catKey(pl.merchant, pl.category)]
		var pid int64
		if err := admin.QueryRow(ctx,
			// 商品行上**没有价格**（00019 删掉了那两列冗余价格）。
			// 价格区间由下面那条 SKU 现算出来，所以这个夹具从此是
			// 「价格过滤读的是不是真的 SKU 价」这件事的靶子 ——
			// 此前商品行与 SKU 行各写一份同样的数，把 LATERAL 写岔了也看不出来。
			`INSERT INTO products (merchant_id, category_id, title, subtitle,
			                       total_stock, sales_count, status, published_at)
			 VALUES ($1,$2,$3,$4,0,0,1,now()) RETURNING id`,
			pl.merchant, cid, pl.p.Title, pl.p.Subtitle).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		pl.ids[pl.p.Title] = pid

		var sid int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents, status)
			 VALUES ($1,$2,$3,$4,1) RETURNING id`,
			pl.merchant, pid, fmt.Sprintf("SRCH-%d", pid), pl.p.Cents).Scan(&sid); err != nil {
			t.Fatal(err)
		}
		// 库存行总是插，数量可以是 0 —— 「有 SKU、水位 0」与「没有库存行」
		// 对 in_stock_only 是同一个结果，但前者才是断货商品真实的样子。
		if _, err := admin.Exec(ctx,
			`INSERT INTO inventories (sku_id, available_qty, warning_qty) VALUES ($1,$2,1)`,
			sid, pl.p.Stock); err != nil {
			t.Fatal(err)
		}
	}

	// 两份派生数据，走真实写入路径。
	emb := conceptEmbedder{}
	repo := repository.New(testPool)
	for _, pl := range plans {
		pid := pl.ids[pl.p.Title]
		text := search.ProductText{
			Title: pl.p.Title, Subtitle: pl.p.Subtitle, CategoryName: pl.category,
		}
		res, err := emb.Embed(ctx, []string{text.EmbedContent()})
		if err != nil {
			t.Fatal(err)
		}
		tctx := tenant.NewContext(ctx, pl.merchant)
		if err := repo.WithTenant(tctx, func(tx repository.Tx) error {
			if err := tx.SetProductSearchText(ctx, pid, text.SearchText()); err != nil {
				return err
			}
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID:    pid,
				Content:      text.EmbedContent(),
				Embedding:    res.Vectors[0],
				ModelName:    res.Model,
				ModelVersion: res.ModelVersion,
			})
		}); err != nil {
			t.Fatalf("写派生数据失败（product %d）: %v", pid, err)
		}
	}

	// 两件反例**先拿到派生数据，再被改成草稿 / 软删除**。理由写在
	// searchProduct.Draft 上：顺序反过来的话它们根本进不了召回的候选面，
	// 「它们没出现在结果里」就与 status / deleted_at 那两个过滤条件无关了。
	for _, pl := range plans {
		switch {
		case pl.p.Draft:
			if _, err := admin.Exec(ctx,
				`UPDATE products SET status = 0, published_at = NULL WHERE id = $1`,
				pl.ids[pl.p.Title]); err != nil {
				t.Fatal(err)
			}
		case pl.p.Deleted:
			if _, err := admin.Exec(ctx,
				`UPDATE products SET deleted_at = now() WHERE id = $1`,
				pl.ids[pl.p.Title]); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 自证：两件反例真的**带着两份派生数据**躺在库里。
	//
	// 少了这一段，夹具哪天把写派生数据的循环挪到翻状态之后（或者 RLS 让那两次
	// 写入静默失败），反例就退化成「没有数据所以搜不到」，而依赖它们的断言
	// 会在完全没有守护力的情况下继续全绿 —— 这正是这一整条要修的那个毛病。
	for _, p := range []searchProduct{fxDraft, fxDeleted} {
		pid := fx.IDsA[p.Title]
		var hasText, hasVector bool
		if err := admin.QueryRow(ctx, `
			SELECT p.search_text IS NOT NULL,
			       EXISTS (SELECT 1 FROM product_text_vectors v WHERE v.product_id = p.id)
			  FROM products p WHERE p.id = $1`, pid).Scan(&hasText, &hasVector); err != nil {
			t.Fatal(err)
		}
		if !hasText || !hasVector {
			t.Fatalf("反例 %q（product %d）没有完整的派生数据"+
				"（search_text=%v 向量=%v）—— 它被挡在结果外面是因为没有数据，"+
				"不是因为 status / deleted_at，依赖它的断言因此是空转的",
				p.Title, pid, hasText, hasVector)
		}
	}
	return fx
}
