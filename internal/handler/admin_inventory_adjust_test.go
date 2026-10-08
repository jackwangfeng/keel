package handler_test

// 库存的相对调整（POST .../inventory/adjustments，数据模型 §15 第 12 / 18 条）。
//
// 这一组要钉住的是「相对」两个字带来的每一处与 CAS 不同的地方：
//
//	① 加减本身、以及「扣完会变负」是 409 inventory-insufficient（带 current），
//	   不是 CAS 那条会教客户端重试的 inventory-precondition-failed；
//	② 缺行 ≡ 可售 0：正的 delta 建出那一行，负的 delta 是 409、current 为 0；
//	③ 404 与 409 分得开（软删的 SKU、别家店的 SKU 都是 404）；
//	④ **不是天然幂等的**，所以同一把钥匙重放不能再加一次；
//	⑤ 并发的调整全部生效（这是它存在的理由 —— CAS 在这里会让 19 个人拿到 409）；
//	⑥ 每一次成功写一行 biz_type = 5 的流水，重放不写第二行。
//
// 断言一律同时看响应与库里（inventories / inventory_logs），理由与 order_fixture_test.go
// 的文件头一样：只看响应的话，它只证明了同一段代码前后自洽。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// adjustPath 是按门店那条的路径。
func adjustPath(storeID, skuID int64) string {
	return fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory/adjustments", storeID, skuID)
}

// storeQtyInDB 直接读 inventories 那一行；没有那一行时返回 (0, false)。
func storeQtyInDB(t *testing.T, storeID, skuID int64) (int32, bool) {
	t.Helper()
	var qty int32
	err := admin(t).QueryRow(context.Background(),
		`SELECT available_qty FROM inventories WHERE store_id = $1 AND sku_id = $2`,
		storeID, skuID).Scan(&qty)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false
		}
		t.Fatalf("读门店 %d / sku %d 的库存失败: %v", storeID, skuID, err)
	}
	return qty, true
}

// manualLog 是一行 biz_type = 5 的流水。
type manualLog struct {
	ChangeQty int32
	BizID     string
	Before    int32
	After     int32
	Reason    *string
}

