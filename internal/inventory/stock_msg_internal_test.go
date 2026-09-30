package inventory

import (
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

func TestStockMsgGIDRoundTripAndChunking(t *testing.T) {
	ids := make([]int64, 0, 40)
	for i := int64(0); i < 40; i++ {
		ids = append(ids, 1_000_000_000+i)
	}
	gids, err := stockMsgGIDs(9_000_000_000_000_000_000, 8_000_000_000_000_000_000, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(gids) < 2 {
		t.Fatalf("40 个十位数的 SKU 应当拆成几条，实得 %d 条", len(gids))
	}
	var back []int64
	for _, g := range gids {
		if len(g) > 128 {
			t.Fatalf("gid 超过 128 字节：%d %q", len(g), g)
		}
		m, err := ParseStockMsgGID(g)
		if err != nil {
			t.Fatalf("解不回来 %q: %v", g, err)
		}
		if m.MerchantID != 9_000_000_000_000_000_000 || m.StoreID != 8_000_000_000_000_000_000 {
			t.Fatalf("租户 / 门店解错了：%+v", m)
		}
		back = append(back, m.SKUIDs...)
	}
	if !reflect.DeepEqual(back, ids) {
		t.Fatalf("拆开再拼回来的 SKU 不一致：%v", back)
	}
	// 每次跨 0 都是一条新消息：同样的键两次编出来的 gid 不同。
	a, _ := stockMsgGIDs(1, 2, []int64{3})
	b, _ := stockMsgGIDs(1, 2, []int64{3})
	if a[0] == b[0] {
		t.Fatalf("两次跨 0 编出了同一个 gid %q：第二条会被协调器当成第一条的重试", a[0])
	}
}

func TestParseStockMsgGIDIsStrict(t *testing.T) {
	for _, g := range []string{
		"stock-12-3-abcd-5",
		"stock-12-3-abcd-5.6",
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
		"stock-12-3",
		"stock-12-3-abcd-" + strings.Repeat("9", 130),
	} {
		if _, err := ParseStockMsgGID(g); err == nil {
			t.Errorf("%q 应当被拒绝", g)
		}
	}
}
