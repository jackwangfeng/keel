package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 商家写路径（M4 Task 2）的夹具：**两家商家，形状对称**。
//
// 对称是这一组测试全部区分力的来源。只播一家的话，「拿别家的 id 去写失败了」
// 这个观察没有任何区分力 —— 失败也可能只是因为那一行根本不存在，
// 而那正是这些测试要排除的另一种解释。
type catalogFixture struct {
	merchantA, merchantB int64
	catA, catB           int64
	prodA, prodB         int64 // 都是 status = 1 在架
	skuA, skuB           int64 // 都有库存行，水位都是 10
	storeA, storeB       int64 // 各一家默认门店，库存行挂在它上面
	regionA, regionB     int64 // 门店所属大区；买家侧读路径的 StoreScope 要它
	staffA, staffB       int64
	upA1, upA2, upA3     int64 // A 的三张商品图（purpose = 1）
	upAEvidence          int64 // A 的一张退款凭证（purpose = 3）
	upB1                 int64 // B 的一张商品图
	suffix               string
}

func seedCatalog(t *testing.T) catalogFixture {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	f := catalogFixture{suffix: fmt.Sprintf("m4cat-%d", time.Now().UnixNano())}

	// status = 2（停用）：夹具不是营业中的店。理由与 seedInventories 同 ——
	// tenant.Preflight 断言的是整个库的形态，多一家活跃商家会让别的包随机红。
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1,'A',2), ($2,'B',2)
		 RETURNING id`, f.suffix+"-a", f.suffix+"-b").Scan(&f.merchantA); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`SELECT id FROM merchants WHERE code = $1`, f.suffix+"-b").Scan(&f.merchantB); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		c := context.Background()
		ids := []int64{f.merchantA, f.merchantB}
		// stores 排在 inventories 之后、merchants 之前：inventories 指向
		// stores，stores 指向 regions。漏掉哪一张都不会让**这条**测试红，
		// 而是让下一轮的 `DELETE FROM merchants` 以 23503 失败 —— 那条错误
		// 出现在别的测试里，指不回这里。
		// inventories 自带 merchant_id 了（00020），不必再绕 skus 的子查询。
		for _, stmt := range []string{
			`DELETE FROM product_images WHERE merchant_id = ANY($1)`,
			`DELETE FROM uploads        WHERE merchant_id = ANY($1)`,
			`DELETE FROM inventories WHERE merchant_id = ANY($1)`,
			`DELETE FROM skus       WHERE merchant_id = ANY($1)`,
			`DELETE FROM products   WHERE merchant_id = ANY($1)`,
			`DELETE FROM categories WHERE merchant_id = ANY($1)`,
			`DELETE FROM stores     WHERE merchant_id = ANY($1)`,
			`DELETE FROM regions    WHERE merchant_id = ANY($1)`,
			`DELETE FROM staff      WHERE merchant_id = ANY($1)`,
			`DELETE FROM merchants  WHERE id          = ANY($1)`,
		} {
			if _, err := admin.Exec(c, stmt, ids); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	for i, m := range []int64{f.merchantA, f.merchantB} {
		var catID, prodID, skuID, staffID, regionID, storeID int64
		// 每家一个大区 + 一家默认门店。00020 的回填只覆盖迁移那一刻库里已有
		// 的商家，这两家是之后插的。这不是样板代码：CreateSKU 建库存行那一步
		// （CreateInventoryRow）挑的就是**默认门店**，没有它建出来的 SKU
		// 一行库存都没有，而那正是 TestCreateSKUAlwaysCreatesInventoryRow
		// 要证伪的状态。
		// is_default = TRUE：默认店靠「全国兜底」接单，不画围栏是正常形态；
		// 这一组测试一条都不碰地理围栏。
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
			m, prodID, fmt.Sprintf("%s-seed-%d", f.suffix, m)).Scan(&skuID); err != nil {
			t.Fatal(err)
		}
		// merchant_id 显式写：管理员连接上没有 app.merchant_id，
		// 列默认值 current_merchant() 会 RAISE 而不是填空。
		if _, err := admin.Exec(ctx,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
			 VALUES ($1, $2, $3, 10)`, skuID, storeID, m); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx,
			`INSERT INTO staff (merchant_id, email, name) VALUES ($1,$2,'ops')
			 RETURNING id`, m, fmt.Sprintf("%s-%d@example.test", f.suffix, m)).Scan(&staffID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.catA, f.prodA, f.skuA, f.staffA = catID, prodID, skuID, staffID
			f.storeA, f.regionA = storeID, regionID
		} else {
			f.catB, f.prodB, f.skuB, f.staffB = catID, prodID, skuID, staffID
			f.storeB, f.regionB = storeID, regionID
		}
	}

	newUpload := func(m, staffID int64, purpose int16, tag string) int64 {
		var id int64
		if err := admin.QueryRow(ctx,
			`INSERT INTO uploads (merchant_id, staff_id, purpose, driver, storage_key,
			                      content_type, size_bytes, sha256)
			 VALUES ($1,$2,$3,1,$4,'image/png',123,'deadbeef') RETURNING id`,
			m, staffID, purpose, fmt.Sprintf("%s/%s", f.suffix, tag)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.upA1 = newUpload(f.merchantA, f.staffA, 1, "a1")
	f.upA2 = newUpload(f.merchantA, f.staffA, 1, "a2")
	f.upA3 = newUpload(f.merchantA, f.staffA, 1, "a3")
	f.upAEvidence = newUpload(f.merchantA, f.staffA, 3, "a-evidence")
	f.upB1 = newUpload(f.merchantB, f.staffB, 1, "b1")
	return f
}

// scopeA / scopeB 是买家侧读路径要的那个门店作用域（00020）。
//
// 写成方法而不是让每个调用点自己拼 StoreScope{...}：门店与大区必须是
// **同一家店的那一对**，拼错了（A 的店配 B 的大区）查询不会报错，
// 只会安静地按另一个大区取价。一处拼装，拼错了到处都红。
func (f catalogFixture) scopeA() repository.StoreScope {
	return repository.StoreScope{StoreID: f.storeA, RegionID: f.regionA}
}

func (f catalogFixture) scopeB() repository.StoreScope {
	return repository.StoreScope{StoreID: f.storeB, RegionID: f.regionB}
}

// adminQuery 绕过 RLS 读真实状态，用来证明「失败的那次真的什么都没写」，
// 以及「成功的那次真的写进去了」。
func adminQuery(t *testing.T, sql string, args ...any) pgx.Row {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	return admin.QueryRow(ctx, sql, args...)
}

func realQty(t *testing.T, skuID int64) int32 {
	t.Helper()
	var q int32
	if err := adminQuery(t, `SELECT available_qty FROM inventories WHERE sku_id = $1`,
		skuID).Scan(&q); err != nil {
		t.Fatal(err)
	}
	return q
}

// =============================================================================
// 一、库存 CAS：两种 rows_affected = 0 必须分得开
// =============================================================================
//
// 这是本任务最要紧的一条断言。契约把两者定成不同的响应码，而它们在 SQL 层的
// 信号**完全一样**：一次 UPDATE 影响 0 行。
//
//	CAS 对不上          → 409，调用方刷新那一格重试**会**成功
//	SKU 不在本租户      → 404，重试**永远**不会成功
//
// 混掉的代价写在契约上：把「不是你的 SKU」报成 409，调用方会对着一个永远不会
// 成功的请求无限重试。
//
// 四条路径一次跑全，且互为对照：少了「写成功」那一支，另外几支可能只是因为
// 整条语句根本没生效；少了「CAS 不匹配」那一支，ErrSKUNotInTenant 可能只是
// 「任何失败都报不可见」。
func TestSetInventoryTellsPreconditionFromCrossTenant(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	t.Run("自家_CAS匹配_写成功", func(t *testing.T) {
		var inv repository.Inventory
		err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			inv, e = q.SetInventory(ctx, f.storeA, f.skuA, 10, 25, nil)
			return e
		})
		if err != nil {
			t.Fatalf("改自家库存失败: %v", err)
		}
		if inv.AvailableQty != 25 {
			t.Fatalf("返回的水位是 %d，期望 25", inv.AvailableQty)
		}
		if got := realQty(t, f.skuA); got != 25 {
			t.Fatalf("库里的水位是 %d，期望 25 —— 返回值和真实状态对不上", got)
		}
	})

	t.Run("自家_CAS不匹配_是409且带当前真实值", func(t *testing.T) {
		before := realQty(t, f.skuA) // 25
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetInventory(ctx, f.storeA, f.skuA, before+1, 999, nil)
			return e
		})
		if !errors.Is(err, repository.ErrInventoryPrecondition) {
			t.Fatalf("期望 ErrInventoryPrecondition，实得 %v", err)
		}
		// 反向断言：它**不能**同时是「不在本租户」。两个 sentinel 要是被谁包成
		// 了同一个错误，上面那句 errors.Is 照样绿。
		if errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatal("CAS 不匹配被同时报成了「不可见」—— 两种成因又混回去了")
		}
		var conflict *repository.InventoryConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("拿不到 *InventoryConflict，服务层就填不出契约要求的 current: %v", err)
		}
		if conflict.Current.AvailableQty != before {
			t.Fatalf("回传的当前值是 %d，真实值是 %d —— 后台页面据此刷新会刷出一个错的数",
				conflict.Current.AvailableQty, before)
		}
		if got := realQty(t, f.skuA); got != before {
			t.Fatalf("CAS 失败时水位从 %d 变成了 %d —— 条件原子更新没守住", before, got)
		}
	})

	t.Run("别家的SKU_是404_不是409", func(t *testing.T) {
		// **这里是整条测试的要害。** 传进去的 expected 是商家 B 那一行的
		// **真实水位**，所以这次失败**只可能**是因为 RLS 把它挡在视野外 ——
		// 如果实现退化成「rows_affected = 0 ⇒ CAS 不匹配」，这里会拿到
		// ErrInventoryPrecondition，而 expected 明明是对的。
		//
		// store_id 也得是**商家 B 那一家**（00020 之后它进了主键）。
		// 传 f.storeA 的话，(skuB, storeA) 这一行本来就不存在，这次失败
		// 会退化成「查无此行」，而 RLS 拦没拦住就再也看不出来了。
		realB := realQty(t, f.skuB)
		if realB != 10 {
			t.Fatalf("夹具坏了：商家 B 的水位是 %d，这次失败就分不清是 CAS 还是不可见", realB)
		}
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetInventory(ctx, f.storeB, f.skuB, realB, 999, nil)
			return e
		})
		if !errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatalf("期望 ErrSKUNotInTenant，实得 %v", err)
		}
		if errors.Is(err, repository.ErrInventoryPrecondition) {
			t.Fatal("跨租户改库存被报成了 CAS 失败 —— 调用方会对一个永远不会成功的请求无限重试")
		}
		if got := realQty(t, f.skuB); got != realB {
			t.Fatalf("商家 B 的水位变成了 %d —— 跨租户写真的写进去了", got)
		}
	})

	t.Run("软删掉的SKU_是404", func(t *testing.T) {
		// skus.deleted_at 是 00018 新加的，而 inventories 的 RLS 谓词看的是
		// skus.merchant_id，**不看 deleted_at**。所以这一条靠的是查询里那句
		// 显式的 sk.deleted_at IS NULL；去掉它，后台就能给一个「已经不存在」
		// 的规格改库存。
		var deadSKU int64
		if err := adminQuery(t,
			`INSERT INTO skus (merchant_id, product_id, sku_code, price_cents, deleted_at)
			 VALUES ($1,$2,$3,100,now()) RETURNING id`,
			f.merchantA, f.prodA, f.suffix+"-dead").Scan(&deadSKU); err != nil {
			t.Fatal(err)
		}
		// 挂在 A 的默认门店上：这一条要测的是 sk.deleted_at IS NULL 那道闸门，
		// 所以除了「已软删」之外的每一维都得是合法的 —— 门店挂错会让语句
		// 先撞上复合外键，断言就变成在测外键。
		if _, err := adminExec(t,
			`INSERT INTO inventories (sku_id, store_id, merchant_id, available_qty)
			 VALUES ($1, $2, $3, 7)`, deadSKU, f.storeA, f.merchantA); err != nil {
			t.Fatal(err)
		}
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetInventory(ctx, f.storeA, deadSKU, 7, 99, nil)
			return e
		})
		if !errors.Is(err, repository.ErrSKUNotInTenant) {
			t.Fatalf("期望 ErrSKUNotInTenant，实得 %v", err)
		}
		if got := realQty(t, deadSKU); got != 7 {
			t.Fatalf("软删 SKU 的水位被改成了 %d", got)
		}
	})
}

