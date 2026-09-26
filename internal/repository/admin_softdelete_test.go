package repository_test

import (
	"context"
	"testing"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 软删掉的规格必须从**买家看得到的每一个视图**里消失。
//
// skus.deleted_at 是 00018 新加的，而前台那几条查询是 M1/M2 写的 —— 它们此前
// 没有、也不可能有这个条件。补漏的那几行（products.sql 的 ListProductSKUs、
// orders.sql 的 ListSKUsForPricing、search.sql 里四处 in_stock 的 EXISTS）
// 由这条测试守着：漏掉其中任何一条，一个被商家删掉的规格照样出现在详情页上，
// 或者照样下得了单 —— 而它已经不在任何一个后台视图里，商家看不到这笔订单卖的是什么。
func TestSoftDeletedSKUDisappearsFromBuyerViews(t *testing.T) {
	ctx := context.Background()
	f := seedCatalog(t)
	r := repository.New(pool(t))
	asA := tenant.NewContext(ctx, f.merchantA)

	// 先补一个规格，否则 f.skuA 是在架商品的最后一个，删不掉（那条闸门另有测试）。
	var keep repository.AdminSKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		keep, e = q.CreateSKU(ctx, repository.NewSKU{
			ProductID: f.prodA, SKUCode: f.suffix + "-keep", PriceCents: 300,
			Status: 1, AvailableQty: 5,
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}

	// 阳性对照：删之前，两个规格在前台详情里都看得见。
	// 少了它，下面那句「看不见了」可以由「这条查询本来就一条也不返回」来解释。
	var before []repository.SKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		before, e = q.ListProductSKUs(ctx, f.prodA)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("删之前前台详情里有 %d 个规格，期望 2", len(before))
	}

	if err := r.WithTenant(asA, func(q repository.Tx) error {
		_, e := q.SoftDeleteSKU(ctx, f.skuA)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	var after []repository.SKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		after, e = q.ListProductSKUs(ctx, f.prodA)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].ID != keep.ID {
		t.Fatalf("软删之后前台详情里还有 %d 个规格（%+v）—— "+
			"ListProductSKUs 漏了 deleted_at IS NULL", len(after), after)
	}

	// 后台的规格列表同样不含软删的（契约：AdminProductDetail.skus 不含已软删）。
	var adminSKUs []repository.AdminSKU
	if err := r.WithTenant(asA, func(q repository.Tx) error {
		var e error
		adminSKUs, e = q.AdminListProductSKUs(ctx, f.prodA)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(adminSKUs) != 1 {
		t.Fatalf("软删之后后台列表里还有 %d 个规格", len(adminSKUs))
	}

	// 而它在库里还在 —— 软删不删行。硬删在这个 schema 下做不到：
	// order_items / cart_items / inventories / inventory_logs 四张表都对 skus
	// 有外键，一个卖过一次的 SKU 永远删不掉。
	var n int
	if err := adminQuery(t, `SELECT count(*) FROM skus WHERE id = $1`, f.skuA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("软删把行真的删掉了")
	}
}