// manualLogsOf 读一个门店 SKU 的全部手工调整流水，按写入顺序。
func manualLogsOf(t *testing.T, storeID, skuID int64) []manualLog {
	t.Helper()
	rows, err := adminSession(t).Query(context.Background(), `
		SELECT change_qty, biz_id, before_available, after_available, reason
		  FROM inventory_logs
		 WHERE store_id = $1 AND sku_id = $2 AND biz_type = 5
		 ORDER BY id`, storeID, skuID)
	if err != nil {
		t.Fatalf("读手工调整流水失败: %v", err)
	}
	defer rows.Close()
	var out []manualLog
	for rows.Next() {
		var l manualLog
		if err := rows.Scan(&l.ChangeQty, &l.BizID, &l.Before, &l.After, &l.Reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// 加、减、扣到正好 0、扣过头（409 + current），以及每一次成功各一行流水。
func TestInventoryAdjustAddsAndSubtractsAndRefusesToGoNegative(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJ", 1000, 10)

	var inv api.AdminInventory
	decodeInto(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku),
		`{"delta":100,"reason":"3 月进货"}`, sh.Token), http.StatusOK, "进货 100", &inv)
	if inv.AvailableQty != 110 || inv.StoreId != sh.StoreID || inv.SkuId != sku {
		t.Fatalf("进货之后得到 store=%d sku=%d qty=%d，期望 %d / %d / 110",
			inv.StoreId, inv.SkuId, inv.AvailableQty, sh.StoreID, sku)
	}

	decodeInto(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku),
		`{"delta":-30}`, sh.Token), http.StatusOK, "扣 30", &inv)
	if inv.AvailableQty != 80 {
		t.Fatalf("扣 30 之后是 %d，期望 80", inv.AvailableQty)
	}

	// 扣过头：409 inventory-insufficient，current 是真实值，而且**什么都没写**。
	w := postIdem(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":-81}`, sh.Token)
	if got := problemType(t, w, http.StatusConflict, "扣过头"); got != problem.TypeInventoryInsufficient {
		t.Fatalf("扣过头的 Problem type 是 %q，期望 %q —— 尤其不能是 inventory-precondition-failed："+
			"那一个在教客户端重读重试，而这一个原样重试永远不会成功", got, problem.TypeInventoryInsufficient)
	}
	var conflict api.InventoryConflict
	decodeInto(t, w, http.StatusConflict, "扣过头的冲突体", &conflict)
	if conflict.Current.AvailableQty != 80 || conflict.Current.StoreId != sh.StoreID {
		t.Fatalf("409 里的 current 是 store=%d qty=%d，期望 %d / 80",
			conflict.Current.StoreId, conflict.Current.AvailableQty, sh.StoreID)
	}
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 80 {
		t.Fatalf("409 之后库里是 %d，期望还是 80", q)
	}

	// 边界：正好扣到 0 是允许的（条件是 >= 0，不是 > 0）。
	decodeInto(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku),
		`{"delta":-80}`, sh.Token), http.StatusOK, "扣到 0", &inv)
	if inv.AvailableQty != 0 {
		t.Fatalf("扣到 0 之后是 %d", inv.AvailableQty)
	}

	logs := manualLogsOf(t, sh.StoreID, sku)
	want := []struct{ change, before, after int32 }{{100, 10, 110}, {-30, 110, 80}, {-80, 80, 0}}
	if len(logs) != len(want) {
		t.Fatalf("手工调整流水有 %d 行，期望 %d 行（三次成功；409 那一次不该留下任何东西）：%+v",
			len(logs), len(want), logs)
	}
	for i, l := range logs {
		if l.ChangeQty != want[i].change || l.Before != want[i].before || l.After != want[i].after {
			t.Fatalf("第 %d 行流水是 %+d（%d → %d），期望 %+d（%d → %d）", i+1,
				l.ChangeQty, l.Before, l.After, want[i].change, want[i].before, want[i].after)
		}
	}
	if logs[0].Reason == nil || *logs[0].Reason != "3 月进货" {
		t.Fatalf("第一行流水的 reason 是 %v，期望「3 月进货」", logs[0].Reason)
	}
	if logs[1].Reason != nil {
		t.Fatalf("没传 reason 的那一次，流水里的 reason 应为 NULL，实际是 %q", *logs[1].Reason)
	}
	if want := fmt.Sprintf("adj:%d:", sh.StaffID); len(logs[0].BizID) <= len(want) || logs[0].BizID[:len(want)] != want {
		t.Fatalf("流水的 biz_id 是 %q，期望以 %q 打头（谁、哪一次请求）", logs[0].BizID, want)
	}
}