func adminExec(t *testing.T, sql string, args ...any) (pgconn.CommandTag, error) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	return admin.Exec(ctx, sql, args...)
}

// =============================================================================
// 二、软删之后货号能复用
// =============================================================================
//
// 这条测试守的是 00018 里那个「DROP INDEX 再 CREATE 成部分唯一索引」的动作。
// 不做那一步，一个被删掉的规格会永久占住一个货号，而货号是商家自己的编码体系。
//
// **阳性对照不可省**：先证明唯一索引确实在守（同一个货号建两次会被拒），
// 否则「软删后能复用」这个观察可以由「唯一索引根本没生效」来解释，
// 而那是一个严重得多的问题，且它会让这条测试永远是绿的。
func TestSKUCodeIsReusableAfterSoftDelete(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	code := f.suffix + "-REUSE"
	newSKU := repository.NewSKU{
		ProductID: f.prodA, SKUCode: code, PriceCents: 1000, Status: 1, AvailableQty: 3,
	}

	var first repository.AdminSKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		first, e = q.CreateSKU(ctx, newSKU)
		return e
	}); err != nil {
		t.Fatalf("建第一个 SKU 失败: %v", err)
	}

	t.Run("阳性对照_同货号再建一次会被唯一索引拒绝", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.CreateSKU(ctx, newSKU)
			return e
		})
		if !errors.Is(err, repository.ErrSKUCodeDuplicated) {
			t.Fatalf("期望 ErrSKUCodeDuplicated，实得 %v —— "+
				"唯一索引没在守的话，下面那条「软删后能复用」证明不了任何事", err)
		}
	})

	t.Run("软删之后同一个货号能再用", func(t *testing.T) {
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SoftDeleteSKU(ctx, first.ID)
			return e
		}); err != nil {
			t.Fatalf("软删失败: %v", err)
		}

		// 软删是**置 deleted_at，不删行** —— 硬删在这个 schema 下做不到
		// （order_items / cart_items / inventories / inventory_logs 四张表
		// 都对 skus 有外键）。所以那一行必须还在。
		var stillThere bool
		if err := adminQuery(t,
			`SELECT deleted_at IS NOT NULL FROM skus WHERE id = $1`, first.ID).
			Scan(&stillThere); err != nil {
			t.Fatalf("软删把行删掉了？%v", err)
		}
		if !stillThere {
			t.Fatal("SoftDeleteSKU 没有置 deleted_at")
		}

		var second repository.AdminSKU
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			second, e = q.CreateSKU(ctx, newSKU)
			return e
		}); err != nil {
			t.Fatalf("软删之后货号仍然被占着: %v —— uk_skus_code 还是全局形状", err)
		}
		if second.ID == first.ID {
			t.Fatal("第二次建出来的是同一行")
		}
	})

	t.Run("同租户内货号仍然唯一_跨租户可以重名", func(t *testing.T) {
		// 规矩三：uk_skus_code 的首列是 merchant_id，两家店各有一个同名货号
		// 是正常的。少了这一条，「部分唯一索引」可能被写成全局的部分唯一索引，
		// 而那会让后建的那家店建不了商品。
		asB := tenant.NewContext(ctx, f.merchantB)
		err := r.WithTenant(asB, func(q repository.Tx) error {
			_, e := q.CreateSKU(ctx, repository.NewSKU{
				ProductID: f.prodB, SKUCode: code, PriceCents: 500, Status: 1,
			})
			return e
		})
		if err != nil {
			t.Fatalf("商家 B 用同一个货号失败了: %v —— 唯一索引没收进租户内", err)
		}
	})
}

