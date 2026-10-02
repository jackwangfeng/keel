package inventory

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// 跨 0 的判据：只看第一个前值与最后一个后值是否分居 0 的两侧。
func TestStockCrossingsOnlyReportsZeroCrossings(t *testing.T) {
	var c stockCrossings
	c.record(1, 10, 5, 4) // 不跨
	c.record(1, 11, 1, 0) // 跨（到 0）
	c.record(1, 12, 0, 3) // 跨（从 0 回来）
	c.record(2, 10, 2, 0) // 另一家店
	c.record(1, 13, 3, 0) // 同一事务里先到 0……
	c.record(1, 13, 0, 2) // ……又回来：净效果不跨
	c.record(1, 14, 0, 0) // 0 → 0（核对行）不跨
	got := c.crossed()
	want := map[int64][]int64{1: {11, 12}, 2: {10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("crossed() = %v，期望 %v", got, want)
	}
	var nilRec *stockCrossings
	nilRec.record(1, 1, 1, 0) // nil 上 record 是空操作，不 panic
	if nilRec.crossed() != nil {
		t.Fatal("nil 的 crossed() 应为 nil")
	}
}

// 0.12 起门店与 SKU 在载荷里，gid 只有 stock-商家-随机串：SKU 再多也是一条消息，不再按 128 字节拆条。
func TestStockMsgPayloadRoundTrip(t *testing.T) {
	ids := make([]int64, 0, 400)
	for i := int64(0); i < 400; i++ {
		ids = append(ids, 1_000_000_000+i)
	}
	gid, err := stockMsgGID(9_000_000_000_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(gid) > 128 {
		t.Fatalf("gid 超过 128 字节：%d", len(gid))
	}
	payload, _ := json.Marshal(StockMsgPayload{StoreID: 8_000_000_000_000_000_000, SKUIDs: ids})
	m, err := DecodeStockMsg(gid, string(payload))
	if err != nil {
		t.Fatal(err)
	}
	if m.MerchantID != 9_000_000_000_000_000_000 || m.StoreID != 8_000_000_000_000_000_000 || !reflect.DeepEqual(m.SKUIDs, ids) {
		t.Fatalf("解回来不一致：商家 %d 门店 %d SKU %d 个", m.MerchantID, m.StoreID, len(m.SKUIDs))
	}
	// 每次跨 0 都是一条新消息：同样的键两次编出来的 gid 不同。
	a, _ := stockMsgGID(1)
	b, _ := stockMsgGID(1)
	if a == b {
		t.Fatalf("两次跨 0 编出了同一个 gid %q：第二条会被协调器当成第一条的重试", a)
	}
	// 旧形状（0.12 之前登记、升级时在途）没有载荷，从 gid 里解。
	old, err := DecodeStockMsg("stock-12-3-abcd-5.6", "{}")
	if err != nil || old.StoreID != 3 || !reflect.DeepEqual(old.SKUIDs, []int64{5, 6}) {
		t.Fatalf("旧形状解错了：%+v %v", old, err)
	}
	// 新形状却没有载荷 / 载荷缺字段：解不出门店与 SKU，报错（接收方记下来、吞掉）。
	for _, pl := range []string{"", "{}", `{"store_id":3}`, `{"sku_ids":[1]}`, `not json`} {
		if _, err := DecodeStockMsg(gid, pl); err == nil {
			t.Errorf("新形状配载荷 %q 应当报错", pl)
		}
	}
}

func TestParseStockMsgGIDIsStrict(t *testing.T) {
	for _, g := range []string{
		"stock-12-3-abcd-5",
		"stock-12-3-abcd-5.6",
		"stock-12-abcd", // 0.12 起的短形状：内容在载荷里
	} {
		if _, err := ParseStockMsgGID(g); err != nil {
			t.Errorf("%q 应当合法：%v", g, err)
		}
	}
	for _, g := range []string{
		"stock-012-3-abcd-5", // 租户前导零
		"stock-+12-3-abcd-5",
		"order-12-3-abcd-5",  // 别的前缀
		"stock-12-03-abcd-5", // 门店前导零
		"stock-12-3--5",      // 没有随机串
		"stock-12-3-abcd-",   // 没有 SKU
		"stock-12-3-abcd-5..6",
		"stock-12-3-abcd-05",
		"stock-12-",
		"stock-12-3-abcd-" + strings.Repeat("9", 130),
	} {
		if _, err := ParseStockMsgGID(g); err == nil {
			t.Errorf("%q 应当被拒绝", g)
		}
	}
}