// 缺行 ≡ 可售 0：正的 delta 建出那一行，负的 delta 是 409、current 为 0、仍然没有那一行。
func TestInventoryAdjustOnAMissingRowTreatsItAsZero(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJMISS", 1000, 5)
	region := createRegion(t, sh, "adjm", "调整大区")
	second := createStore(t, sh, region, "adjm", "新开的店", 116.2, 39.9)
	setFence(t, sh, second, 116.15, 39.85, 116.25, 39.95)
	if _, ok := storeQtyInDB(t, second, sku); ok {
		t.Fatal("新开的店已经有这一行库存 —— 夹具的前提不成立，下面什么也验不了")
	}

	// 负的 delta：从 0 扣，409，current 为 0，而且**不建行**。
	w := postIdem(t, sh.Host, adjustPath(second, sku), `{"delta":-1}`, sh.Token)
	if got := problemType(t, w, http.StatusConflict, "缺行扣减"); got != problem.TypeInventoryInsufficient {
		t.Fatalf("缺行扣减的 Problem type 是 %q，期望 %q", got, problem.TypeInventoryInsufficient)
	}
	var conflict api.InventoryConflict
	decodeInto(t, w, http.StatusConflict, "缺行扣减的冲突体", &conflict)
	if conflict.Current.AvailableQty != 0 || conflict.Current.StoreId != second || conflict.Current.SkuId != sku {
		t.Fatalf("缺行扣减的 current 是 %+v，期望 store=%d sku=%d qty=0", conflict.Current, second, sku)
	}
	if _, ok := storeQtyInDB(t, second, sku); ok {
		t.Fatal("一次失败的扣减建出了库存行 —— 「从没录过」被改写成了「录过、是 0」")
	}

	// 正的 delta：建出那一行。
	var inv api.AdminInventory
	decodeInto(t, postIdem(t, sh.Host, adjustPath(second, sku), `{"delta":7}`, sh.Token),
		http.StatusOK, "缺行进货", &inv)
	if inv.AvailableQty != 7 || inv.StoreId != second || inv.WarningQty != 0 {
		t.Fatalf("缺行进货之后得到 %+v，期望 store=%d qty=7 warning=0", inv, second)
	}
	if q, ok := storeQtyInDB(t, second, sku); !ok || q != 7 {
		t.Fatalf("缺行进货之后库里是 (%d, %v)，期望 (7, true)", q, ok)
	}
	// 另一家店没被动过：WHERE 里漏了 store_id 的话，两家会一起变。
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 5 {
		t.Fatalf("调第二家店把默认店也改了：默认店现在是 %d，期望还是 5", q)
	}
	logs := manualLogsOf(t, second, sku)
	if len(logs) != 1 || logs[0].Before != 0 || logs[0].After != 7 || logs[0].ChangeQty != 7 {
		t.Fatalf("缺行进货的流水是 %+v，期望一行 +7（0 → 7）", logs)
	}
}

// 软删的 SKU、别家店的 SKU、别家店的门店 → 404，**不是** 409。
func TestInventoryAdjustTellsNotFoundApartFromInsufficient(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)
	skuA := seedOneSKU(t, a, "ADJ404A", 1000, 3)
	skuB := seedOneSKU(t, b, "ADJ404B", 1000, 3)

	for _, tc := range []struct {
		what string
		path string
	}{
		{"别家店的 SKU", adjustPath(a.StoreID, skuB)},
		{"别家店的门店", adjustPath(b.StoreID, skuA)},
	} {
		// 用一个一定会「扣过头」的 delta：如果可见性判错了，这里会拿到 409 而不是 404。
		w := postIdem(t, a.Host, tc.path, `{"delta":-100}`, a.Token)
		if got := problemType(t, w, http.StatusNotFound, tc.what); got != problem.TypeNotFound {
			t.Fatalf("%s 回的 Problem type 是 %q，期望 %q", tc.what, got, problem.TypeNotFound)
		}
	}
	if q, _ := storeQtyInDB(t, b.StoreID, skuB); q != 3 {
		t.Fatalf("别家店的库存被动过了：现在是 %d，期望 3", q)
	}

	// 软删之后 404 —— 先证明它删之前是可调的（阳性对照）。
	wantStatus(t, postIdem(t, a.Host, adjustPath(a.StoreID, skuA), `{"delta":1}`, a.Token),
		http.StatusOK, "删之前调一次")
	wantStatus(t, deleteAs(t, a.Host, fmt.Sprintf("/api/v1/admin/skus/%d", skuA), a.Token),
		http.StatusNoContent, "软删 SKU")
	w := postIdem(t, a.Host, adjustPath(a.StoreID, skuA), `{"delta":1}`, a.Token)
	if got := problemType(t, w, http.StatusNotFound, "软删之后调"); got != problem.TypeNotFound {
		t.Fatalf("给软删的 SKU 调库存回的 Problem type 是 %q，期望 %q", got, problem.TypeNotFound)
	}
	// 捷径同理。
	w = postIdem(t, a.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory/adjustments", skuA),
		`{"delta":1}`, a.Token)
	if got := problemType(t, w, http.StatusNotFound, "捷径：软删之后调"); got != problem.TypeNotFound {
		t.Fatalf("捷径给软删的 SKU 调库存回的 Problem type 是 %q，期望 %q", got, problem.TypeNotFound)
	}
}

