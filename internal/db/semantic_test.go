package db_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/db"
)

// 语义检索底座（数据模型 §8 / 迁移 00016）的行为闸门。
//
// migrate_test.go 那一批是**形状**检查：从系统目录枚举，问「策略挂了吗、
// GRANT 面对不对、触发器在不在」。它们盖不住这一层的三件事：
//
//   - 向量表的 RLS **拦不拦得住**。形状检查对 `USING (merchant_id =
//     current_merchant() OR true)` 这种写法全部照绿（策略在、名字对、cmd 是
//     ALL），而库照漏。inventories 早就有一条真实数据的行为测试
//     （rls_test.go 的 TestInventoriesPolicyBlocksCrossTenantAccess），
//     向量表同样需要一条 —— 而且向量表比 inventories 更值得有：
//     embedding 是商品文本的完整表示，泄露一张竞对的向量表等于泄露它的全部商品。
//   - HNSW 索引**建上了没有**。没建索引的向量检索仍然返回正确结果，只是走
//     顺序扫描 —— 功能完全正常，只有延迟变了，没有任何断言会红。
//   - search_vector 由 **search_text** 生成，而不是由 title 生成。
//     改成 title 不会报错、不会让任何形状断言变红，只会让中文关键词召回悄悄失效
//     （`to_tsvector('simple', '羊毛衫')` 整句一个 token，搜「毛衫」零结果）。
//
// 每条测试都自带**对照**：只断言「查不到 / 改不动」的话，夹具没播进去时它照样绿。

// vectorLiteral 造一条 dims 维、每一维都是 v 的向量字面量。
// 不在 SQL 里用 array_fill：那会让语句里出现一个和被测内容无关的函数，
// 而这些测试要证明的是列类型真的是 vector(N) —— 维度不对时 INSERT 直接报错。
func vectorLiteral(dims int, v float64) string {
	parts := make([]string, dims)
	s := strconv.FormatFloat(v, 'f', -1, 64)
	for i := range parts {
		parts[i] = s
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// semanticFixture 播两家商家，各一个类目、一个商品、一行文本向量。
// 返回 (商家A, 商家B, 商家 -> 商品 id)。清理由 t.Cleanup 负责。
func semanticFixture(t *testing.T, admin *pgx.Conn, tag string) (int64, int64, map[int64]int64) {
	t.Helper()
	ctx := context.Background()

	suffix := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
	// status = 2（停用）：这两家是夹具不是营业中的店。tenant.Preflight 断言的是
	// **整个库**的形态（配了默认商家时活跃商家只能有一家），插一家活跃商家会在
	// 别的包里表现为一次随机的断言失败 —— 理由同 rls_test.go 里那两处。
	var idA, idB int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2)
		 RETURNING id`, suffix+"-a", suffix+"-b").Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, suffix+"-b").Scan(&idB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{idA, idB}
		// 向量表是 ON DELETE CASCADE 的，删 products 会带走它们；仍然逐张删，
		// 因为「级联真的在」是别的断言的事，清理不该依赖一个未被本测试验证的性质。
		for _, stmt := range []string{
			`DELETE FROM product_text_vectors  WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_image_vectors WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_understanding WHERE merchant_id = ANY($1)`,
			`DELETE FROM product_clusters      WHERE merchant_id = ANY($1)`,
			`DELETE FROM products              WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories            WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants             WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, ids); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	products := map[int64]int64{}
	for _, m := range []int64{idA, idB} {
		var catID, prodID int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'c','/c/')
			 RETURNING id`, m).Scan(&catID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO products (merchant_id, category_id, title, status, published_at)
			 VALUES ($1,$2,$3,1,now()) RETURNING id`,
			m, catID, fmt.Sprintf("商品-%d", m)).Scan(&prodID); err != nil {
			t.Fatal(err)
		}
		products[m] = prodID
	}
	return idA, idB, products
}