// =============================================================================
// 三、建 SKU 必须在同一个事务里建出库存行
// =============================================================================
//
// 没有那一行时，下单 SAGA 的 rows_affected = 0 会被判成缺货，症状是
// 「这件商品永远缺货」，而排查方向从一开始就是错的。
func TestCreateSKUAlwaysCreatesInventoryRow(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	var sku repository.AdminSKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		sku, e = q.CreateSKU(ctx, repository.NewSKU{
			ProductID: f.prodA, SKUCode: f.suffix + "-INV", PriceCents: 700,
			Status: 1, AvailableQty: 12, WarningQty: 2,
		})
		return e
	}); err != nil {
		t.Fatalf("建 SKU 失败: %v", err)
	}

	var qty, warn int32
	if err := adminQuery(t,
		`SELECT available_qty, warning_qty FROM inventories WHERE sku_id = $1`, sku.ID).
		Scan(&qty, &warn); err != nil {
		t.Fatalf("新建的 SKU 没有库存行: %v —— 这件商品会表现为「永远缺货」", err)
	}
	if qty != 12 || warn != 2 {
		t.Fatalf("库存行是 (%d, %d)，期望 (12, 2)", qty, warn)
	}
	if sku.AvailableQty != 12 {
		t.Fatalf("返回的水位是 %d，期望 12", sku.AvailableQty)
	}

	// 扣库存之前先记一次商品的总库存，下面要拿它做差。
	before, err := findProduct(t, r, asA, f.prodA)
	if err != nil {
		t.Fatal(err)
	}

	// 真正要守的不是「有一行」，是「SAGA 扣得动」。直接用下单那条语句验一次：
	// 漏建库存行时它返回的是 ErrInsufficientStock，而那正是最误导人的症状。
	var after int32
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		after, e = q.DeductInventory(ctx, sku.ID, f.storeA, 5)
		return e
	}); err != nil {
		t.Fatalf("新建 SKU 扣不动库存: %v —— SAGA 会把它判成缺货", err)
	}
	if after != 7 {
		t.Fatalf("扣减后水位 %d，期望 7", after)
	}

	// 价格区间与总库存要跟着 SKU 一起动。
	//
	// 00019 之前这句话的意思是「同步器跑过了」；现在它们是读的时候从
	// skus / inventories 现算的，所以这条断言检查的是那个 LEFT JOIN LATERAL
	// 真的接到了这件商品的 SKU 上 —— 把 LATERAL 的 WHERE 改成一个恒假条件，
	// 两个数都会回到 0，这里就红。
	p, err := findProduct(t, r, asA, f.prodA)
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalStock == 0 || p.MinPriceCents == 0 {
		t.Fatalf("建完 SKU 之后商品的价格区间与总库存还是 (min=%d, stock=%d) —— 现算没接上",
			p.MinPriceCents, p.TotalStock)
	}
	// 现算比同步器多守住一件事：这个数是**当下**的水位，不是某一次写入时的快照。
	// 上面那次 DeductInventory 扣掉了 5 件，而它不经过任何「重算冗余字段」的
	// 代码路径 —— 同步器版本在这里会原样回 before.TotalStock。
	if got := before.TotalStock - p.TotalStock; got != 5 {
		t.Fatalf("扣掉 5 件之后商品的总库存从 %d 变成 %d（差 %d），期望差 5 —— "+
			"这个数不是现算的，它停在某一次写入的快照上",
			before.TotalStock, p.TotalStock, got)
	}
}