// 请求体的边界：0、超过上限、reason 超长、缺 Idempotency-Key，全部 422，而且什么都没写。
func TestInventoryAdjustRejectsBadBodies(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJ422", 1000, 3)
	long := make([]rune, 201)
	for i := range long {
		long[i] = '货'
	}
	for _, body := range []string{
		`{"delta":0}`,
		`{}`,
		`{"delta":1000001}`,
		`{"delta":-1000001}`,
		fmt.Sprintf(`{"delta":1,"reason":%q}`, string(long)),
	} {
		wantStatus(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku), body, sh.Token),
			http.StatusUnprocessableEntity, "请求体 "+body)
	}
	// 200 个汉字正好在上限上（按字符计，不按字节）—— 阳性对照。
	wantStatus(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku),
		fmt.Sprintf(`{"delta":1,"reason":%q}`, string(long[:200])), sh.Token),
		http.StatusOK, "200 字的 reason")

	// 缺 Idempotency-Key：422。相对调整不是天然幂等的，这把钥匙不能是可选的。
	wantStatus(t, postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":1}`, sh.Token, ""),
		http.StatusUnprocessableEntity, "不带 Idempotency-Key")

	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 4 {
		t.Fatalf("被拒的请求改了库存：现在是 %d，期望 4（只有那一次 200 生效）", q)
	}
}

// 同一把钥匙重放：返回同一个结果、带 Idempotency-Replayed，**不再加一次**，也不多写流水。
// 换了请求体则是 422 idempotency-key-reused。
func TestInventoryAdjustReplayDoesNotApplyTwice(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJIDEM", 1000, 10)
	key := freshIdemKey()

	var first, second api.AdminInventory
	w := postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":100}`, sh.Token, key)
	decodeInto(t, w, http.StatusOK, "第一次", &first)
	if w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("第一次请求就带了 Idempotency-Replayed")
	}
	w = postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":100}`, sh.Token, key)
	decodeInto(t, w, http.StatusOK, "重放", &second)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("重放没有带 Idempotency-Replayed: true")
	}
	if second.AvailableQty != 110 || first.AvailableQty != 110 {
		t.Fatalf("两次响应的水位是 %d / %d，期望都是 110（重放返回首次的结果）",
			first.AvailableQty, second.AvailableQty)
	}
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 110 {
		t.Fatalf("重放之后库里是 %d，期望 110 —— 「+100」被网络重试加了两次", q)
	}
	if logs := manualLogsOf(t, sh.StoreID, sku); len(logs) != 1 {
		t.Fatalf("重放之后有 %d 行流水，期望 1 行", len(logs))
	}

	// 同一把钥匙、不同请求体 → 422，而不是被当成重放静默吞掉。
	w = postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":5}`, sh.Token, key)
	if got := problemType(t, w, http.StatusUnprocessableEntity, "钥匙复用"); got != problem.TypeIdempotencyKeyReused {
		t.Fatalf("钥匙复用回的 Problem type 是 %q，期望 %q", got, problem.TypeIdempotencyKeyReused)
	}
	// 同一把钥匙打捷径（落到同一家店）也是另一个请求：422。
	w = postWithKey(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory/adjustments", sku),
		`{"delta":100}`, sh.Token, key)
	if got := problemType(t, w, http.StatusUnprocessableEntity, "钥匙跨路径复用"); got != problem.TypeIdempotencyKeyReused {
		t.Fatalf("钥匙跨路径复用回的 Problem type 是 %q，期望 %q", got, problem.TypeIdempotencyKeyReused)
	}

	// 409 不存档：扣过头失败之后，补完货拿同一把钥匙原样重试可以成功。
	key2 := freshIdemKey()
	wantStatus(t, postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":-200}`, sh.Token, key2),
		http.StatusConflict, "扣过头")
	wantStatus(t, postIdem(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":90}`, sh.Token),
		http.StatusOK, "补货")
	var after api.AdminInventory
	decodeInto(t, postWithKey(t, sh.Host, adjustPath(sh.StoreID, sku), `{"delta":-200}`, sh.Token, key2),
		http.StatusOK, "补货之后同一把钥匙重试", &after)
	if after.AvailableQty != 0 {
		t.Fatalf("补货之后重试扣 200 得到 %d，期望 0", after.AvailableQty)
	}
}

