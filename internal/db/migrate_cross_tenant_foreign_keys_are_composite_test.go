package db_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCrossTenantForeignKeysAreComposite(t *testing.T) {
	conn := migratedConn(t)
	man := loadManifest(t)

	hasMerchantID := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		var n int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_attribute a
			   JOIN pg_class c ON c.oid = a.attrelid
			   JOIN pg_namespace ns ON ns.oid = c.relnamespace
			  WHERE ns.nspname = 'public' AND c.relname = $1
			    AND a.attname = 'merchant_id' AND a.attnum > 0 AND NOT a.attisdropped`,
			tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		hasMerchantID[tbl] = n > 0
	}

	used := map[string]bool{}
	checked := 0
	for _, fk := range foreignKeys(t, conn) {
		// 两端都带 merchant_id 才是「跨租户可错挂」的形状。
		if !hasMerchantID[fk.from] || !hasMerchantID[fk.to] {
			continue
		}
		checked++
		if slices.Contains(fk.cols, "merchant_id") {
			continue
		}
		key := fmt.Sprintf("%s(%s)", fk.from, strings.Join(fk.cols, ","))
		if exempt(man.FKSingleOK, key) {
			used[key] = true
			continue
		}
		t.Errorf("外键 %s（%s → %s）只引用了 %v，没带 merchant_id——"+
			"A 商家的行可以挂到 B 商家的行上，数据库不会拒绝。"+
			"改成 FOREIGN KEY (..., merchant_id) REFERENCES %s(id, merchant_id)，"+
			"或把 %s 登记进 db/tenancy.json 的 fk_single_column_ok 并写明理由",
			fk.name, fk.from, fk.to, fk.cols, fk.to, key)
	}
	if checked == 0 {
		t.Fatal("一条跨租户外键都没查到——这个检查本身失效了")
	}

	// 清单里写着一条库里根本没有的豁免，说明它过期了——它会静默地豁免掉
	// 一条将来同名的外键。只对**库里已有的表**做这个检查：清单是前瞻的，
	// M2/M3 才会建的表现在就登记在册，那不是过期。
	live := map[string]bool{}
	for _, tbl := range allTables(t, conn) {
		live[tbl] = true
	}
	for key := range man.FKSingleOK {
		if strings.HasPrefix(key, "_") || used[key] {
			continue
		}
		if tbl, _, ok := strings.Cut(key, "("); ok && live[tbl] {
			t.Errorf("fk_single_column_ok 里的 %s 已经不是一条单列跨租户外键了，该清理了", key)
		}
	}
}

// 规矩二的**对偶**：看起来是外键、却根本没有外键的列。
//
// 上面那条测试有一个盲区，是实测出来的，不是推断：
//
//	把 products 的复合外键降级成单列 → 变红 ✅
//	把这条外键整个删掉               → 仍然全绿 ❌
//
// **违规做得更彻底反而绕过了闸门。** 它只看「已存在的外键够不够复合」，
// 而「压根没有外键」这件事在它的查询里根本不产生一行。
//
// 代价具体：数据模型 §5 的 order_items 按原样实现时，sku_id / product_id
// 都没有外键约束，于是 A 商家的订单行可以引用 B 商家的 SKU——数据库不拒、
// 闸门不红、测试全绿。而订单行带着价格与规格快照，错挂之后展示、对账、退款
// 看起来全都正常，只有拿 sku_id 回查库存时才露馅，而那正是 M2 下单 SAGA 的主链路。
//
// 所以这里做对偶断言：业务表上名为 <x>_id、存在同名父表（sku_id → skus）、
// 却没有任何外键引用它的列，要么补外键，要么进 db/tenancy.json 的
// fk_missing_ok 并写明理由。