func findProduct(t *testing.T, r *repository.Repo, ctx context.Context, id int64) (repository.AdminProduct, error) {
	t.Helper()
	var p repository.AdminProduct
	err := r.WithTenant(ctx, func(q repository.Tx) error {
		var e error
		p, e = q.AdminFindProduct(context.Background(), id)
		return e
	})
	return p, err
}

// =============================================================================
// 四、图片是整组替换，顺序由 sort_order 表达
// =============================================================================
func TestReplaceProductImagesIsWholeGroupAndOrdered(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	replace := func(ids ...int64) ([]repository.ProductImage, error) {
		var out []repository.ProductImage
		err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			out, e = q.ReplaceProductImages(ctx, f.prodA, ids)
			return e
		})
		return out, err
	}

	t.Run("顺序即数组下标_第0张是主图", func(t *testing.T) {
		got, err := replace(f.upA3, f.upA1, f.upA2)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{f.upA3, f.upA1, f.upA2}
		if len(got) != 3 {
			t.Fatalf("回传 %d 张，期望 3", len(got))
		}
		for i, img := range got {
			if img.UploadID != want[i] {
				t.Fatalf("第 %d 张是 upload %d，期望 %d —— 顺序没有按数组下标落进 sort_order",
					i, img.UploadID, want[i])
			}
			if img.SortOrder != int32(i) {
				t.Fatalf("第 %d 张的 sort_order 是 %d —— 主图（最小的那张）会认错人",
					i, img.SortOrder)
			}
		}
		// 顺序必须真的在**库里**，而不只是返回值里的排列。
		var firstUpload int64
		if err := adminQuery(t,
			`SELECT upload_id FROM product_images
			  WHERE product_id = $1 ORDER BY sort_order LIMIT 1`, f.prodA).
			Scan(&firstUpload); err != nil {
			t.Fatal(err)
		}
		if firstUpload != f.upA3 {
			t.Fatalf("库里 sort_order 最小的是 upload %d，期望 %d", firstUpload, f.upA3)
		}
	})

	t.Run("整组替换_不是增量", func(t *testing.T) {
		got, err := replace(f.upA2, f.upA3)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("替换成两张之后有 %d 张 —— 这是增量语义，不是整组替换", len(got))
		}
		if got[0].UploadID != f.upA2 || got[1].UploadID != f.upA3 {
			t.Fatalf("顺序是 [%d %d]，期望 [%d %d]",
				got[0].UploadID, got[1].UploadID, f.upA2, f.upA3)
		}
		var n int
		if err := adminQuery(t,
			`SELECT count(*) FROM product_images WHERE product_id = $1`, f.prodA).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("库里还有 %d 行 —— 上一组没被清干净", n)
		}
	})

	t.Run("referenced在同一个事务里置位", func(t *testing.T) {
		var referenced bool
		if err := adminQuery(t,
			`SELECT referenced FROM uploads WHERE id = $1`, f.upA2).Scan(&referenced); err != nil {
			t.Fatal(err)
		}
		if !referenced {
			t.Fatal("upload 挂上了商品但 referenced 还是 FALSE —— 孤儿回收会在 24 小时后删掉它")
		}
	})

	t.Run("传空数组即清空", func(t *testing.T) {
		got, err := replace()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("清空之后还有 %d 张", len(got))
		}
	})

	t.Run("同一个upload出现两次_在动任何一行之前就被拒", func(t *testing.T) {
		if _, err := replace(f.upA1, f.upA2); err != nil {
			t.Fatal(err)
		}
		_, err := replace(f.upA3, f.upA3)
		if !errors.Is(err, repository.ErrProductImageDuplicated) {
			t.Fatalf("期望 ErrProductImageDuplicated，实得 %v", err)
		}
		// 校验必须发生在 ClearProductImages **之前**：否则「先毁掉再发现输入
		// 不合法」这个形状会留在代码里，而它只是靠事务回滚兜住。
		var n int
		if err := adminQuery(t,
			`SELECT count(*) FROM product_images WHERE product_id = $1`, f.prodA).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Fatalf("被拒之后原来那两张变成了 %d 张", n)
		}
	})

	t.Run("用途不对的upload_是422不是404", func(t *testing.T) {
		// 退款凭证当商品图挂上去。**数据库挡不住这一件事** ——
		// 复合外键挡的是跨租户，不是用途。所以这一条只有应用层挡得住，
		// 而它和 ErrUploadNotFound 必须分得开：契约给了两个不同的 problem type。
		_, err := replace(f.upAEvidence)
		if !errors.Is(err, repository.ErrUploadWrongPurpose) {
			t.Fatalf("期望 ErrUploadWrongPurpose，实得 %v", err)
		}
		if errors.Is(err, repository.ErrUploadNotFound) {
			t.Fatal("用途不对被报成了「找不到」—— 商家会去找一个明明存在的文件")
		}
	})
}

