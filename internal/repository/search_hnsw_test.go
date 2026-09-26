package repository_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	gendb "github.com/keel/keel/internal/repository/internal/db"
	"github.com/keel/keel/internal/tenant"
)

// M3 计划第一条要求的那个实验，做成一条会跑在 `make test-db` 里的回归测试。
//
// ===========================================================================
// 它在证明什么
// ===========================================================================
//
// pgvector 的 HNSW 是近似索引：不开迭代扫描时，一次索引扫描一共只吐
// hnsw.ef_search 条候选（默认 40），吐完就结束。而这个仓库里过滤条件是
// **RLS 注入的** `merchant_id = current_merchant()` —— 应用看不见也改不掉。
// 于是一家只占全库百分之一点几的商家，那 40 条候选里属于它的期望值不到一条。
//
// 这条测试造一个真实规模的库（40 家商家、三万多条 1024 维向量），
// 然后用**同一条生成出来的 SQL**、同一个数据库角色、同一批数据跑两次：
//
//	坏的那一半：裸连接 + `SET LOCAL app.merchant_id`，**不设** hnsw.* ——
//	            也就是 repository.withTenantTx 里那三行被删掉之后的样子。
//	好的那一半：走 repository.WithTenant。
//
// 两次之间唯一的差别就是那三行 GUC。没有前半段，后半段证明不了任何事：
// 一条「设了迭代扫描之后能搜到东西」的断言，在陷阱根本没触发的小数据集上
// 永远是绿的。
//
// ===========================================================================
// 为什么夹具必须这么大，以及那个数是怎么定的
// ===========================================================================
//
// 陷阱是**规划器选中 HNSW 计划时**才发生的，不是数据一稀疏就发生的。
// 同一个库里，规划器给足够小的商家选的是「顺序扫描 + top-N 排序」——
// 精确、不踩陷阱。两种计划的估算代价（本机实测，见 tenant.go 那三个常量上的
// 完整数据）：
//
//	顺序扫描      ≈ 0.275 × N
//	HNSW + LIMIT  ≈ 349 + 4.31 × N × size / M
//
// 检索那条查询还 JOIN 了 products，多一条「先用 idx_products_listing 取本店
// 商品、再逐行算距离」的路，翻转点因此变成约 sqrt(22 × N)（N=30,340 时约 870 件）。
//
// 所以 victimProducts 取 2000：它是翻转点的两倍多，规划器会稳稳地选 HNSW；
// 同时它只占全库的 6%，40 条候选里属于它的期望值是 2.4 条，离 size=20 差着
// 一个数量级。**这条测试仍然自己核对计划真的是 HNSW**（阳性对照），
// 核不上就 Fatal 并说明该把夹具调大 —— 而不是安静地在一个没踩到陷阱的
// 数据集上变绿。
//
// ===========================================================================
// 代价：这条测试要跑约半分钟
// ===========================================================================
//
// 本机实测：造 3.3 万条随机单位向量约 3 s，重建 HNSW 索引约 24 s，清理约 2 s。
// 索引是**先删掉、灌完数据再建回来**的 —— 带着索引逐条插 3 万条要 124 s，
// 建一次只要 24 s。t.Cleanup 会把索引按 00016 里的原样建回去。
//
// 这半分钟买的是这个任务唯一一件真的会静默失效的事。把它挪出 test-db
// 之前请先想清楚：一条不在闸门里的测试等于没有（Makefile 里那段关于
// keel_fake_embedder 的话说的就是这件事）。

const (
	// hnswFixtureMerchants 是「几十家商家」。
	hnswFixtureMerchants = 40

	// hnswFixtureBulk 是每家陪跑商家的商品数。
	hnswFixtureBulk = 800

	// hnswVictimProducts 是被查的那家小商家的商品数。理由见文件头。
	hnswVictimProducts = 2000

	// hnswSearchSize 是检索要几条。20 是契约里 size 的默认值。
	hnswSearchSize = 20
)

