//go:build keel_real_engine

// keel-integration.md 的判据一：**归一化要从存储里读回来验。**
//
//	「Read the vector back out of PostgreSQL and recompute the L2 norm there
//	 — not just assert on the slice the client received.」
//
// 为什么这一条不能放在 internal/inference 里：那个包断言的是「引擎交给 Go 的
// 那个切片」。从那个切片到 `product_text_vectors.embedding` 这一列之间还隔着
// 三段代码 —— repository.vectorLiteral 把 []float32 格式化成 `[a,b,…]` 文本、
// PostgreSQL 把文本解析成 vector(1024)、pgvector 把它按 float4 存下来。
// 这三段里任何一段做了截断、丢精度、或者把科学计数法写成了别的东西，
// **都不会报错**，只会让存进去的东西和算出来的东西不是一回事。
// 语义检索层 §2.3：未归一化的向量配 vector_cosine_ops 不报错，只悄悄拉低召回。
//
// 而它也不能反过来放在 internal/inference 里跑 ——
// internal/repository 依赖 internal/inference（要 inference.Dim），
// 反向 import 是一个环。所以判据一落在这里，由 make test-engine 一起拉起来。
//
// 跑法（要真引擎 + 真数据库两样都在）：
//
//	KEEL_EMBED_ENDPOINT=http://127.0.0.1:18081 PGHOST=… PGPORT=… make test-engine

package repository_test

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 真引擎算出来的向量，写进 PostgreSQL 再读回来，L2 范数还得是 1。
//
// 关键在「读回来」三个字：范数是**在数据库里、对着那一列真正存着的 float4**
// 重算的（`sqrt(sum(x*x))` over `unnest(embedding::real[])`），不是对 Go 侧
// 那个切片重算的。后者 internal/inference 已经算过一遍了，再算一遍只是同一个
// 断言的复读。
//
// 阳性对照在同一个包里：TestUpsertTextVectorRejectsUnnormalizedVector 证明
// 这条链路**会**因为范数不对而拒绝。没有它，这条测试在「写入被悄悄丢弃、
// 读回来的是上一次的行」时也可能是绿的。
func TestRealEngineVectorStaysNormalizedThroughPostgres(t *testing.T) {
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

	ctx := context.Background()
	res, err := c.Embed(ctx, inference.SemanticProbeTexts)
	if err != nil {
		t.Fatalf("真引擎调用失败: %v", err)
	}

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatalf("连不上数据库（判据一要真的写进 PostgreSQL 再读回来）: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	merchantID, prodID := seedOneProduct(t, admin, "inferonorm")
	r := repository.New(pool(t))
	tctx := tenant.NewContext(ctx, merchantID)

	// 三条探针都写一遍，每次覆盖同一行 —— 三条文本长短不同，
	// 只写一条的话「只有短文本能正确落库」这种事看不出来。
	for i, text := range inference.SemanticProbeTexts {
		vec := res.Vectors[i]

		if err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID:    prodID,
				Content:      text,
				Embedding:    vec,
				ModelName:    res.Model,
				ModelVersion: res.ModelVersion,
			})
		}); err != nil {
			t.Fatalf("第 %d 条写不进去: %v", i, err)
		}

		// 范数在**数据库里**算，对着那一列真正存着的 float4。
		var dbNorm float64
		var dims int
		var gotModel, gotVersion string
		if err := admin.QueryRow(ctx,
			`SELECT sqrt((SELECT sum(x::float8 * x::float8)
			               FROM unnest(embedding::real[]) AS x)),
			        array_length(embedding::real[], 1),
			        model_name, model_version
			   FROM product_text_vectors WHERE product_id = $1`,
			prodID).Scan(&dbNorm, &dims, &gotModel, &gotVersion); err != nil {
			t.Fatalf("第 %d 条读不回来: %v", i, err)
		}

		if dims != inference.Dim {
			t.Fatalf("第 %d 条从库里读回来是 %d 维，而列类型是 vector(%d)",
				i, dims, inference.Dim)
		}
		if math.Abs(dbNorm-1) > inference.NormTolerance {
			t.Fatalf("第 %d 条**从 PostgreSQL 读回来重算**的 L2 范数是 %.9f，不是 1（容差 %g）。"+
				"语义检索层 §2.3：未归一化的向量配 vector_cosine_ops 会让距离失真，"+
				"而它入库之后不会报错、只会悄悄拉低召回质量",
				i, dbNorm, inference.NormTolerance)
		}
		// 落库的 model_name / model_version 必须就是引擎自己报的那两个值。
		// 它们回答「这批向量要不要重算」，从配置里抄一份的话，
		// 引擎换了模型而配置没跟，重算判定从此失灵。
		if gotModel != res.Model || gotVersion != res.ModelVersion {
			t.Fatalf("第 %d 条落库记的是 %s@%s，引擎报的是 %s@%s",
				i, gotModel, gotVersion, res.Model, res.ModelVersion)
		}
		t.Logf("第 %d 条 %-28s 库内重算 L2 范数 = %.9f  (%s@%s)",
			i, inference.SemanticProbeTexts[i], dbNorm, gotModel, gotVersion)
	}
}