// 向量表的 RLS 用**真实数据**走一遍读、改、插三个面。
//
// 形状检查（TestTenantPoliciesArePresentAndExact）证明的是「有一条叫 tenant
// 的策略，谓词文本等于 (merchant_id = current_merchant())」。那条断言很严，
// 但它守的是**迁移写了什么**，不是**数据库拦不拦得住** —— 两者之间隔着
// FORCE 有没有加、连接角色能不能绕过、WITH CHECK 回退到哪一侧这几件事，
// 而它们各自都有一个「全部形状断言照绿」的失效形态。
func TestVectorTablesPolicyBlocksCrossTenantAccess(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	idA, idB, products := semanticFixture(t, admin, "vecrls")

	const dims = 1024
	for _, m := range []int64{idA, idB} {
		if _, err := admin.Exec(ctx,
			`INSERT INTO product_text_vectors
			     (product_id, merchant_id, content, embedding, model_name, model_version)
			 VALUES ($1, $2, $3, $4::vector, 'Qwen3-Embedding-0', 'test')`,
			products[m], m, fmt.Sprintf("商品-%d 的拼接文本", m),
			vectorLiteral(dims, 0.001)); err != nil {
			t.Fatal(err)
		}
	}

	app, err := db.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)
	// 以商家 A 的身份连着做下面几件事。
	if _, err := app.Exec(ctx,
		`SELECT set_config('app.merchant_id', $1, false)`, fmt.Sprint(idA)); err != nil {
		t.Fatal(err)
	}

	t.Run("读不到别家的向量与拼接文本", func(t *testing.T) {
		var n int
		if err := app.QueryRow(ctx,
			`SELECT count(*) FROM product_text_vectors WHERE product_id = $1`,
			products[idB]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("商家 A 看得到商家 B 的文本向量（%d 行）—— RLS 的读侧没生效。"+
				"embedding 是商品文本的完整表示，这不是「少量元数据泄露」", n)
		}
		// 对照：自己的那一行必须看得见。没有这一条，上面那句断言在夹具没播进去
		// 或者表被清空的那天照样绿。
		if err := app.QueryRow(ctx,
			`SELECT count(*) FROM product_text_vectors WHERE product_id = $1`,
			products[idA]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("商家 A 看不到自己的文本向量（%d 行）—— 夹具或策略写反了", n)
		}
	})

	t.Run("按向量距离排序也越不过策略", func(t *testing.T) {
		// 这一条不是上一条的重复。向量召回的真实形状是
		// `ORDER BY embedding <=> $1 LIMIT n`，走的是 HNSW 索引扫描，
		// 而索引扫描与顺序扫描是两条不同的执行路径 —— RLS 的 qual 必须
		// 在索引返回候选**之后**仍然被套用（那正是 00016 文件头第三节讲的
		// post-filter：过滤发生在索引之后，不是之前）。
		//
		// 夹具只有两行，规划器完全可能选顺序扫描，那样这条测试就退化成上一条的
		// 重复，而它**照样是绿的** —— 正是这个仓库反复在抓的「断言存在但和被测
		// 路径没有因果关系」。所以先关掉顺序扫描，再用 EXPLAIN 当场确认走的
		// 真是 idx_ptv_hnsw，确认不了就红在这里，而不是含糊地过去。
		if _, err := app.Exec(ctx, `SET enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := app.Exec(context.Background(), `RESET enable_seqscan`); err != nil {
				t.Errorf("恢复 enable_seqscan 失败: %v", err)
			}
		}()
		var plan string
		if err := app.QueryRow(ctx,
			`EXPLAIN (FORMAT JSON) SELECT product_id FROM product_text_vectors
			  ORDER BY embedding <=> $1::vector LIMIT 10`,
			vectorLiteral(dims, 0.001)).Scan(&plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, "idx_ptv_hnsw") {
			t.Fatalf("这条查询没走 HNSW 索引，计划是：%s —— "+
				"那么下面的断言证明的是顺序扫描路径上的 RLS，不是向量召回路径上的", plan)
		}

		rows, err := app.Query(ctx,
			`SELECT product_id FROM product_text_vectors
			  ORDER BY embedding <=> $1::vector LIMIT 10`, vectorLiteral(dims, 0.001))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != products[idA] {
			t.Fatalf("按距离排序拿到 %v，期望只有商家 A 自己的 %d —— "+
				"向量召回这条路径上策略没生效", got, products[idA])
		}
	})

	t.Run("改不动别家的向量", func(t *testing.T) {
		tag, err := app.Exec(ctx,
			`UPDATE product_text_vectors SET content = '被改过了' WHERE product_id = $1`,
			products[idB])
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("商家 A 改动了商家 B 的向量行（%d 行）", tag.RowsAffected())
		}
		// 真的没改到：绕过 RLS 用管理员连接回读。
		var content string
		if err := admin.QueryRow(ctx,
			`SELECT content FROM product_text_vectors WHERE product_id = $1`,
			products[idB]).Scan(&content); err != nil {
			t.Fatal(err)
		}
		if content == "被改过了" {
			t.Fatal("商家 B 的 content 真的被改了")
		}
	})

	t.Run("插不进别家商品的向量", func(t *testing.T) {
		// WITH CHECK 那一侧。先删掉 B 的行（管理员连接），否则主键冲突会
		// 先于 RLS 报错，这条断言就变成在测主键 —— 与 rls_test.go 里
		// inventories 那条踩过的是同一个坑。
		if _, err := admin.Exec(ctx,
			`DELETE FROM product_text_vectors WHERE product_id = $1`, products[idB]); err != nil {
			t.Fatal(err)
		}

		_, err := app.Exec(ctx,
			`INSERT INTO product_text_vectors
			     (product_id, merchant_id, content, embedding, model_name, model_version)
			 VALUES ($1, $2, '偷插的', $3::vector, 'Qwen3-Embedding-0', 'test')`,
			products[idB], idB, vectorLiteral(dims, 0.002))
		if err == nil {
			t.Fatal("商家 A 往商家 B 的商品上插进了向量行 —— WITH CHECK 没生效")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("期望 42501（new row violates row-level security policy），实得: %v", err)
		}

		// 对照：同一条语句写自己名下必须成功。没有它，上面那条断言在
		// 「向量列写不进去」「FK 挂了」之类与租户无关的原因下也会绿。
		if _, err := admin.Exec(ctx,
			`DELETE FROM product_text_vectors WHERE product_id = $1`, products[idA]); err != nil {
			t.Fatal(err)
		}
		if _, err := app.Exec(ctx,
			`INSERT INTO product_text_vectors
			     (product_id, content, embedding, model_name, model_version)
			 VALUES ($1, '自己的', $2::vector, 'Qwen3-Embedding-0', 'test')`,
			products[idA], vectorLiteral(dims, 0.002)); err != nil {
			t.Fatalf("商家 A 写自己名下的向量也失败了，说明上一条断言的红不是 RLS 给的: %v", err)
		}
		// 顺带把 merchant_id 的 DEFAULT current_merchant() 钉住：上面那条
		// INSERT 里根本没有 merchant_id 这一列（db/queries 不许出现它，
		// 见 scripts/check_query_tenancy.py），列必须由默认值填成 A。
		var got int64
		if err := admin.QueryRow(ctx,
			`SELECT merchant_id FROM product_text_vectors WHERE product_id = $1`,
			products[idA]).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != idA {
			t.Fatalf("不写 merchant_id 时它被填成了 %d，期望 %d —— "+
				"DEFAULT current_merchant() 没生效，Task 3 的入库语句会写不出来", got, idA)
		}
	})

	t.Run("另外三张表同样拦得住", func(t *testing.T) {
		// 四张表是同一批、同一套策略，逐张走完整的读改插太长；这里对其余三张
		// 各做一次跨租户读，证明它们不是「策略写在迁移里但漏了 FORCE」。
		// 不做这一步的话，漏掉某一张表的 ENABLE 只会在 migrate_test 的形状
		// 检查里红 —— 而形状检查正是本文件存在的理由所反对的那种唯一依据。
		for _, tbl := range []string{
			"product_image_vectors", "product_understanding", "product_clusters",
		} {
			switch tbl {
			case "product_image_vectors":
				if _, err := admin.Exec(ctx,
					`INSERT INTO product_image_vectors
					     (product_id, merchant_id, image_url, embedding, model_name, model_version)
					 VALUES ($1,$2,'http://x/1.jpg',$3::vector,'clip-vit-l','test')`,
					products[idB], idB, vectorLiteral(768, 0.001)); err != nil {
					t.Fatal(err)
				}
			case "product_understanding":
				if _, err := admin.Exec(ctx,
					`INSERT INTO product_understanding
					     (product_id, merchant_id, pipeline_version) VALUES ($1,$2,'v1')`,
					products[idB], idB); err != nil {
					t.Fatal(err)
				}
			case "product_clusters":
				if _, err := admin.Exec(ctx,
					`INSERT INTO product_clusters
					     (product_id, merchant_id, cluster_id, confidence, method)
					 VALUES ($1,$2,1,0.9,1)`, products[idB], idB); err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err := app.QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE product_id = $1`, tbl),
				products[idB]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s: 商家 A 看得到商家 B 的 %d 行", tbl, n)
			}
			// 对照：管理员看得见，证明刚才那行真的插进去了。
			if err := admin.QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s WHERE product_id = $1`, tbl),
				products[idB]).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("%s: 夹具没播进去（管理员也只看到 %d 行），"+
					"上面那条断言证明不了任何事", tbl, n)
			}
		}
	})
}

// HNSW 索引必须真的建上，且参数就是语义检索层 §2.3 定的那一组。
//
// 没有索引时向量检索**结果完全正确**，只是走顺序扫描 —— 也就是说
// 「索引忘了建」这件事不会让任何功能测试变红，只会让 M3 的延迟预算（§8）
// 在数据量长起来之后悄悄崩掉。所以它需要一条自己的断言。
//
// 参数也要查：`WITH (m = 16, ef_construction = 64)` 写错或漏掉时，pgvector
// 用自己的默认值（m=16, ef_construction=64 —— 眼下恰好相同），索引照样能建、
// 照样能用。今天相同不代表明天相同，而「文档写了一组值、库里是另一组」
// 正是这个仓库反复在消灭的那类脱节。
func TestVectorIndexesAreHNSWWithDocumentedParams(t *testing.T) {
	conn := migratedConn(t)
	ctx := context.Background()

	for _, tc := range []struct{ index, table, opclass string }{
		{"idx_ptv_hnsw", "product_text_vectors", "vector_cosine_ops"},
		{"idx_piv_hnsw", "product_image_vectors", "vector_cosine_ops"},
	} {
		var am, opclass string
		var reloptions []string
		err := conn.QueryRow(ctx, `
			SELECT am.amname,
			       (SELECT oc.opcname
			          FROM pg_opclass oc
			         WHERE oc.oid = ix.indclass[0]),
			       coalesce(i.reloptions, '{}')
			  FROM pg_index ix
			  JOIN pg_class i ON i.oid = ix.indexrelid
			  JOIN pg_class c ON c.oid = ix.indrelid
			  JOIN pg_am am   ON am.oid = i.relam
			 WHERE i.relname = $1 AND c.relname = $2`, tc.index, tc.table).
			Scan(&am, &opclass, &reloptions)
		if err != nil {
			t.Errorf("%s 上的索引 %s 查不到：%v —— "+
				"没有它，向量检索走顺序扫描，结果正确而延迟预算崩掉，没有任何功能测试会红",
				tc.table, tc.index, err)
			continue
		}
		if am != "hnsw" {
			t.Errorf("%s 的访问方法是 %q，期望 hnsw", tc.index, am)
		}
		if opclass != tc.opclass {
			t.Errorf("%s 的算子类是 %q，期望 %q —— "+
				"向量按 L2 归一化后写入（§2.3），配的必须是 cosine",
				tc.index, opclass, tc.opclass)
		}
		want := map[string]bool{"m=16": false, "ef_construction=64": false}
		for _, opt := range reloptions {
			if _, ok := want[opt]; ok {
				want[opt] = true
			}
		}
		for opt, seen := range want {
			if !seen {
				t.Errorf("%s 的 reloptions 是 %v，缺 %s —— 语义检索层 §2.3 定的是 "+
					"m = 16 / ef_construction = 64", tc.index, reloptions, opt)
			}
		}
	}

	// 关键词召回那一侧：GIN on search_vector。
	var am string
	if err := conn.QueryRow(ctx, `
		SELECT am.amname FROM pg_index ix
		  JOIN pg_class i ON i.oid = ix.indexrelid
		  JOIN pg_am am   ON am.oid = i.relam
		 WHERE i.relname = 'idx_products_fts'`).Scan(&am); err != nil {
		t.Fatalf("idx_products_fts 查不到：%v", err)
	}
	if am != "gin" {
		t.Errorf("idx_products_fts 的访问方法是 %q，期望 gin", am)
	}
}

// pgvector 必须装着，且版本够得上 hnsw.iterative_scan。
//
// 前半段其实由迁移自己保证（00016 的第一条语句就是 CREATE EXTENSION，
// 装不上整条链就断）。真正需要一条断言的是后半段：**0.7 装得上、迁移全绿、
// 索引照建，只是 hnsw.iterative_scan 不存在** —— 而按 M3 计划第一条，
// 那个 GUC 是多租户下向量召回能用的前提，不是优化项。
// 把底座镜像降一档，这里要红，而且要红在版本上，不是红在别的地方。
func TestPgvectorSupportsIterativeScan(t *testing.T) {
	conn := migratedConn(t)
	ctx := context.Background()

	var ver string
	if err := conn.QueryRow(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&ver); err != nil {
		t.Fatalf("vector 扩展没装：%v", err)
	}
	major, minor := 0, 0
	if _, err := fmt.Sscanf(ver, "%d.%d", &major, &minor); err != nil {
		t.Fatalf("解析不了 pgvector 版本 %q: %v", ver, err)
	}
	if major == 0 && minor < 8 {
		t.Fatalf("pgvector 版本是 %s，hnsw.iterative_scan 要 0.8+ —— "+
			"没有它，小商家的向量召回会正常地返回零条（00016 文件头第三节）", ver)
	}

	// 版本号对不等于 GUC 真的在。PostgreSQL 对「前缀没被任何已加载库认领」的
	// 自定义参数是宽容的：`SET hnsw.xxx = '随便什么'` 会被当成占位符接受而不报错。
	// 所以先摸一下 vector 类型把库加载进来，再故意设一个非法值 —— 只有真正
	// 注册过的枚举型 GUC 才会拒绝它。
	if _, err := conn.Exec(ctx, `SELECT '[1,2,3]'::vector`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET hnsw.iterative_scan = 'nonsense_value'`); err == nil {
		t.Fatal("hnsw.iterative_scan 接受了一个非法值 —— 说明它只是个占位符，" +
			"这个库里根本没有注册过这个参数")
	}
	var got string
	if _, err := conn.Exec(ctx, `SET hnsw.iterative_scan = 'relaxed_order'`); err != nil {
		t.Fatalf("设不上 hnsw.iterative_scan: %v", err)
	}
	if err := conn.QueryRow(ctx, `SHOW hnsw.iterative_scan`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "relaxed_order" {
		t.Fatalf("hnsw.iterative_scan 读回来是 %q", got)
	}
	t.Logf("pgvector %s，hnsw.iterative_scan 可用（默认 off，取值由 Task 4 的实验定）", ver)
}