// TestHNSWPostFilterTrapAndItsFix 是那个实验。
func TestHNSWPostFilterTrapAndItsFix(t *testing.T) {
	ctx := context.Background()
	p := pool(t)
	fx := seedHNSWScaleFixture(t, ctx)

	params := gendb.SearchProductsByVectorParams{
		QueryEmbedding: fx.queryLiteral,
		StoreID:        fx.victimStore,
		RegionID:       fx.victimRegion,
		InStockOnly:    false,
		RowLimit:       hnswSearchSize,
	}

	// ---------------------------------------------------------------
	// 阳性对照一：这家商家在 RLS 下**真的看得到**那么多向量。
	//
	// 少了它，下面那句「返回 0 条」既可能是陷阱，也可能只是夹具没播进去、
	// 或者 RLS 把它自己的数据也一起挡了。
	// ---------------------------------------------------------------
	visible := countVisibleVectors(t, ctx, p, fx.victim)
	if visible != hnswVictimProducts {
		t.Fatalf("被查商家在 RLS 下只看得到 %d 条向量，期望 %d —— 夹具坏了",
			visible, hnswVictimProducts)
	}

	// ---------------------------------------------------------------
	// 坏的那一半：只设 app.merchant_id，不设 hnsw.*
	// ---------------------------------------------------------------
	plan, bare := runBareTenantSearch(t, ctx, p, fx.victim, params)

	// 阳性对照二：规划器真的选了 HNSW。没选的话这一半在测别的东西。
	if !strings.Contains(plan, "idx_ptv_hnsw") {
		t.Fatalf("规划器没有选 HNSW 索引，这条测试没有踩到 post-filter 陷阱，"+
			"证明不了任何事。把 hnswVictimProducts（现在 %d）调大，"+
			"或者把 hnswFixtureBulk 调小到让本店占比更低。计划：\n%s",
			hnswVictimProducts, plan)
	}

	t.Logf("【坏的一半】不设 hnsw.* 时返回 %d 条（要 %d 条）。计划：\n%s",
		len(bare), hnswSearchSize, plan)

	if len(bare) >= hnswSearchSize {
		t.Fatalf("不设迭代扫描竟然也返回了 %d 条（要 %d 条）—— "+
			"post-filter 陷阱没有触发。这条测试的后半段因此证明不了任何事。"+
			"本店 %d 条 / 全库 %d 条，占比 %.1f%%，hnsw.ef_search 默认 40。计划：\n%s",
			len(bare), hnswSearchSize, hnswVictimProducts, fx.total,
			100*float64(hnswVictimProducts)/float64(fx.total), plan)
	}

	// ---------------------------------------------------------------
	// 好的那一半：走 WithTenant（它会设那三个 GUC）
	// ---------------------------------------------------------------
	var fixed []repository.SearchHit
	if err := repository.New(p).WithTenant(tenant.NewContext(ctx, fx.victim),
		func(tx repository.Tx) error {
			var err error
			fixed, err = tx.SearchProductsByVector(ctx, fx.scope(), fx.queryVector,
				repository.SearchFilters{}, hnswSearchSize)
			return err
		}); err != nil {
		t.Fatalf("走 WithTenant 的那一半直接报错了: %v", err)
	}
	if len(fixed) != hnswSearchSize {
		t.Fatalf("走 WithTenant 只返回了 %d 条，期望 %d 条 —— "+
			"withTenantTx 里那三行 hnsw.* 没有生效。"+
			"（同一条 SQL、同一批数据，上面那一半返回了 %d 条）",
			len(fixed), hnswSearchSize, len(bare))
	}

	// 返回的每一条都必须属于本店 —— 迭代扫描是「多扫几条」，
	// 不是「放宽过滤」。这一条挡的是「为了凑够条数把 RLS 绕过去」那种修法。
	for _, h := range fixed {
		if !fx.victimProductIDs[h.ID] {
			t.Fatalf("返回的 product_id=%d 不属于被查商家 —— 迭代扫描不该放宽过滤", h.ID)
		}
	}

	t.Logf("【好的一半】走 WithTenant（%s / max_scan_tuples / ef_search 已设）返回 %d 条，全部属于本店",
		"hnsw.iterative_scan", len(fixed))
	t.Logf("库：%d 家商家 / %d 条向量；被查商家 %d 条（占 %.1f%%）；size=%d",
		hnswFixtureMerchants+1, fx.total, hnswVictimProducts,
		100*float64(hnswVictimProducts)/float64(fx.total), hnswSearchSize)
}

