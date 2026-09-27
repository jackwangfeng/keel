package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/dtm"
	"github.com/keel/keel/internal/inventory"
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

// 下单扣减（微服务拆分阶段 1b 起是库存服务的 SAGA 分支）在仓储这一层的三条出路。
//
// 拆分前这里守的是「两种 UPDATE 0 必须分得开」：别家的 SKU 报 ErrSKUNotInTenant、缺货报
// ErrInsufficientStock。拆分之后库存服务看不见 skus 表，「这个 SKU 是不是本租户的」由 core 在定价时判
// （ListSKUsForPricing 在 RLS 之下查不到别家的 SKU，下单直接 422），库存服务只认 id 与数。
// 于是在库存这一层，别家的库存行与缺行**长得一样**（RLS 把它挡在视野外 ≡ 可售 0），都是
// 「库存不足」的拒绝 —— 这里要守的变成了**真正要命的那一半**：别家的水位一件都不能动，
// 而且拒绝确实提交了（core 的收尾分支靠它回 409）。三条路径互为对照。
func runDeduct(t *testing.T, local *inventory.Local, merchantID int64, op, orderNo string, storeID, skuID int64, qty int32) int {
	t.Helper()
	gid, err := dtm.OrderGID(merchantID, orderNo)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := inventory.EncodeDeductPayload(inventory.DeductPayload{
		OrderNo: orderNo, StoreID: storeID, Lines: []inventory.OrderLine{{SKUID: skuID, Qty: qty}},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := inventory.BranchDeduct
	if op == "compensate" {
		name = inventory.BranchRestore
	}
	return local.SagaBranches()[name](gid, "03", op, payload)
}

func trailOf(t *testing.T, local *inventory.Local, merchantID int64, orderNo string) []inventory.TrailEntry {
	t.Helper()
	tr, err := local.OrderTrail(tenant.NewContext(context.Background(), merchantID), orderNo)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestInventoryDeductBranchPaths(t *testing.T) {
	f := seedInventories(t)
	local := inventory.NewLocal(repository.NewInventoryStore(pool(t)))
	no := func(tag string) string { return fmt.Sprintf("inv%s%d", tag, time.Now().UnixNano()) }

	t.Run("自家_库存够_扣成功并记流水", func(t *testing.T) {
		o := no("ok")
		if got := runDeduct(t, local, f.merchantA, "action", o, f.storeA, f.skuA, 3); got != dtm.Success {
			t.Fatalf("扣自家库存返回 %d", got)
		}
		if got := availableQty(t, f.skuA); got != 7 {
			t.Fatalf("库里的水位是 %d，期望 7", got)
		}
		tr := trailOf(t, local, f.merchantA, o)
		if len(tr) != 1 || tr[0].BizType != inventory.BizOrderDeduct || tr[0].Before != 10 || tr[0].After != 7 {
			t.Fatalf("流水不对：%+v", tr)
		}
	})

	t.Run("自家_库存不足_提交一行拒绝_水位不动", func(t *testing.T) {
		o := no("short")
		before := availableQty(t, f.skuA)
		if got := runDeduct(t, local, f.merchantA, "action", o, f.storeA, f.skuA, before+1); got != dtm.Success {
			t.Fatalf("缺货返回 %d，期望 Success（拒绝是提交的结论，不是失败）", got)
		}
		if got := availableQty(t, f.skuA); got != before {
			t.Fatalf("库存不足时水位从 %d 变成了 %d", before, got)
		}
		tr := trailOf(t, local, f.merchantA, o)
		if len(tr) != 1 || tr[0].BizType != inventory.BizOrderRejected || tr[0].Reason != inventory.RejectInsufficient {
			t.Fatalf("期望一行 insufficient 拒绝，实得 %+v", tr)
		}
	})

	t.Run("别家的SKU_一件都不动", func(t *testing.T) {
		// 商家 B 的那一行水位是 10，足够扣 1：拒绝只可能来自 RLS 把它挡在视野外。
		o := no("cross")
		if got := runDeduct(t, local, f.merchantA, "action", o, f.storeB, f.skuB, 1); got != dtm.Success {
			t.Fatalf("返回 %d", got)
		}
		if got := availableQty(t, f.skuB); got != 10 {
			t.Fatalf("商家 B 的水位变成了 %d —— 跨租户扣减真的写进去了", got)
		}
		if tr := trailOf(t, local, f.merchantA, o); len(tr) != 1 || tr[0].BizType != inventory.BizOrderRejected {
			t.Fatalf("期望商家 A 名下一行拒绝，实得 %+v", tr)
		}
		if tr := trailOf(t, local, f.merchantB, o); len(tr) != 0 {
			t.Fatalf("商家 B 名下出现了流水：%+v", tr)
		}
	})

	t.Run("补偿按流水放回_别家的不补", func(t *testing.T) {
		// 补偿最容易直接攥着 sku_id 回滚（数据模型 §4 点名说了）。按流水放：商家 A 名下这一单
		// 没扣过商家 B 的货，所以什么都不补 —— 回补别家的库存不是「补偿失败」，是往别人账上打钱。
		o := no("comp")
		if got := runDeduct(t, local, f.merchantA, "compensate", o, f.storeB, f.skuB, 5); got != dtm.Success {
			t.Fatalf("补偿返回 %d", got)
		}
		if got := availableQty(t, f.skuB); got != 10 {
			t.Fatalf("商家 B 的水位变成了 %d —— 跨租户回补真的写进去了", got)
		}
		// 阳性对照：自家扣 2 再补偿，回到原值，两行流水。
		o2 := no("comp2")
		before := availableQty(t, f.skuA)
		runDeduct(t, local, f.merchantA, "action", o2, f.storeA, f.skuA, 2)
		if got := runDeduct(t, local, f.merchantA, "compensate", o2, f.storeA, f.skuA, 2); got != dtm.Success {
			t.Fatalf("补偿返回 %d", got)
		}
		if got := availableQty(t, f.skuA); got != before {
			t.Fatalf("补偿之后水位 %d，期望回到 %d", got, before)
		}
		if tr := trailOf(t, local, f.merchantA, o2); len(tr) != 2 || tr[1].BizType != inventory.BizSagaCompensate || tr[1].Change != 2 {
			t.Fatalf("流水不对：%+v", tr)
		}
	})
}

// 没有租户上下文时，库存仓储必须在 Go 这一侧就拒绝（tenant.ErrNoTenant），而不是落进
// 「库存不足」—— 那会让「所有下单都失败」表现为「所有商品都缺货」。
func TestInventoryStoreWithoutTenantIsRefused(t *testing.T) {
	store := repository.NewInventoryStore(pool(t))
	err := store.WithTenant(context.Background(), func(repository.InventoryStoreTx) error { return nil })
	if !errors.Is(err, tenant.ErrNoTenant) {
		t.Fatalf("期望 tenant.ErrNoTenant，实得 %v", err)
	}
}