// search_vector 必须由 **search_text** 生成，不是由 title 生成。
//
// 这一条是整条关键词召回路成立与否的分界线（语义检索层 §3）：
// `to_tsvector('simple', title)` 对中文等于不分词，「羊毛衫」整句一个 token，
// 用户搜「毛衫」零结果。分词发生在应用层，落进 search_text。
//
// 把生成表达式改成 coalesce(title,'') 之后：列还在、GIN 索引还在、
// 任何「索引建上了吗」的断言照绿，全文检索也照样能用（对英文甚至还挺好），
// 只有中文悄悄失效。所以判据必须是**因果**的：让 title 与 search_text 的
// token 互不相交，然后两个方向各断言一次。
func TestSearchVectorIsGeneratedFromSearchText(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	idA, _, products := semanticFixture(t, admin, "fts")
	_ = idA

	// title 与 search_text 刻意没有一个共同 token。
	const title = "titletoken"
	const searchText = "羊毛 毛衫 searchtoken"
	if _, err := admin.Exec(ctx,
		`UPDATE products SET title = $2, search_text = $3 WHERE id = $1`,
		products[idA], title, searchText); err != nil {
		t.Fatal(err)
	}

	var fromSearchText, fromTitle bool
	if err := admin.QueryRow(ctx, `
		SELECT search_vector @@ to_tsquery('simple', 'searchtoken'),
		       search_vector @@ to_tsquery('simple', 'titletoken')
		  FROM products WHERE id = $1`, products[idA]).
		Scan(&fromSearchText, &fromTitle); err != nil {
		t.Fatal(err)
	}
	if !fromSearchText {
		t.Error("search_text 里的 token 没进 search_vector —— " +
			"生成表达式不是 to_tsvector('simple', coalesce(search_text,''))")
	}
	if fromTitle {
		t.Error("title 里的 token 进了 search_vector —— 生成表达式读的是 title。" +
			"这不会报任何错，只会让中文关键词召回悄悄失效：" +
			"to_tsvector('simple','羊毛衫') 整句一个 token，搜「毛衫」零结果")
	}

	// 上面两条只证明「读的是 search_text 这一列」。这一条证明 bigram 方案
	// 本身在这个配置下真的成立：应用层切出来的「毛衫」召得回，而未切分的
	// 整词召不回。没有它，把 search_text 直接写成未切分的标题也能通过上面两条。
	var bigramHit, wholeHit bool
	if err := admin.QueryRow(ctx, `
		SELECT search_vector @@ to_tsquery('simple', '毛衫'),
		       to_tsvector('simple', '羊毛衫') @@ to_tsquery('simple', '毛衫')
		  FROM products WHERE id = $1`, products[idA]).Scan(&bigramHit, &wholeHit); err != nil {
		t.Fatal(err)
	}
	if !bigramHit {
		t.Error("应用层切出来的 bigram「毛衫」召不回 —— simple 配置下 search_text 的切分没起作用")
	}
	if wholeHit {
		t.Error("to_tsvector('simple','羊毛衫') 居然能被「毛衫」召回 —— " +
			"那说明这个库装了中文分词器，bigram 方案的**前提**（§3 开头那句" +
			"「simple 对中文完全不可用」）在这个环境下不成立，整个选型要重看")
	}
}