// =============================================================================
// 五、跨租户挂别家的 upload：两道闸门，逐道验
// =============================================================================
//
// 这条测试要回答的不是「跨租户失败了吗」，而是「**它为什么失败**」。
// 本仓库反复出现的那类假绿就长在这里：跨租户测试在闸门双双失效时仍然绿，
// 因为另一个过滤条件挡住了。所以每一段都配一个阳性对照。
func TestProductImagesRejectCrossTenantUpload(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	t.Run("阳性对照_同一条路径用自己的upload是成功的", func(t *testing.T) {
		// 少了这一段，下面那个「跨租户失败」可以由「这条路径本身就是坏的」
		// 来解释 —— 而那种解释同样会让测试变绿。
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.ReplaceProductImages(ctx, f.prodA, []int64{f.upA1})
			return e
		}); err != nil {
			t.Fatalf("自家的 upload 也挂不上: %v", err)
		}
	})

	t.Run("应用层_拿别家的upload_id会被当成不存在", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.ReplaceProductImages(ctx, f.prodA, []int64{f.upB1})
			return e
		})
		if !errors.Is(err, repository.ErrUploadNotFound) {
			t.Fatalf("期望 ErrUploadNotFound，实得 %v", err)
		}
		// 「不存在」与「是别家的」刻意合成同一个错误：upload id 是自增的，
		// 两者一旦分开报，这个接口就成了一个能数出别家店传了多少文件的探测器。
		if errors.Is(err, repository.ErrUploadWrongPurpose) {
			t.Fatal("跨租户被报成了「用途不对」—— 那等于承认这个 id 存在")
		}
		var n int
		if err := adminQuery(t,
			`SELECT count(*) FROM product_images WHERE upload_id = $1`, f.upB1).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("商家 B 的文件被挂上了 %d 次", n)
		}
	})

	t.Run("数据库_绕过应用层也挂不上_复合外键是第二道", func(t *testing.T) {
		// 把应用层那一段检查整个跳过，直接发 INSERT。复合外键
		// (upload_id, merchant_id) → uploads(id, merchant_id) 必须拒绝：
		// merchant_id 由 DEFAULT current_merchant() 填成 A，而 (B的upload, A)
		// 这一对在 uploads 里不存在。
		//
		// 这一段是「应用层检查被删掉之后还剩什么」的回答。没有它，
		// 上面那条测试只证明了应用层在守，证明不了库在守。
		var pgErr *pgconn.PgError
		err := r.RawTenantTx(asA, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO product_images (product_id, upload_id, sort_order)
				 VALUES ($1, $2, 0)`, f.prodA, f.upB1)
			return e
		})
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Fatalf("期望一条 23503 外键冲突，实得 %v —— "+
				"复合外键没在守的话，应用层那一道就是唯一的一道", err)
		}

		// 阳性对照：同一条裸 INSERT，换成自己的 upload 就该成功。
		// 少了它，上面那个 23503 也可能是因为这条语句本身写错了。
		if err := r.RawTenantTx(asA, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO product_images (product_id, upload_id, sort_order)
				 VALUES ($1, $2, 9)`, f.prodA, f.upA2)
			return e
		}); err != nil {
			t.Fatalf("同一条裸 INSERT 用自己的 upload 也失败了: %v —— "+
				"上面那个 23503 证明不了任何关于租户的事", err)
		}
	})
}

