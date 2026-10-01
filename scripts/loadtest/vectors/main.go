// Command vectors 给压测种子（scripts/loadtest/seed）造出来的商品补文本向量，好让 /search 的向量路能跑起来。
//
//	go run ./scripts/loadtest/vectors -core-dsn postgres://keel:keel@<core-ip>:5432/keel?sslmode=disable \
//	  -embed http://<inference-ip>:8081
//
// # 这是近似，不是真实索引
//
// 种子标题 = 品牌（2 字）+ 形容词 + 叶子类目名 + " 型号"。10 万件商品逐件过 CPU 版引擎要一个多小时，
// 所以这里按「形容词 + 叶子类目名」去重（30 × 50 ≈ 1500 种），每种算一次向量，同一种的商品共用这一条：
//
//   - 送进模型的是 EmbedContent(标题 = 形容词+类目名, 类目 = 类目名)：**品牌、型号、副标题都不参与向量**。
//     查「栖木复古地毯」这类带品牌的长尾词时，向量路分不出品牌，只能靠关键词路。
//   - 同一种的约 67 件商品向量完全相同，HNSW 图里有大量零距离重复点，召回形态与真实目录不同。
//   - product_text_vectors.content 写的是实际送进模型的那段近似文本；而 product_understanding.input_hashes
//     写的是按**真实**标题/副标题/类目算出的指纹（与 IndexService.decide 同一套），否则 app 里的增量
//     索引任务会判定全部过期、把 10 万件重新送进引擎，和压测抢 CPU。
//
// 只处理还没有向量行的商品（demo 种子那几十件由 app 自己的索引任务算，不碰）。model_name / model_version
// 用引擎响应里的值。写之前先 DROP 向量索引、写完按 00016 的同一定义重建（批量写比逐行维护 HNSW 快得多）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/search"
	"github.com/keel/keel/internal/understanding"
)