// 增量重算判据的原材料（M3 计划第四条 / 00016 文件头第四节）。
//
// 本任务不实现重算（那是 Task 3）。但判据的形状要立得住，而它依赖三个
// **事实**，每一个都可以是错的，错了 Task 3 会建在沙子上：
//
//	① 改 title 会让 products.updated_at 前进        —— 判据的触发点
//	② 改 sales_count **也**会让它前进               —— 判据为什么不能只有这一半
//	③ 改 products 不会让 product_text_vectors.updated_at 跟着动
//	                                                 —— 这个先后关系是真信号
//
// ③ 看着理所当然，但它恰恰是最容易被「顺手优化」掉的：有人给向量表也挂一个
// 跟随 products 的触发器（"让派生数据的时间戳保持一致"），这个先后关系当场
// 变成恒等式，判据从此永远说「没过期」，而且不会有任何东西报错。
//
// ② 是这条测试真正的产出：计划里那句「updated_at 的先后关系是一个现成的、
// 可机械检查的判据」只对了一半。touch_updated_at 挂在整张 products 上，
// 一次下单（sales_count / total_stock）就会把全店商品判成向量过期 ——
// 而重算 embedding 的钱是真花出去的。所以判定那一半必须落在
// product_understanding.input_hashes 上（数据模型 §8「只认一处」）。
func TestStalenessCriterionRawMaterial(t *testing.T) {
	ctx := context.Background()

	if _, err := migrate(t); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	idA, _, products := semanticFixture(t, admin, "stale")
	pid := products[idA]

	if _, err := admin.Exec(ctx,
		`INSERT INTO product_text_vectors
		     (product_id, merchant_id, content, embedding, model_name, model_version)
		 VALUES ($1, $2, '拼接文本', $3::vector, 'Qwen3-Embedding-0', 'test')`,
		pid, idA, vectorLiteral(1024, 0.001)); err != nil {
		t.Fatal(err)
	}

	stamps := func() (prod, vec time.Time) {
		t.Helper()
		if err := admin.QueryRow(ctx,
			`SELECT p.updated_at, v.updated_at
			   FROM products p JOIN product_text_vectors v ON v.product_id = p.id
			  WHERE p.id = $1`, pid).Scan(&prod, &vec); err != nil {
			t.Fatal(err)
		}
		return
	}

	prod0, vec0 := stamps()

	// ① 改 title —— 文本真的变了，向量该重算。
	if _, err := admin.Exec(ctx,
		`UPDATE products SET title = '改过的标题' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	prod1, vec1 := stamps()
	if !prod1.After(prod0) {
		t.Fatalf("改 title 之后 products.updated_at 没前进：%v → %v —— "+
			"判据的触发点不成立，Task 3 的增量重算永远不会被唤醒", prod0, prod1)
	}
	if !vec1.Equal(vec0) {
		t.Fatalf("改 products 之后 product_text_vectors.updated_at 跟着动了：%v → %v —— "+
			"两个时间戳的先后关系从此是恒等式，判据永远说「没过期」，而且不会报错", vec0, vec1)
	}

	// ② 改 sales_count —— 文本一个字没变，向量不该重算。
	if _, err := admin.Exec(ctx,
		`UPDATE products SET sales_count = sales_count + 1 WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	prod2, _ := stamps()
	if !prod2.After(prod1) {
		t.Fatalf("改 sales_count 之后 products.updated_at 没前进：%v → %v —— "+
			"touch_updated_at 的行为和这条判据的前提不一致，00016 文件头第四节要重写",
			prod1, prod2)
	}
	t.Logf("改 sales_count 也让 products.updated_at 前进了（%v → %v）—— "+
		"这就是为什么「先后关系」只能当触发点，判定必须落在 "+
		"product_understanding.input_hashes ->> 'text_embedding' 上（数据模型 §8）",
		prod1, prod2)

	// ③ input_hashes 这一格真的能按 processor 分别存取 —— 判据的后半段落在
	// 它身上，而它是 JSONB，写错一层嵌套不会报错，只会让 ->> 返回 NULL。
	if _, err := admin.Exec(ctx,
		`INSERT INTO product_understanding (product_id, merchant_id, pipeline_version, input_hashes)
		 VALUES ($1, $2, 'v1', $3::jsonb)`,
		pid, idA, `{"text_embedding":"abc","search_text":"def"}`); err != nil {
		t.Fatal(err)
	}
	var textHash, searchHash, missing *string
	if err := admin.QueryRow(ctx,
		`SELECT input_hashes ->> 'text_embedding',
		        input_hashes ->> 'search_text',
		        input_hashes ->> 'image_embedding'
		   FROM product_understanding WHERE product_id = $1`, pid).
		Scan(&textHash, &searchHash, &missing); err != nil {
		t.Fatal(err)
	}
	if textHash == nil || *textHash != "abc" || searchHash == nil || *searchHash != "def" {
		t.Fatalf("input_hashes 按 processor 取不出来：text_embedding=%v search_text=%v",
			textHash, searchHash)
	}
	if missing != nil {
		t.Fatalf("没写过的 processor 取出了 %q，期望 NULL —— "+
			"Task 3 判定「从没算过」靠的就是这个 NULL", *missing)
	}
}
