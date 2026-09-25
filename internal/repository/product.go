package repository

import (
	"context"
	"fmt"
	"math"

	"github.com/keel/keel/internal/repository/internal/db"
)

// Product 是商品在 repository 边界上的形状 —— 领域类型，不是 sqlc 的产物。
//
// 为什么不直接把 db.ListProductsRow 用类型别名导出去（那样少写一个结构体和
// 一段拷贝）：别名会把生成代码的形状焊进 service 的函数签名。之后每一次改
// db/queries/*.sql —— 多 SELECT 一列、把一列改名 —— 都会直接改掉 service 与
// handler 能看到的类型，而 sqlc 的重生成是一条不经人眼的自动化路径。
// 这里多写的这几行，换的是「查询怎么写」与「业务看到什么」之间的一道缝。
//
// 字段类型也在这里收口：sqlc 按 PostgreSQL 的 int4/int8 出 int32/int64，
// 那是列的宽度，不是业务的语义。
type Product struct {
	ID            int64
	Title         string
	Subtitle      *string
	MinPriceCents int64
	MaxPriceCents int64
	TotalStock    int32
	SalesCount    int32
	Status        int16
}

// Tx 是一次租户事务里能做的全部事情，也是 service 能拿到的全部。
//
// 这个接口存在的理由是 *db.Queries 上有一个 WithTx(pgx.Tx) 方法。它是生成代码
// 的导出方法，所以只要 service 手里握着 *db.Queries，它就能自己 Begin 一个事务
// 再 WithTx 过去 —— 那条路上没有 set_config('app.merchant_id')，查询会直接撞上
// current_merchant() 抛的 42501，症状是线上偶发的 insufficient_privilege，
// 而不是任何指向「少设了租户」的信息。
//
// 交出接口而不是具体类型，那个方法在 service 那一侧就不存在了：不是「不该调」，
// 是编译器不认得。fn 收到的 Tx 背后永远是本包在 WithTenant 里建出来的实现，
// 而 WithTenant 是唯一能建出它的地方。
//
// 新查询加进来时，这个接口跟着长 —— 长的过程本身就是一次复核：
// 这个能力确实要交给业务层吗？
//
// 它由若干个按主题拆开的接口嵌套而成，而不是一张平铺的方法表。理由是并发：
// 同一时期有好几条任务在往这一层加能力，平铺意味着他们改的是同一处声明的
// 同一批行。拆开之后每条任务加自己那个文件里的方法，这里只多一行嵌入。
type Tx interface {
	ProductTx
	SagaTx
	UserTx
}

// ProductTx 是商品读取这一面。
type ProductTx interface {
	// ListProducts 返回当前租户的在架商品，按上架时间倒序。
	//
	// limit / offset 的钳制是业务规则，在 service 里做。这里只负责把它们安全地
	// 送进 int32 的参数位 —— 越界的值到这一层还是要挡，因为 int32 溢出的后果是
	// 一个负数 OFFSET，Postgres 会报错，而错误里没有任何东西指向「页码太大」。
	ListProducts(ctx context.Context, limit, offset int64) ([]Product, error)

	// CountProducts 返回当前租户在架商品的总数，用于填契约里必填的 total。
	CountProducts(ctx context.Context) (int64, error)

	// 库存的两个方法搬去了 SagaTx（saga.go）：它们本来就是为 SAGA 分支存在的
	// —— 正向扣减、补偿回补、超时关单释放。它们的三条出路是这一层唯一一处
	// 「用返回值的形状去挡一类误用」的设计，注释仍在 inventory.go。
}

// tenantTx 是 Tx 的唯一实现：一层薄薄的转换，把 sqlc 的行变成领域类型。
type tenantTx struct{ q *db.Queries }

func (t tenantTx) ListProducts(ctx context.Context, limit, offset int64) ([]Product, error) {
	// 到这里还越界只可能是上游的钳制没生效。报错而不是截断：截断会把
	// 「第 1 亿页」悄悄变成某一页真实数据，一个错误的结果比一个错误更难发现。
	if limit < 0 || limit > math.MaxInt32 {
		return nil, fmt.Errorf("limit %d 超出范围 [0, %d]", limit, math.MaxInt32)
	}
	if offset < 0 || offset > math.MaxInt32 {
		return nil, fmt.Errorf("offset %d 超出范围 [0, %d]", offset, math.MaxInt32)
	}

	rows, err := t.q.ListProducts(ctx, db.ListProductsParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Product, 0, len(rows))
	for _, r := range rows {
		out = append(out, Product{
			ID:            r.ID,
			Title:         r.Title,
			Subtitle:      r.Subtitle,
			MinPriceCents: r.MinPriceCents,
			MaxPriceCents: r.MaxPriceCents,
			TotalStock:    r.TotalStock,
			SalesCount:    r.SalesCount,
			Status:        r.Status,
		})
	}
	return out, nil
}

func (t tenantTx) CountProducts(ctx context.Context) (int64, error) {
	return t.q.CountProducts(ctx)
}