// 捷径：一家店时可用（落到那一家），两家店时 409 store-ambiguous、什么都没写。
func TestInventoryAdjustWithoutStoreIsAmbiguousWhenThereAreTwoStores(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJAMB", 1000, 4)
	path := fmt.Sprintf("/api/v1/admin/skus/%d/inventory/adjustments", sku)

	var inv api.AdminInventory
	decodeInto(t, postIdem(t, sh.Host, path, `{"delta":6}`, sh.Token), http.StatusOK, "单店捷径", &inv)
	if inv.StoreId != sh.StoreID || inv.AvailableQty != 10 {
		t.Fatalf("单店捷径得到 store=%d qty=%d，期望 %d / 10", inv.StoreId, inv.AvailableQty, sh.StoreID)
	}
	if logs := manualLogsOf(t, sh.StoreID, sku); len(logs) != 1 || logs[0].After != 10 {
		t.Fatalf("单店捷径的流水是 %+v，期望一行 → 10", logs)
	}

	region := createRegion(t, sh, "adja", "第二大区")
	second := createStore(t, sh, region, "adja", "第二家店", 116.2, 39.9)
	setFence(t, sh, second, 116.15, 39.85, 116.25, 39.95)

	w := postIdem(t, sh.Host, path, `{"delta":6}`, sh.Token)
	if got := problemType(t, w, http.StatusConflict, "两家店时的捷径"); got != problem.TypeStoreAmbiguous {
		t.Fatalf("两家店时捷径回的 Problem type 是 %q，期望 %q", got, problem.TypeStoreAmbiguous)
	}
	if q, _ := storeQtyInDB(t, sh.StoreID, sku); q != 10 {
		t.Fatalf("store-ambiguous 之后默认店是 %d，期望还是 10", q)
	}
	if _, ok := storeQtyInDB(t, second, sku); ok {
		t.Fatal("store-ambiguous 之后第二家店多出了一行库存 —— 捷径猜了一家")
	}
}

