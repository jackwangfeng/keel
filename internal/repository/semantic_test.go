package repository_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// testModelName 是这一组测试往 product_text_vectors.model_name 里写的名字。
//
// 取 infero 那条腿的（今天的默认部署）。这一层不比对模型名 —— 比对它的是
// internal/inference 的 validate 与 service/index.go 的 decide；这里只要求
// **写进去的和读出来的是同一个字符串**，所以用哪条腿的名字都行，
// 用真的那个只是为了别在库里留一个不存在的模型名。
var testModelName = inference.MustDialect(inference.DialectInfero).ModelName

// 入库这一侧的归一化闸门（M3 Task 3 的硬约束一）。
//
// 语义检索层 §2.3 把这件事的性质写死了：配合 vector_cosine_ops，
// **写入未归一化的向量不会报错，只会悄悄拉低召回质量**。也就是说一个范数是
// 7.3 的向量：INSERT 成功、HNSW 索引照建、检索照常返回结果、
// 本仓库每一条形状断言照绿 —— 只有排序变得没有意义。
//
// internal/inference 的客户端已经核过一遍（它核的是引擎的输出）。这一条核的是
// **真的要写进那一列的东西**，两者之间隔着调用方。§10 明说引擎可以独立演进，
// 那一道将来还在不在、还严不严，不由这一层决定；而这一层下面就是 SQL，
// SQL 下面是一列不会抱怨的 vector(1024)。