// withTenantTx 真的把那三个 GUC 设上了 —— 直接读回来核一遍。
//
// 与上面那条不是重复：那条测的是**后果**（召回够不够），这条测的是**事实**
// （值对不对）。只有后果那一条的话，「值设成了 strict_order」或者
// 「max_scan_tuples 设成了 1」这类改动可能仍然让那条测试绿（数据量小的时候），
// 而它们改变的是生产行为。
func TestWithTenantSetsPgvectorGUCs(t *testing.T) {
	ctx := context.Background()
	idA, _ := seedTwoTenants(t)

	want := map[string]string{
		"hnsw.iterative_scan":  "relaxed_order",
		"hnsw.max_scan_tuples": "20000",
		"hnsw.ef_search":       "100",
	}
	r := repository.New(pool(t))
	// RawTenantTx 是 export_test.go 里那个只在测试里存在的口子：它跑的是
	// **同一个** withTenantTx，所以读回来的是生产路径上真实生效的值。
	if err := r.RawTenantTx(tenant.NewContext(ctx, idA), func(tx pgx.Tx) error {
		for name, exp := range want {
			// 收成 *string 而不是 string：GUC 压根没被设过时
			// current_setting(..., true) 返回的是 NULL，而 NULL 扫进 string
			// 报的是 pgx 的类型错误 —— 那句话不会告诉任何人「那三行没了」。
			//
			// GUC 名字拼进 SQL 而不是当绑定参数：current_setting($1, true)
			// 在扩展协议下解析不到那个两参数重载（实测 42704）。
			// 这里没有注入面 —— name 来自上面那张写死的表。
			var got *string
			if err := tx.QueryRow(ctx,
				`SELECT current_setting('`+name+`', true)`).Scan(&got); err != nil {
				return err
			}
			if got == nil {
				t.Errorf("%s 根本没被设过（current_setting 返回 NULL）—— "+
					"withTenantTx 里那三行 set_config 不在了。"+
					"后果是 HNSW 被规划器选中时检索安静地返回零条，"+
					"完整实测见 tenant.go 上那三个常量", name)
				continue
			}
			if *got != exp {
				t.Errorf("%s = %q，期望 %q", name, *got, exp)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 事务级（is_local）而不是会话级：会话级的设置会留在连接上，被池交给
	// 下一个请求 —— 与 TestTenantSettingDiesWithTheTransaction 同一个理由。
	// 这里从池里拿一条**裸**连接看事务结束之后剩下什么。
	//
	// 用 current_setting(name, true)（missing_ok）而不是 SHOW：pgvector 的
	// 共享库是按需加载的，没跑过向量操作的后端里 hnsw.* 只是**占位符**，
	// 而事务一结束占位符连同它的值一起消失 —— 这时 SHOW 报的是
	// 42704 unrecognized configuration parameter，而那不是这条断言要区分的失败。
	// missing_ok 把「不存在」和「是 off」都收成一个可判定的结果。
	p := pool(t)
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var leaked *string
	if err := conn.QueryRow(ctx,
		`SELECT current_setting('hnsw.iterative_scan', true)`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != nil && *leaked != "off" {
		t.Errorf("池里的连接上 hnsw.iterative_scan 还是 %q —— "+
			"那三个 set_config 的第三个参数不是 true（事务级），设置漏到了下一个请求", *leaked)
	}
	t.Logf("三个 GUC 在事务里生效，事务结束后连接上剩下的是 %v（nil = 占位符随事务一起消失）", leaked)
}

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------

type hnswFixture struct {
	victim           int64
	victimProductIDs map[int64]bool
	total            int64
	queryVector      []float32
	queryLiteral     string

	// 00020 之后检索按门店取价、按门店算 in_stock、按门店/大区排除不卖的款，
	// 所以两路召回都要一个 StoreScope。这条测试不关心那三样（它测的是索引
	// 扫描的机制），但**门店必须是真的那一家**：随手传 0 的话，
	// 两条 NOT EXISTS 与那个 LEFT JOIN LATERAL 会退化成恒真/恒空，
	// 于是将来任何一次把它们写坏的改动都不会在这条测试上留下痕迹。
	victimStore, victimRegion int64
}

// scope 是被查商家那家默认门店的作用域。
func (f hnswFixture) scope() repository.StoreScope {
	return repository.StoreScope{StoreID: f.victimStore, RegionID: f.victimRegion}
}

// seedHNSWScaleFixture 造那个真实规模的库。
//
// 向量在**数据库里**生成（l2_normalize(random 向量)），不是从 Go 送过去的：
// 3.3 万条 1024 维的文本字面量是 400 MB 的网络流量，本机实测要几十秒，
// 而在库里生成只要 3 秒。这些向量的唯一要求是「均匀散开、互不相同、归一化」——
// 它们不需要有语义，这条测试测的是索引扫描的机制，不是召回质量。
func seedHNSWScaleFixture(t *testing.T, ctx context.Context) hnswFixture {
	t.Helper()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	tag := fmt.Sprintf("hnswfx-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM product_text_vectors WHERE merchant_id IN
			   (SELECT id FROM merchants WHERE code LIKE $1)`,
			`DELETE FROM products   WHERE merchant_id IN
			   (SELECT id FROM merchants WHERE code LIKE $1)`,
			`DELETE FROM categories WHERE merchant_id IN
			   (SELECT id FROM merchants WHERE code LIKE $1)`,
			// stores → regions 排在 merchants 之前（00020 加的两条外键）。
			// 漏掉它们不会让这条测试红，会让下一轮那句
			// `DELETE FROM merchants` 以 23503 失败，而报错出现在别处。
			`DELETE FROM stores     WHERE merchant_id IN
			   (SELECT id FROM merchants WHERE code LIKE $1)`,
			`DELETE FROM regions    WHERE merchant_id IN
			   (SELECT id FROM merchants WHERE code LIKE $1)`,
			`DELETE FROM merchants  WHERE code LIKE $1`,
		} {
			if _, err := admin.Exec(c, stmt, tag+"%"); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
		if _, err := admin.Exec(c, `DROP FUNCTION IF EXISTS keel_test_rnd_vec(int)`); err != nil {
			t.Errorf("清理失败: %v", err)
		}
	})

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s\n%v", sql, err)
		}
	}

	// status = 2（停用）：这些是夹具不是营业中的店，理由同 seedTwoTenants。
	exec(`INSERT INTO merchants (code, name, status)
	      SELECT $1 || '-' || g, 'HNSW 陪跑 ' || g, 2
	        FROM generate_series(1, $2::int) g`, tag, hnswFixtureMerchants)
	exec(`INSERT INTO merchants (code, name, status) VALUES ($1, 'HNSW 被查店', 2)`,
		tag+"-victim")
	exec(`INSERT INTO categories (merchant_id, name, path, status)
	      SELECT id, '默认', '/', 1 FROM merchants WHERE code LIKE $1`, tag+"%")
	// 每家一个大区 + 一家默认门店。陪跑的那 40 家也建：这条测试要的是一个
	// **形状真实**的库，而真实的库里每家店都有门店；只给被查那家建的话，
	// 规划器面对的表大小和生产里不一样。
	// is_default = TRUE，不画围栏（默认店靠「全国兜底」接单；「非默认 且
	// 「非默认 且 无围栏」），这条测试不碰地理。
	exec(`INSERT INTO regions (merchant_id, code, name)
	      SELECT id, 'default', '默认大区' FROM merchants WHERE code LIKE $1`, tag+"%")
	exec(`INSERT INTO stores (merchant_id, region_id, code, name, is_default)
	      SELECT r.merchant_id, r.id, 'default', '默认门店', TRUE
	        FROM regions r
	        JOIN merchants m ON m.id = r.merchant_id AND m.code LIKE $1`, tag+"%")
	exec(`INSERT INTO products (merchant_id, category_id, title, status, published_at)
	      SELECT c.merchant_id, c.id, 'P' || c.merchant_id || '-' || g, 1, now()
	        FROM categories c
	        JOIN merchants m ON m.id = c.merchant_id AND m.code LIKE $1
	        CROSS JOIN LATERAL generate_series(1,
	             CASE WHEN m.code = $2 THEN $3::int ELSE $4::int END) g`,
		tag+"%", tag+"-victim", hnswVictimProducts, hnswFixtureBulk)

	exec(`CREATE OR REPLACE FUNCTION keel_test_rnd_vec(dims int) RETURNS vector AS $$
	        SELECT l2_normalize(array_agg(random() - 0.5)::vector)
	          FROM generate_series(1, dims)
	      $$ LANGUAGE sql VOLATILE`)

	// 先删索引、灌完再建回来。理由与耗时写在文件头。
	// 建回去的语句必须与 00016 逐字一致 —— 参数不同的话，这条测试跑完之后
	// 库里那个索引就不是迁移建出来的那一个了，而 internal/db 的
	// TestVectorIndexesAreHNSWWithDocumentedParams 会在下一个包里红。
	const recreate = `CREATE INDEX idx_ptv_hnsw ON product_text_vectors
	    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`
	exec(`DROP INDEX idx_ptv_hnsw`)
	indexBack := false
	t.Cleanup(func() {
		if indexBack {
			return
		}
		// 上面某一步 Fatal 了，索引还没建回去 —— 这里补上，
		// 否则下一个包里那条「索引建没建上」的测试会红在一个与它无关的原因上。
		if _, err := admin.Exec(context.Background(), recreate); err != nil {
			t.Errorf("把 idx_ptv_hnsw 建回去失败: %v", err)
		}
	})

	start := time.Now()
	exec(`INSERT INTO product_text_vectors
	        (product_id, merchant_id, content, embedding, model_name, model_version)
	      SELECT p.id, p.merchant_id, p.title, keel_test_rnd_vec($2::int), $3, 'hnsw-fixture'
	        FROM products p
	        JOIN merchants m ON m.id = p.merchant_id
	       WHERE m.code LIKE $1`, tag+"%", inference.Dim, inference.ModelName)
	insertTook := time.Since(start)

	start = time.Now()
	// maintenance_work_mem 与并行度只影响建索引的**速度**，不影响图的参数。
	exec(`SET maintenance_work_mem = '512MB'`)
	exec(`SET max_parallel_maintenance_workers = 7`)
	exec(recreate)
	// ANALYZE 不是可选的。规划器对 `merchant_id = current_merchant()` 的
	// 选择性估算全靠统计信息，而 current_merchant() 是 stable 函数、
	// 规划期就会被求值 —— 统计信息没跟上时它估出来的行数与真实值差一两个
	// 数量级，选出来的计划也就完全是另一个。实测：同一份夹具，ANALYZE 之前
	// 规划器选「先按 merchant_id 取本店商品再逐行算距离」（精确，踩不到陷阱），
	// ANALYZE 之后才选 HNSW。
	//
	// 这件事本身就是那三个 GUC 必须无条件设的理由之一：**陷阱踩不踩得到，
	// 取决于一张统计表新不新**，而那是应用完全管不着的东西。
	exec(`ANALYZE product_text_vectors`)
	exec(`ANALYZE products`)
	exec(`RESET maintenance_work_mem`)
	exec(`RESET max_parallel_maintenance_workers`)
	indexBack = true
	buildTook := time.Since(start)

	var total int64
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM product_text_vectors`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	t.Logf("夹具：%d 条向量（灌 %s，建索引 %s）", total, insertTook.Round(time.Millisecond),
		buildTook.Round(time.Millisecond))

	var victim int64
	if err := admin.QueryRow(ctx, `SELECT id FROM merchants WHERE code = $1`,
		tag+"-victim").Scan(&victim); err != nil {
		t.Fatal(err)
	}
	var victimStore, victimRegion int64
	if err := admin.QueryRow(ctx,
		`SELECT id, region_id FROM stores WHERE merchant_id = $1 AND is_default`,
		victim).Scan(&victimStore, &victimRegion); err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	rows, err := admin.Query(ctx, `SELECT id FROM products WHERE merchant_id = $1`, victim)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	qv := deterministicUnitVector(20260926)
	lit := make([]string, 0, inference.Dim)
	for _, x := range qv {
		lit = append(lit, fmt.Sprintf("%g", x))
	}
	return hnswFixture{
		victim:           victim,
		victimProductIDs: ids,
		total:            total,
		queryVector:      qv,
		queryLiteral:     "[" + strings.Join(lit, ",") + "]",
		victimStore:      victimStore,
		victimRegion:     victimRegion,
	}
}

// deterministicUnitVector 造一条确定的单位向量当查询向量。
//
// 确定而不是随机：这条测试要在失败时可复现。它不需要有语义 ——
// 全库的向量都是随机方向的，任何一个方向的「最近 40 条」都均匀落在各家商家上，
// 而那正是被测的那件事。
func deterministicUnitVector(seed uint64) []float32 {
	v := make([]float64, inference.Dim)
	var sum float64
	for i := range v {
		seed = seed*6364136223846793005 + 1442695040888963407
		x := float64(int64(seed>>11))/float64(int64(1)<<52) - 0.5
		v[i] = x
		sum += x * x
	}
	norm := math.Sqrt(sum)
	out := make([]float32, inference.Dim)
	for i, x := range v {
		out[i] = float32(x / norm)
	}
	return out
}

func countVisibleVectors(t *testing.T, ctx context.Context, p *pgxpool.Pool, merchant int64) int64 {
	t.Helper()
	var n int64
	if err := repository.New(p).RawTenantTx(tenant.NewContext(ctx, merchant),
		func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM product_text_vectors`).Scan(&n)
		}); err != nil {
		t.Fatal(err)
	}
	return n
}

// runBareTenantSearch 用**裸连接**发同一条生成出来的查询：只设 app.merchant_id，
// 不设那三个 hnsw.*。
//
// 这就是「把 withTenantTx 里那三行删掉」之后的样子。用生成的 db.Queries 而不是
// 在测试里手抄一遍 SQL —— 手抄的那份和生产跑的那条之间没有任何东西把它们钉住，
// 而这条测试的全部意义就是「同一条 SQL，只差那三行 GUC」。
func runBareTenantSearch(t *testing.T, ctx context.Context, p *pgxpool.Pool,
	merchant int64, params gendb.SearchProductsByVectorParams) (string, []gendb.SearchProductsByVectorRow) {
	t.Helper()

	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`,
		fmt.Sprint(merchant)); err != nil {
		t.Fatal(err)
	}

	rows, err := gendb.New(tx).SearchProductsByVector(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	// 取一次计划做阳性对照。EXPLAIN 单独发一条，不与上面那次共用 ——
	// 共用要么改查询文本（那就不是同一条 SQL 了），要么解析 EXPLAIN 的输出
	// 当结果。两者都会让「跑的是生成出来的那条查询」这句话不再成立。
	plan := explainVectorSearch(t, ctx, tx, params)
	return plan, rows
}

// trimVectorLiteral 把计划里那条 1024 维的向量字面量压成一个省略号。
//
// 不压的话，一次失败会往终端里吐 12 KB 的浮点数，而要看的那一行
// （Rows Removed by Filter）被埋在里面 —— 一条读不到的诊断等于没有诊断。
func trimVectorLiteral(line string) string {
	i := strings.Index(line, "'[")
	if i < 0 {
		return line
	}
	j := strings.Index(line[i:], "]'")
	if j < 0 {
		return line
	}
	return line[:i] + "'[…1024 维…]'" + line[i+j+2:]
}

func explainVectorSearch(t *testing.T, ctx context.Context, tx pgx.Tx,
	params gendb.SearchProductsByVectorParams) string {
	t.Helper()

	const q = `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF)
	SELECT p.id
	  FROM product_text_vectors v
	  JOIN products p ON p.id = v.product_id
	 WHERE p.deleted_at IS NULL AND p.status = 1
	 ORDER BY v.embedding <=> $1::vector
	 LIMIT $2`
	rows, err := tx.Query(ctx, q, params.QueryEmbedding, params.RowLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString("  " + trimVectorLiteral(line) + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// 查询向量也要过归一化与维度那两道闸门。
//
// `<=>` 算的是余弦距离，它对**查询侧**的模长同样敏感：一个范数是 7.3 的
// 查询向量不会报错、不会变慢、照样返回结果，只是排序失真 ——
// 与 §2.3 说的入库侧是同一件事的另一半，而入库侧那一半已经有
// semantic_test.go 守着了。
//
// 阳性对照：同一条路径上，一个真的归一化了的向量必须能过。
// 没有它，「两个坏向量都被拒了」也可能只是这条查询整个不通。
func TestSearchRejectsBadQueryVectors(t *testing.T) {
	ctx := context.Background()
	idA, _ := seedTwoTenants(t)
	r := repository.New(pool(t))
	// 门店作用域取一次就够：这条测试要挡的是**查询向量**本身，
	// 作用域只是让那条查询在阳性对照里能真的跑起来。
	sc := defaultScope(t, idA)

	unit := make([]float32, inference.Dim)
	x := float32(1 / math.Sqrt(float64(inference.Dim)))
	for i := range unit {
		unit[i] = x
	}
	unnormalized := make([]float32, inference.Dim)
	copy(unnormalized, unit)
	unnormalized[0] = 7.3
	wrongDim := unit[:inference.Dim-1]

	for _, c := range []struct {
		name string
		vec  []float32
		want error
	}{
		{"没归一化", unnormalized, repository.ErrVectorNotNormalized},
		{"维度不对", wrongDim, repository.ErrVectorWrongDim},
	} {
		err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
			_, err := tx.SearchProductsByVector(ctx, sc, c.vec, repository.SearchFilters{}, 10)
			return err
		})
		if !errors.Is(err, c.want) {
			t.Errorf("%s 的查询向量：期望 %v，实得 %v —— "+
				"它不会报错、不会变慢、照样返回结果，只是排序失真", c.name, c.want, err)
		}
	}

	// 阳性对照。
	if err := r.WithTenant(tenant.NewContext(ctx, idA), func(tx repository.Tx) error {
		_, err := tx.SearchProductsByVector(ctx, sc, unit, repository.SearchFilters{}, 10)
		return err
	}); err != nil {
		t.Fatalf("一条合格的查询向量也被拒了：%v —— 上面那两条断言因此说明不了什么", err)
	}
}