// =============================================================================
// 六、product_images 的 RLS
// =============================================================================
//
// **42501 的歧义是这一组测试的头号陷阱**：permission denied（漏了 GRANT）与
// RLS 拒绝是同一个 SQLSTATE。漏 GRANT 时「跨租户被拒」看起来是通过的，
// 而真实情况是这张表对本租户也写不进去 —— 那不是隔离，是整张表都不能用。
//
// 所以每一段都先用**自己**的上下文做一次同样的操作：它成功，就证明
// keel_app 在这张表上确实有那一项权限，于是跨租户那次失败只可能是 RLS。
func TestProductImagesAreTenantIsolated(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)
	asB := tenant.NewContext(ctx, f.merchantB)

	if err := r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.ReplaceProductImages(ctx, f.prodA, []int64{f.upA1, f.upA2, f.upA3})
		return e
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("读侧_商家A看得到三张", func(t *testing.T) {
		// 阳性对照。没有它，下面那个「B 看到 0 张」可以由「根本没写进去」
		// 来解释，而那种解释会让这条测试在 RLS 被整个删掉的那天照样绿。
		var imgs []repository.ProductImage
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			imgs, e = q.ListProductImages(ctx, f.prodA)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if len(imgs) != 3 {
			t.Fatalf("商家 A 自己只看到 %d 张", len(imgs))
		}
	})

	t.Run("读侧_商家B拿着A的product_id什么也看不到", func(t *testing.T) {
		// 注意这条查询的 WHERE 里**只有** product_id 一个条件，
		// 传进去的又恰恰是 A 那件商品的 id —— 也就是说除了 RLS，
		// 没有任何别的东西能把这些行挡住。这一点是刻意的：
		// 「跨租户测试在策略失效时仍然绿，因为另一个过滤条件挡住了」
		// 是本仓库抓到过的真实假绿。
		var imgs []repository.ProductImage
		if err := r.WithTenant(asB, func(q repository.Tx) error {
			var e error
			imgs, e = q.ListProductImages(ctx, f.prodA)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if len(imgs) != 0 {
			t.Fatalf("商家 B 读到了商家 A 的 %d 张商品图 —— product_images 的 RLS 没生效", len(imgs))
		}
	})

	t.Run("写侧_阳性对照_B往自己名下插行是成功的", func(t *testing.T) {
		// 这一段证明 keel_app 在 product_images 上**有 INSERT 权限**。
		// 少了它，下面那个 42501 分不清是「RLS 拒绝」还是「压根没 GRANT」，
		// 而后者的真实含义是这张表谁都写不了 —— 一个看起来像隔离的全面故障。
		if err := r.RawTenantTx(asB, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO product_images (merchant_id, product_id, upload_id, sort_order)
				 VALUES ($1, $2, $3, 0)`, f.merchantB, f.prodB, f.upB1)
			return e
		}); err != nil {
			t.Fatalf("商家 B 往自己名下插行失败了: %v —— "+
				"keel_app 在这张表上可能根本没有 INSERT 权限", err)
		}
	})

	t.Run("写侧_B想往A名下插行_被WITH_CHECK拒绝", func(t *testing.T) {
		var pgErr *pgconn.PgError
		err := r.RawTenantTx(asB, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx,
				`INSERT INTO product_images (merchant_id, product_id, upload_id, sort_order)
				 VALUES ($1, $2, $3, 0)`, f.merchantA, f.prodA, f.upA1)
			return e
		})
		if err == nil {
			t.Fatal("商家 B 往商家 A 名下插进了一行 —— RLS 的写谓词被放开了")
		}
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("期望 42501（RLS 拒绝），实得 %v", err)
		}
		// 上一段刚证明过同一条语句在自己名下能成功，所以这个 42501
		// **只可能**是 RLS，不可能是缺 GRANT。
	})
}

// =============================================================================
// 七、商品与类目的几条闸门
// =============================================================================

func TestProductPublicationGates(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	var draft repository.AdminProduct
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		draft, e = q.CreateProduct(ctx, repository.NewProduct{
			CategoryID: f.catA, Title: "草稿",
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if draft.Status != 0 || draft.PublishedAt != nil {
		t.Fatalf("新建的商品是 status=%d published_at=%v，期望草稿", draft.Status, draft.PublishedAt)
	}

	t.Run("没有SKU的商品上不了架", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetProductPublication(ctx, draft.ID, true)
			return e
		})
		if !errors.Is(err, repository.ErrProductHasNoSKU) {
			t.Fatalf("期望 ErrProductHasNoSKU，实得 %v", err)
		}
		if errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatal("「没有 SKU」被报成了 404 —— 商家会以为商品不见了")
		}
		// **这一句是变异验证逼出来的。** 只断言错误的话，「SQL 里那条
		// AND (SELECT n FROM sku_count) > 0 被删掉」是一次**全绿**的改动：
		// Go 这一侧仍然按 sku_rows 报 409，而库里那件商品已经上架了。
		// 症状是商家看到一句「不能上架」，同时它出现在了前台列表里。
		var status int16
		if err := adminQuery(t, `SELECT status FROM products WHERE id = $1`, draft.ID).
			Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != 0 {
			t.Fatalf("报了 409，库里的 status 却是 %d —— 闸门只在 Go 这一侧，"+
				"UPDATE 其实已经执行了", status)
		}
	})

	t.Run("补一个SKU之后上得了架_published_at置位", func(t *testing.T) {
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.CreateSKU(ctx, repository.NewSKU{
				ProductID: draft.ID, SKUCode: f.suffix + "-pub", PriceCents: 900, Status: 1,
			})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		var p repository.AdminProduct
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			p, e = q.SetProductPublication(ctx, draft.ID, true)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if p.Status != 1 || p.PublishedAt == nil {
			t.Fatalf("上架之后 status=%d published_at=%v", p.Status, p.PublishedAt)
		}
		first := *p.PublishedAt

		// 下架再上架，published_at **不能**被覆盖：它是「首次发布时间」，
		// 前台按它倒序排；每次上架都覆盖的话，一次临时下架再上架就能把一件
		// 老商品顶到列表最前面。
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetProductPublication(ctx, draft.ID, false)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		var again repository.AdminProduct
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			again, e = q.SetProductPublication(ctx, draft.ID, true)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if again.PublishedAt == nil || !again.PublishedAt.Equal(first) {
			t.Fatalf("再次上架把 published_at 从 %v 改成了 %v", first, again.PublishedAt)
		}
	})

	t.Run("在架商品删不得_先下架", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteProduct(ctx, draft.ID)
		})
		if !errors.Is(err, repository.ErrProductStillPublished) {
			t.Fatalf("期望 ErrProductStillPublished，实得 %v", err)
		}
		if errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatal("「还在架」被报成了 404")
		}
	})

	t.Run("下架之后删得掉_再删是404_改它是409", func(t *testing.T) {
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.SetProductPublication(ctx, draft.ID, false)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteProduct(ctx, draft.ID)
		}); err != nil {
			t.Fatalf("下架之后还是删不掉: %v", err)
		}
		// 再删一次：契约把「已被软删」也定成 404。
		err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteProduct(ctx, draft.ID)
		})
		if !errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatalf("重复软删期望 ErrCatalogNotFound，实得 %v", err)
		}
		// 改一个已软删的商品：契约定成 409 product-deleted，**不是** 404。
		// 两者混掉的代价是后台页面说「这个商品不见了」，而它就在列表里
		// （include_deleted=true 时看得见）。
		title := "改个名"
		err = r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.UpdateProduct(ctx, draft.ID, repository.ProductPatch{Title: &title})
			return e
		})
		if !errors.Is(err, repository.ErrProductDeleted) {
			t.Fatalf("改已软删的商品期望 ErrProductDeleted，实得 %v", err)
		}
		if errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatal("「已软删」被报成了 404 —— 那和「不存在」是两件事")
		}
	})

	t.Run("跨租户_拿B的商品id_一律404", func(t *testing.T) {
		asB := tenant.NewContext(ctx, f.merchantB)
		// 阳性对照：B 自己看得到它。
		if _, err := findProduct(t, r, asB, f.prodB); err != nil {
			t.Fatalf("商家 B 看不到自己的商品: %v", err)
		}
		if _, err := findProduct(t, r, asA, f.prodB); !errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatalf("商家 A 读到了商家 B 的商品: %v", err)
		}
		err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteProduct(ctx, f.prodB)
		})
		if !errors.Is(err, repository.ErrCatalogNotFound) {
			t.Fatalf("商家 A 删商家 B 的商品期望 404，实得 %v", err)
		}
	})
}

func TestCategoryTreeMovesWholeSubtree(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	mk := func(name string, parent *int64) repository.AdminCategory {
		t.Helper()
		var c repository.AdminCategory
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			var e error
			c, e = q.CreateCategory(ctx, repository.NewCategory{ParentID: parent, Name: name})
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}

	root := mk("root", nil)
	if root.Path != fmt.Sprintf("/%d/", root.ID) || root.Level != 1 {
		t.Fatalf("根分类的 path=%q level=%d —— path 含自己的 id 这件事没落地",
			root.Path, root.Level)
	}
	mid := mk("mid", &root.ID)
	leaf := mk("leaf", &mid.ID)
	if leaf.Path != fmt.Sprintf("/%d/%d/%d/", root.ID, mid.ID, leaf.ID) || leaf.Level != 3 {
		t.Fatalf("三层之后 path=%q level=%d", leaf.Path, leaf.Level)
	}

	t.Run("移动子树_后代的path和level一起改", func(t *testing.T) {
		other := mk("other", nil)
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.MoveCategory(ctx, mid.ID, &other.ID)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		var path string
		var level int16
		if err := adminQuery(t, `SELECT path, level FROM categories WHERE id = $1`, leaf.ID).
			Scan(&path, &level); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("/%d/%d/%d/", other.ID, mid.ID, leaf.ID)
		if path != want || level != 3 {
			t.Fatalf("移动之后叶子的 path=%q level=%d，期望 %q / 3 —— "+
				"子树的 path 没跟着走，那些商品在新位置下就查不出来了", path, level, want)
		}
	})

	t.Run("移到自己的后代下面_拒绝成环", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			_, e := q.MoveCategory(ctx, mid.ID, &leaf.ID)
			return e
		})
		if !errors.Is(err, repository.ErrCategoryCycle) {
			t.Fatalf("期望 ErrCategoryCycle，实得 %v", err)
		}
	})

	t.Run("删分类的两条闸门", func(t *testing.T) {
		err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteCategory(ctx, mid.ID)
		})
		if !errors.Is(err, repository.ErrCategoryHasChildren) {
			t.Fatalf("期望 ErrCategoryHasChildren，实得 %v", err)
		}
		// f.catA 下面挂着 f.prodA。
		err = r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteCategory(ctx, f.catA)
		})
		if !errors.Is(err, repository.ErrCategoryHasProducts) {
			t.Fatalf("期望 ErrCategoryHasProducts，实得 %v", err)
		}
		// 叶子没有子分类也没有商品，删得掉 —— 阳性对照，
		// 否则上面两条可能只是因为 SoftDeleteCategory 对谁都失败。
		if err := r.WithTenant(asA, func(q repository.Tx) error {
			return q.SoftDeleteCategory(ctx, leaf.ID)
		}); err != nil {
			t.Fatalf("删一个空叶子也失败了: %v", err)
		}
	})

	t.Run("跨租户_看不到别家的分类", func(t *testing.T) {
		asB := tenant.NewContext(ctx, f.merchantB)
		var cats []repository.AdminCategory
		if err := r.WithTenant(asB, func(q repository.Tx) error {
			var e error
			cats, e = q.AdminListCategories(ctx)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		for _, c := range cats {
			if c.ID == root.ID || c.ID == mid.ID {
				t.Fatalf("商家 B 读到了商家 A 的分类 %d", c.ID)
			}
		}
		// 阳性对照：B 看得到自己那一个。
		if len(cats) == 0 {
			t.Fatal("商家 B 一个分类都看不到 —— 这条测试对 RLS 失效是失明的")
		}
	})
}

// 删 SKU 的那条闸门：在架商品的最后一个 SKU 删不得。
func TestSoftDeleteSKUKeepsPublishedProductSellable(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	// f.prodA 是 status = 1 在架，且只有 f.skuA 一个规格。
	err := r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.SoftDeleteSKU(ctx, f.skuA)
		return e
	})
	if !errors.Is(err, repository.ErrSKULastOfPublishedProduct) {
		t.Fatalf("期望 ErrSKULastOfPublishedProduct，实得 %v", err)
	}
	if errors.Is(err, repository.ErrCatalogNotFound) {
		t.Fatal("「最后一个 SKU」被报成了 404")
	}

	// 阳性对照之一：再加一个规格之后，同一个动作就该成功。
	// 少了它，上面那个失败可以由「SoftDeleteSKU 对谁都失败」来解释。
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.CreateSKU(ctx, repository.NewSKU{
			ProductID: f.prodA, SKUCode: f.suffix + "-second", PriceCents: 200, Status: 1,
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.SoftDeleteSKU(ctx, f.skuA)
		return e
	}); err != nil {
		t.Fatalf("有两个规格时删第一个失败了: %v", err)
	}

	// 阳性对照之二：跨租户删别家的 SKU 是 404，不是那条 409。
	err = r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.SoftDeleteSKU(ctx, f.skuB)
		return e
	})
	if !errors.Is(err, repository.ErrCatalogNotFound) {
		t.Fatalf("删别家的 SKU 期望 404，实得 %v", err)
	}
}