func seedOneProduct(t *testing.T, admin *pgx.Conn, tag string) (int64, int64) {
	t.Helper()
	ctx := context.Background()

	suffix := fmt.Sprintf("%s-%d", tag, time.Now().UnixNano())
	var merchantID int64
	// status = 2（停用）：夹具不是营业中的店。理由同 seedTwoTenants 里那一段 ——
	// tenant.Preflight 断言的是整个库的形态，插一家活跃商家会在别的包里
	// 表现为一次随机的断言失败。
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'V',2) RETURNING id`,
		suffix).Scan(&merchantID); err != nil {
		t.Fatal(err)
	}
	var catID, prodID int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'女装','/女装/')
		 RETURNING id`, merchantID).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO products (merchant_id, category_id, title, status, published_at)
		 VALUES ($1,$2,'红色连衣裙',1,now()) RETURNING id`, merchantID, catID).Scan(&prodID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		for _, stmt := range []string{
			`DELETE FROM product_text_vectors  WHERE merchant_id = $1`,
			`DELETE FROM product_understanding WHERE merchant_id = $1`,
			`DELETE FROM products              WHERE merchant_id = $1`,
			`DELETE FROM categories            WHERE merchant_id = $1`,
			`DELETE FROM merchants             WHERE id          = $1`,
		} {
			if _, err := admin.Exec(c, stmt, merchantID); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})
	return merchantID, prodID
}

// unitVector 造一个真的归一化了的 1024 维向量（每一维相等）。
func unitVector() []float32 {
	v := make([]float32, inference.Dim)
	x := float32(1 / math.Sqrt(float64(inference.Dim)))
	for i := range v {
		v[i] = x
	}
	return v
}

// scale 把一个向量整体放大 k 倍 —— 范数跟着变成 k。
func scale(v []float32, k float32) []float32 {
	out := make([]float32, len(v))
	for i := range v {
		out[i] = v[i] * k
	}
	return out
}

func TestUpsertTextVectorRejectsUnnormalizedVector(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	merchantID, prodID := seedOneProduct(t, admin, "normgate")
	r := repository.New(pool(t))
	tctx := tenant.NewContext(ctx, merchantID)

	rowsFor := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(context.Background(),
			`SELECT count(*) FROM product_text_vectors WHERE product_id = $1`,
			prodID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("范数不是 1 的向量写不进去", func(t *testing.T) {
		// 7.3 倍，不是「差一点点」。真的没归一化时范数就是这个量级，
		// 不存在「把容差调松一点就过了」的中间地带。
		err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID: prodID, Content: "红色连衣裙",
				Embedding: scale(unitVector(), 7.3),
				ModelName: testModelName, ModelVersion: "test",
			})
		})
		if !errors.Is(err, repository.ErrVectorNotNormalized) {
			t.Fatalf("期望 ErrVectorNotNormalized，实得 %v —— "+
				"未归一化的向量配 vector_cosine_ops 会让距离失真，"+
				"而它入库之后不报错、不变慢、检索照常返回结果（语义检索层 §2.3）", err)
		}
		if n := rowsFor(); n != 0 {
			t.Fatalf("被拒之后库里还是有 %d 行 —— 闸门只是报了个错，东西照样写进去了", n)
		}
	})

	t.Run("阳性对照：归一化了的同一个向量写得进去", func(t *testing.T) {
		// 没有这一条，上面那条断言在「这条路径整个坏了」（FK 不满足、
		// 租户上下文不对、列名写错）时同样是红的 —— 而那三种红与
		// 「归一化闸门在工作」是两回事。
		if err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID: prodID, Content: "红色连衣裙",
				Embedding: unitVector(),
				ModelName: testModelName, ModelVersion: "v-test",
			})
		}); err != nil {
			t.Fatalf("归一化了的向量也写不进去：%v —— "+
				"那上面那条红的原因不是归一化闸门", err)
		}
		if n := rowsFor(); n != 1 {
			t.Fatalf("库里有 %d 行，期望 1 行", n)
		}

		// model_name / model_version 真的落库了（硬约束二）。它们是
		// 「这批向量是哪个模型算的、要不要重算」的唯一记录。
		var name, version, content string
		var norm float64
		if err := admin.QueryRow(context.Background(), `
			SELECT model_name, model_version, content,
			       sqrt((SELECT sum(x*x) FROM unnest(embedding::real[]) AS x))
			  FROM product_text_vectors WHERE product_id = $1`, prodID).
			Scan(&name, &version, &content, &norm); err != nil {
			t.Fatal(err)
		}
		if name != testModelName || version != "v-test" {
			t.Errorf("落库的是 %s@%s，期望 %s@v-test", name, version, testModelName)
		}
		if content != "红色连衣裙" {
			t.Errorf("content 落库是 %q", content)
		}
		// 从**库里读回来**再算一次范数：闸门查的是 Go 侧那个切片，
		// 而真正要紧的是列里那个值。中间隔着一次 float32 格式化与一次解析。
		if math.Abs(norm-1) > 1e-3 {
			t.Errorf("从库里读回来的向量范数是 %.6f，不是 1 —— "+
				"闸门放行的值在格式化成字面量的路上变了", norm)
		}
	})

	t.Run("维度不对同样拦得住", func(t *testing.T) {
		short := unitVector()[:16]
		err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID: prodID, Content: "x", Embedding: short,
				ModelName: testModelName, ModelVersion: "test",
			})
		})
		if !errors.Is(err, repository.ErrVectorWrongDim) {
			t.Fatalf("期望 ErrVectorWrongDim，实得 %v", err)
		}
	})

	t.Run("NaN 拦得住", func(t *testing.T) {
		bad := unitVector()
		bad[3] = float32(math.NaN())
		err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID: prodID, Content: "x", Embedding: bad,
				ModelName: testModelName, ModelVersion: "test",
			})
		})
		if err == nil {
			t.Fatal("带 NaN 的向量写进去了 —— 它与任何查询向量的距离都是 NaN，" +
				"排序里被当成最大值，这件商品从检索结果里单向静默失踪")
		}
	})

	t.Run("model_name 空着拒绝入库", func(t *testing.T) {
		err := r.WithTenant(tctx, func(tx repository.Tx) error {
			return tx.UpsertProductTextVector(ctx, repository.TextVector{
				ProductID: prodID, Content: "x", Embedding: unitVector(),
				ModelName: "", ModelVersion: "test",
			})
		})
		if err == nil {
			t.Fatal("model_name 空着也写进去了 —— 空字符串能写进 NOT NULL 列，" +
				"而它让「这批向量要不要重算」永远答不上来")
		}
	})
}