// 并发：20 个请求各 +1，结果是 +20，20 行流水首尾相接。
//
// 这是相对调整存在的全部理由：同样的 20 个人用 CAS，19 个会拿到 409。
// 流水首尾相接（每一行的 before 是上一行的 after）证明了 before = after - delta
// 在并发下仍然精确 —— 它不是从一个会过期的快照里算出来的。
func TestInventoryAdjustConcurrentDeltasAllApply(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "ADJCONC", 1000, 0)
	region := createRegion(t, sh, "adjc", "并发大区")
	fresh := createStore(t, sh, region, "adjc", "并发的店", 116.2, 39.9)
	setFence(t, sh, fresh, 116.15, 39.85, 116.25, 39.95)

	// 两家店各跑一组：默认店有行（走 DO UPDATE），新店缺行（20 个并发的首次进货
	// 抢同一个 INSERT —— 先查后插的实现会在这里撞出 23505）。
	for _, store := range []int64{sh.StoreID, fresh} {
		const n = 20
		codes := make([]int, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = postIdem(t, sh.Host, adjustPath(store, sku), `{"delta":1}`, sh.Token).Code
			}(i)
		}
		wg.Wait()
		for i, c := range codes {
			if c != http.StatusOK {
				t.Fatalf("门店 %d 第 %d 个并发请求回了 %d，期望 200", store, i, c)
			}
		}
		if q, _ := storeQtyInDB(t, store, sku); q != n {
			t.Fatalf("门店 %d 并发 %d 次 +1 之后是 %d，期望 %d", store, n, q, n)
		}
		logs := manualLogsOf(t, store, sku)
		if len(logs) != n {
			t.Fatalf("门店 %d 的流水有 %d 行，期望 %d", store, len(logs), n)
		}
		for i, l := range logs {
			if l.Before != int32(i) || l.After != int32(i+1) {
				t.Fatalf("门店 %d 第 %d 行流水是 %d → %d，期望 %d → %d（流水要首尾相接）",
					store, i+1, l.Before, l.After, i, i+1)
			}
		}
	}
}

// 比较并设置（PUT）也要留流水：CAS 成功就证明写之前恰好是 expected，
// 所以 before / delta 不用再读一次就是精确的。只改预警线、数量没变的那一次不写。
func TestInventorySetLeavesAManualLogToo(t *testing.T) {
	sh := newAdminShop(t)
	sku := seedOneSKU(t, sh, "SETLOG", 1000, 10)

	storePath := fmt.Sprintf("/api/v1/admin/stores/%d/skus/%d/inventory", sh.StoreID, sku)
	wantStatus(t, putAs(t, sh.Host, storePath,
		`{"available_qty":25,"expected_available_qty":10}`, sh.Token), http.StatusOK, "按门店设为 25")
	// 单店捷径那一条走的是另一段代码，同样要留。
	wantStatus(t, putAs(t, sh.Host, fmt.Sprintf("/api/v1/admin/skus/%d/inventory", sku),
		`{"available_qty":7,"expected_available_qty":25}`, sh.Token), http.StatusOK, "捷径设为 7")
	// 数量不变、只改预警线：没有库存变动，不写。
	wantStatus(t, putAs(t, sh.Host, storePath,
		`{"available_qty":7,"expected_available_qty":7,"warning_qty":3}`, sh.Token), http.StatusOK, "只改预警线")
	// CAS 对不上：什么都没写，也不该有流水。
	_ = putAs(t, sh.Host, storePath, `{"available_qty":99,"expected_available_qty":1}`, sh.Token)

	logs := manualLogsOf(t, sh.StoreID, sku)
	want := []struct{ change, before, after int32 }{{15, 10, 25}, {-18, 25, 7}}
	if len(logs) != len(want) {
		t.Fatalf("手工流水有 %d 行，期望 %d 行（两次改了数量；只改预警线与 409 不留）：%+v", len(logs), len(want), logs)
	}
	for i, l := range logs {
		if l.ChangeQty != want[i].change || l.Before != want[i].before || l.After != want[i].after {
			t.Fatalf("第 %d 行流水是 %+d（%d → %d），期望 %+d（%d → %d）", i+1,
				l.ChangeQty, l.Before, l.After, want[i].change, want[i].before, want[i].after)
		}
		if p := fmt.Sprintf("set:%d:", sh.StaffID); len(l.BizID) <= len(p) || l.BizID[:len(p)] != p {
			t.Fatalf("第 %d 行流水的 biz_id 是 %q，期望以 %q 打头", i+1, l.BizID, p)
		}
		if l.Reason != nil {
			t.Fatalf("PUT 没有 reason，流水里应为 NULL，实际是 %q", *l.Reason)
		}
	}
	if logs[0].BizID == logs[1].BizID {
		t.Fatalf("两次 PUT 的 biz_id 一样（%q）—— 流水里分不出是两次操作", logs[0].BizID)
	}
}
