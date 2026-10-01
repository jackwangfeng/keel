package service

import (
	"context"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/tenant"
)

// 总数缓存（product_total_cache.go）：TTL 内复用、过期重数、容量有上限、键里带商户。
func TestTotalCacheTTLAndCapacity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	c := newTotalCache(30*time.Second, 3)
	c.now = func() time.Time { return now }

	k := totalKey{merchant: 1, store: 10, region: 2}
	c.put(k, 42)
	if n, ok := c.get(k); !ok || n != 42 {
		t.Fatalf("刚放进去的取不到：%d %v", n, ok)
	}
	now = now.Add(29 * time.Second)
	if _, ok := c.get(k); !ok {
		t.Fatal("29 秒就过期了")
	}
	now = now.Add(time.Second)
	if _, ok := c.get(k); ok {
		t.Fatal("30 秒还没过期")
	}

	// 容量：放 10 个不同的键，表长不超过 3；刚放进去的那个一定在。
	for i := int64(1); i <= 10; i++ {
		kk := totalKey{merchant: 1, store: 10, region: 2, category: i}
		c.put(kk, i)
		if c.len() > 3 {
			t.Fatalf("放第 %d 个之后表长 %d，超过容量 3", i, c.len())
		}
		if n, ok := c.get(kk); !ok || n != i {
			t.Fatalf("第 %d 个刚放进去就取不到", i)
		}
	}

	// ttl <= 0：整个关掉。
	off := newTotalCache(0, 3)
	off.put(k, 1)
	if _, ok := off.get(k); ok {
		t.Fatal("ttl=0 时还在缓存")
	}
	var nilCache *totalCache
	nilCache.put(k, 1)
	if _, ok := nilCache.get(k); ok {
		t.Fatal("nil 缓存取到了东西")
	}
}

// cachedTotal：同一组（商户, 门店, 类目, 只看有货）第二次不再数；换任何一维都重数。
// 商户那一维是多租户的底线：两家的门店 / 类目参数完全相同，也不能拿到对方的数。
func TestCachedTotalKeyedByMerchant(t *testing.T) {
	s := &ProductService{totals: newTotalCache(time.Minute, 100)}
	sc := repository.StoreScope{StoreID: 10, RegionID: 2}
	cat := int64(7)
	calls := 0
	countAs := func(n int64) func() (int64, error) {
		return func() (int64, error) { calls++; return n, nil }
	}
	get := func(merchant int64, category *int64, inStock bool, n int64) int64 {
		t.Helper()
		got, err := s.cachedTotal(tenant.NewContext(context.Background(), merchant), sc, category, inStock, countAs(n))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if get(1, nil, false, 100) != 100 || get(1, nil, false, 999) != 100 || calls != 1 {
		t.Fatalf("同一个键第二次又数了（calls=%d）", calls)
	}
	if get(2, nil, false, 5) != 5 {
		t.Fatal("B 家拿到了 A 家的总数")
	}
	if get(1, &cat, false, 30) != 30 || get(1, nil, true, 80) != 80 {
		t.Fatal("类目 / 只看有货不在键里")
	}
	if calls != 4 {
		t.Fatalf("calls=%d，期望 4", calls)
	}

	// 关掉缓存（WithoutTotalCache）：每次都现数。
	s.WithoutTotalCache()
	get(1, nil, false, 1)
	get(1, nil, false, 1)
	if calls != 6 {
		t.Fatalf("关掉缓存之后 calls=%d，期望 6", calls)
	}
}