func main() {
	coreDSN := flag.String("core-dsn", "", "core 库（管理员账号）")
	endpoint := flag.String("embed", "", "推理引擎地址，如 http://192.168.16.2:8081")
	batch := flag.Int("batch", 32, "每次 /v1/embeddings 的条数（≤ inference.DefaultBatchSize）")
	flag.Parse()
	if *coreDSN == "" || *endpoint == "" {
		log.Fatal("要给 -core-dsn 与 -embed")
	}
	ctx := context.Background()
	start := time.Now()
	db, err := pgx.Connect(ctx, *coreDSN)
	must(err)
	defer db.Close(ctx)

	type prod struct {
		id, merchant         int64
		title, sub, cat, key string
	}
	rows, err := db.Query(ctx, `SELECT p.id, p.merchant_id, p.title, COALESCE(p.subtitle, ''), c.name
	  FROM products p JOIN categories c ON c.id = p.category_id
	 WHERE p.deleted_at IS NULL
	   AND NOT EXISTS (SELECT 1 FROM product_text_vectors v WHERE v.product_id = p.id)
	 ORDER BY p.id`)
	must(err)
	var prods []prod
	skipped := 0
	for rows.Next() {
		var p prod
		must(rows.Scan(&p.id, &p.merchant, &p.title, &p.sub, &p.cat))
		// 去掉 2 字品牌前缀与最后一个空格之后的型号，剩下的应当以类目名结尾。
		r := []rune(p.title)
		rest := string(r[min(2, len(r)):])
		if i := strings.LastIndex(rest, " "); i > 0 {
			rest = rest[:i]
		}
		if !strings.HasSuffix(rest, p.cat) {
			skipped++
			continue
		}
		p.key = rest
		prods = append(prods, p)
	}
	must(rows.Err())
	log.Printf("[%5.1fs] 待补 %d 件（不像种子标题、跳过 %d 件）", time.Since(start).Seconds(), len(prods), skipped)

	keyCat := map[string]string{}
	var keys []string
	for _, p := range prods {
		if _, ok := keyCat[p.key]; !ok {
			keyCat[p.key] = p.cat
			keys = append(keys, p.key)
		}
	}
	texts := make([]string, len(keys))
	for i, k := range keys {
		texts[i] = search.ProductText{Title: k, CategoryName: keyCat[k]}.EmbedContent()
	}
	cli, err := inference.New(inference.Config{Endpoint: *endpoint, BatchSize: *batch, Timeout: 120 * time.Second})
	must(err)
	res, err := cli.Embed(ctx, texts)
	must(err)
	log.Printf("[%5.1fs] 去重后 %d 种文本，向量算完（model=%s version=%s）",
		time.Since(start).Seconds(), len(keys), res.Model, res.ModelVersion)

	tx, err := db.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `CREATE TEMP TABLE lt_keyvec (key text PRIMARY KEY, content text, embedding vector(1024)) ON COMMIT DROP`)
	must(err)
	for i, k := range keys {
		_, err = tx.Exec(ctx, `INSERT INTO lt_keyvec VALUES ($1, $2, $3::vector)`, k, texts[i], vecLiteral(res.Vectors[i]))
		must(err)
	}
	_, err = tx.Exec(ctx, `CREATE TEMP TABLE lt_prod (product_id bigint, merchant_id bigint, key text, hashes jsonb) ON COMMIT DROP`)
	must(err)
	te, st := understanding.TextEmbedding{}, understanding.SearchText{}
	prodRows := make([][]any, len(prods))
	for i, p := range prods {
		in := understanding.ProductInput{ProductID: p.id, Title: p.title, Subtitle: p.sub, CategoryName: p.cat}
		h := fmt.Sprintf(`{%q:%q,%q:%q}`, te.Name(), te.Fingerprint(in), st.Name(), st.Fingerprint(in))
		prodRows[i] = []any{p.id, p.merchant, p.key, h}
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"lt_prod"}, []string{"product_id", "merchant_id", "key", "hashes"},
		pgx.CopyFromRows(prodRows))
	must(err)
	_, err = tx.Exec(ctx, `DROP INDEX IF EXISTS idx_ptv_hnsw`)
	must(err)
	tag, err := tx.Exec(ctx, `INSERT INTO product_text_vectors (product_id, merchant_id, content, embedding, model_name, model_version)
		SELECT lp.product_id, lp.merchant_id, kv.content, kv.embedding, $1, $2
		  FROM lt_prod lp JOIN lt_keyvec kv USING (key)`, res.Model, res.ModelVersion)
	must(err)
	log.Printf("[%5.1fs] 向量行 %d", time.Since(start).Seconds(), tag.RowsAffected())
	tag, err = tx.Exec(ctx, `INSERT INTO product_understanding (product_id, merchant_id, status, input_hashes, pipeline_version)
		SELECT product_id, merchant_id, 1, hashes, $1 FROM lt_prod
		ON CONFLICT (product_id) DO UPDATE SET input_hashes = product_understanding.input_hashes || EXCLUDED.input_hashes,
		       status = EXCLUDED.status, pipeline_version = EXCLUDED.pipeline_version`, search.PipelineVersion)
	must(err)
	log.Printf("[%5.1fs] product_understanding %d 行", time.Since(start).Seconds(), tag.RowsAffected())
	// 容器的 /dev/shm 只有 64MB，并行建索引会在共享内存段上 53100，所以串行建、内存给本地。
	_, err = tx.Exec(ctx, `SET LOCAL max_parallel_maintenance_workers = 0; SET LOCAL maintenance_work_mem = '1GB'`)
	must(err)
	// 与 db/migrations/00016_semantic_layer.sql 同一定义。
	_, err = tx.Exec(ctx, `CREATE INDEX idx_ptv_hnsw ON product_text_vectors
		USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)`)
	must(err)
	must(tx.Commit(ctx))
	_, err = db.Exec(ctx, `ANALYZE product_text_vectors; ANALYZE product_understanding`)
	must(err)
	log.Printf("[%5.1fs] HNSW 索引重建完成、ANALYZE 完成", time.Since(start).Seconds())
}

func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
