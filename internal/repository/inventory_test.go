package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// inventoryFixture 是两家商家各一个 SKU、各一行库存（水位 10）。
//
// storeA / storeB 是 00020 之后多出来的一维：库存按门店分，
// 扣减与回补都要指名是哪一家店，而「哪一家」不是夹具的实现细节 ——
// 拿别家的 store_id 去扣自家的 SKU 正是那两条复合外键要挡的形状，
// 所以门店 id 和 SKU id 一样要露在夹具外面。
type inventoryFixture struct {
	merchantA, merchantB int64
	skuA, skuB           int64
	storeA, storeB       int64
}

// seedInventories 用管理员连接（绕过 RLS）播夹具。
//
// 两家都要有库存行，且水位相同。只播一家的话，「扣别家的 SKU 失败」这个观察
// 完全没有区分力——失败也可能只是因为那一行根本不存在，而那正是这条测试要
// 排除的另一种解释。
func seedInventories(t *testing.T) inventoryFixture {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("invrepo-%d", time.Now().UnixNano())
	var f inventoryFixture
	// status = 2（停用）：夹具不是营业中的店。理由与 seedTwoTenants 同——
	// tenant.Preflight 断言的是整个库的形态，多一家活跃商家会让别的包随机红。
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2)
		 RETURNING id`, suffix+"-a", suffix+"-b").Scan(&f.merchantA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, suffix+"-b").Scan(&f.merchantB); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{f.merchantA, f.merchantB}
		// 顺序由外键定：inventories 指向 stores，stores 指向 regions，
		// 两者都指向 merchants。删漏一张表不会让**这条**测试红，
		// 会让下一轮的 `DELETE FROM merchants` 以 23503 失败 —— 那条错误
		// 出现在别的测试里，指不回这里。
		// inventories 现在自带 merchant_id（00020），不必再绕 skus 的子查询。
		for _, stmt := range []string{
			`DELETE FROM inventories WHERE merchant_id = ANY($1)`,
			`DELETE FROM skus       WHERE merchant_id = ANY($1)`,
			`DELETE FROM products   WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories WHERE merchant_id = ANY($1)`,
			`DELETE FROM stores     WHERE merchant_id = ANY($1)`,
			`DELETE FROM regions    WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, ids); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	for i, m := range []int64{f.merchantA, f.merchantB} {
		var catID, prodID, skuID, regionID, storeID int64
		// 每家一个大区 + 一家默认门店。00020 的回填只覆盖迁移那一刻已有的
		// 商家，这两家是之后插的，所以门店得自己建。
		// is_default = TRUE：默认店靠「全国兜底」接单，不画围栏是正常形态；
		// 这组测试一条都不碰地理围栏，画个假多边形只会让人以为它有意义。
		if err := admin.QueryRow(ctx,
			`INSERT INTO regions (merchant_id, code, name) VALUES ($1,'r','R')
			 RETURNING id`, m).Scan(&regionID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO stores (merchant_id, region_id, code, name, is_default)
			 VALUES ($1,$2,'s','S',TRUE) RETURNING id`, m, regionID).Scan(&storeID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO categories (merchant_id, name, path) VALUES ($1,'c','/c/')
			 RETURNING id`, m).Scan(&catID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO products (merchant_id, category_id, title, status, published_at)
			 VALUES ($1,$2,'p',1,now()) RETURNING id`, m, catID).Scan(&prodID); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents)
			 VALUES ($1,$2,$3,100) RETURNING id`,
			m, prodID, fmt.Sprintf("%s-%d", suffix, m)).Scan(&skuID); err != nil {
			t.Fatal(err)
		}
		// merchant_id 显式写：这是管理员连接，没有 app.merchant_id，
		// 列默认值 current_merchant() 会 RAISE 而不是填一个空。
		if _, err := admin.Exec(ctx,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
			 VALUES ($1, $2, $3, 10)`, skuID, storeID, m); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.skuA, f.storeA = skuID, storeID
		} else {
			f.skuB, f.storeB = skuID, storeID
		}
	}
	return f
}

// availableQty 绕过 RLS 读真实水位，用来证明「失败的那次真的什么都没写」。
func availableQty(t *testing.T, skuID int64) int32 {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var qty int32
	if err := admin.QueryRow(ctx,
		`SELECT available_qty FROM inventories WHERE sku_id = $1`, skuID).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	return qty
}

// 这是本任务的主断言：**两种 `UPDATE 0` 必须能被区分开**。
//
// 数据模型 §4 与 M2 计划第三条写着这件事，但在这条测试之前它只是一段文字。
// 文字挡不住的那句代码长这样：
//
//	tag, _ := tx.Exec(ctx, `UPDATE inventories SET ... WHERE sku_id = $1 AND available_qty >= $2`)
//	if tag.RowsAffected() == 0 { return ErrOutOfStock }   // ← 把越权当成缺货
//
// 它在单租户的开发库上永远正确，在多租户下把一次攻击写进了库存日志。
//
// 三条路径一次跑全，且互为对照：少了「扣成功」那一支，另外两支可能只是因为
// 整条语句根本没生效；少了「库存不足」那一支，ErrSKUNotInTenant 可能只是
// 「任何失败都报不可见」。
func TestDeductInventoryTellsStarvationFromCrossTenant(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))

	asA := tenant.NewContext(ctx, f.merchantA)

	t.Run("自家_库存够_扣成功并回传水位", func(t *testing.T) {
		var after int32
		err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			after, e = q.DeductInventory(ctx, f.skuA, f.storeA, 3)
			return e
		})
		if err != nil {
			t.Fatalf("扣自家库存失败: %v", err)
		}
		if after != 7 {
			t.Fatalf("扣减后水位 %d，期望 7", after)
		}
		if got := availableQty(t, f.skuA); got != 7 {
			t.Fatalf("库里的水位是 %d，期望 7 —— 返回值和真实状态对不上", got)
		}
	})

	t.Run("自家_库存不足_是业务分支", func(t *testing.T) {
		before := availableQty(t, f.skuA)
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.DeductInventory(ctx, f.skuA, f.storeA, before+1)
			return e
		})
		if !errors.Is(err, repository.ErrInsufficientStock) {
			t.Fatalf("期望 ErrInsufficientStock，实得 %v", err)
		}
		// 反向断言：它**不能**同时是 ErrSKUNotInTenant。两个 sentinel 要是
		// 被谁包成了同一个错误，上面那句 errors.Is 照样绿。
		if errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatal("库存不足被同时报成了「不可见」—— 两种成因又混回去了")
		}
		if got := availableQty(t, f.skuA); got != before {
			t.Fatalf("库存不足时水位从 %d 变成了 %d —— 条件原子更新没守住", before, got)
		}
	})

	t.Run("别家的SKU_是不可见_不是缺货", func(t *testing.T) {
		// 商家 B 的那一行水位是 10，足够扣 1。所以这次失败**只可能**是
		// 因为 RLS 把它挡在视野外 —— 这正是本测试区分力的来源：
		// 如果实现退化成「rows_affected = 0 ⇒ 缺货」，这里会拿到
		// ErrInsufficientStock，而库存明明是够的。
		if got := availableQty(t, f.skuB); got < 1 {
			t.Fatalf("夹具坏了：商家 B 的水位是 %d，这次失败就分不清是缺货还是不可见", got)
		}
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.DeductInventory(ctx, f.skuB, f.storeB, 1)
			return e
		})
		if !errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatalf("期望 ErrSKUNotInTenant，实得 %v", err)
		}
		if errors.Is(err, repository.ErrInsufficientStock) {
			t.Fatal("跨租户扣减被报成了「库存不足」—— SAGA 会把一次越权当成缺货去补偿")
		}
		if got := availableQty(t, f.skuB); got != 10 {
			t.Fatalf("商家 B 的水位变成了 %d —— 跨租户扣减真的写进去了", got)
		}
	})

	t.Run("补偿路径同样分得清", func(t *testing.T) {
		// 补偿最容易直接攥着 sku_id 回滚（数据模型 §4 点名说了）。
		// 回补别家的库存不是「补偿失败」，是往别人账上打钱。
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.RestoreInventory(ctx, f.skuB, f.storeB, 5)
			return e
		})
		if !errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatalf("期望 ErrSKUNotInTenant，实得 %v", err)
		}
		if got := availableQty(t, f.skuB); got != 10 {
			t.Fatalf("商家 B 的水位变成了 %d —— 跨租户回补真的写进去了", got)
		}

		var after int32
		err = r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			after, e = q.RestoreInventory(ctx, f.skuA, f.storeA, 3)
			return e
		})
		if err != nil {
			t.Fatalf("回补自家库存失败: %v", err)
		}
		if after != 10 {
			t.Fatalf("回补后水位 %d，期望 10", after)
		}
	})
}

// 没有租户上下文时，扣减必须以 42501 失败，而不是落进上面任何一支。
//
// 数据模型 §4 按实测改正过这一条：真实的 current_merchant() 在未设上下文时是
// RAISE EXCEPTION，不是返回 NULL，所以语句根本走不到「可见 0 行」。
// 把它和「库存不足」混为一谈的代价，是「所有下单都失败」会表现为
// 「所有商品都缺货」，排查方向从第一步就是错的。
func TestDeductInventoryWithoutTenantIsNeitherBranch(t *testing.T) {
	ctx := context.Background()
	f := seedInventories(t)
	r := repository.New(pool(t))

	// WithTenant 在 Go 这一侧就会拒绝没有租户的 ctx，所以这条走不到数据库；
	// 断言的是它**不**返回那两个 sentinel 中的任何一个。
	err := r.WithTenant(ctx, func(q repository.Tx) error {
		_, e := q.DeductInventory(ctx, f.skuA, f.storeA, 1)
		return e
	})
	if errors.Is(err, repository.ErrInsufficientStock) ||
		errors.Is(err, repository.ErrSKUNotInTenant) {
		t.Fatalf("漏设租户上下文被归进了库存的某一支: %v", err)
	}
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("期望 tenant.ErrNoTenant，实得 %v", err)
	}
}
