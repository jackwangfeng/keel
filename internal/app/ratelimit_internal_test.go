package app

import (
	"testing"
	"time"
)

// 桶自己的两条性质。**它们在 HTTP 那一层测不到**，所以在这里测。
//
// handler/search_test.go 的 TestSearchRateLimitsAFloodFromOneIP 证明的是
// 「这道闸门真的挂在 /search 上、真的挡得住一次洪水」。它跑在真实时间上，
// 所以「额度会随时间补回来」与「闲置的桶会被回收」这两件事它验不了：
// 前者要等，后者根本看不见。而这两件事坏掉都不报错 ——
// 额度补不回来的话，一个真人搜过 8 次之后就再也搜不了了（默认配额下）；
// 桶不回收的话，这个限流器自己就是一个内存放大器，攻击者每换一个源 IP
// 就让我们多留一条记录。

func TestRateLimiterRefillsOverTime(t *testing.T) {
	now := time.Unix(0, 0)
	lim := newIPRateLimiter(4, 2) // 每秒 4 个，瞬时 2 个
	lim.now = func() time.Time { return now }

	// 先把额度打空。
	for i := 0; i < 2; i++ {
		if ok, _ := lim.allow("1.2.3.4"); !ok {
			t.Fatalf("第 %d 个就被挡了，瞬时额度是 2", i)
		}
	}
	ok, retry := lim.allow("1.2.3.4")
	if ok {
		t.Fatal("额度已经打空，第 3 个还放行了")
	}
	if retry < 1 {
		t.Errorf("Retry-After 是 %d —— 回 0 等于「立刻重试」，"+
			"而那正好是限流要挡的行为", retry)
	}

	// 250 ms 补回正好一个额度（4/秒）。
	now = now.Add(250 * time.Millisecond)
	if ok, _ := lim.allow("1.2.3.4"); !ok {
		t.Fatal("过了 250 ms（每秒 4 个 = 一个额度的时间）还是被挡着 —— " +
			"额度补不回来的话，一个真人搜过几次之后就再也搜不了了")
	}

	// 不会攒超过 burst：等很久之后也只有 2 个。
	now = now.Add(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := lim.allow("1.2.3.4"); !ok {
			t.Fatalf("等了一小时之后只放行了 %d 个，期望能攒满 2 个", i)
		}
	}
	if ok, _ := lim.allow("1.2.3.4"); ok {
		t.Error("等了一小时攒出了超过 burst 的额度 —— " +
			"那样一次长时间的沉默之后可以换来一次任意大的突发")
	}
}

func TestRateLimiterForgetsIdleSources(t *testing.T) {
	now := time.Unix(0, 0)
	lim := newIPRateLimiter(4, 2)
	lim.now = func() time.Time { return now }

	for i := 0; i < 50; i++ {
		lim.allow(ipOf(i))
	}
	if got := len(lim.buckets); got != 50 {
		t.Fatalf("记下了 %d 个来源，期望 50 —— 这条测试的前提不成立", got)
	}

	// 等过闲置期再来一个请求：扫描在写路径上顺带做，所以要有人来敲门。
	now = now.Add(2*lim.idleTTL + lim.sweepEvery)
	lim.allow("9.9.9.9")

	if got := len(lim.buckets); got != 1 {
		t.Errorf("闲置了 %v 之后还留着 %d 个桶（期望只剩刚来的那一个）—— "+
			"这个限流器自己就是一个内存放大器：攻击者每换一个源 IP "+
			"就让我们多留一条记录，而不会有任何东西报错", 2*lim.idleTTL, got)
	}

	// 反向：**还没攒回额度的**来源不许被顺手丢掉。丢掉等于给它清零重来，
	// 那时一个持续打洪水的 IP 每过一个闲置期就白得一整桶额度。
	//
	// 正常配置下这种情况不会出现 —— idleTTL 取的是「攒满一桶所需时间」的
	// 十倍，闲置那么久的桶必然已经满了。这里把 idleTTL 手工调小，
	// 逼出那个分支，验的是回收条件里 `tokens >= burst` 那一半真的在起作用：
	// 少了它，回收就变成「按时间无条件删」。
	lim2 := newIPRateLimiter(0.0001, 2) // 慢到这次测试里补不回额度
	lim2.idleTTL = time.Second
	lim2.sweepEvery = time.Second
	base := time.Unix(0, 0)
	lim2.now = func() time.Time { return base }
	for i := 0; i < 3; i++ {
		lim2.allow("5.5.5.5")
	}
	base = base.Add(10 * time.Second)
	lim2.allow("9.9.9.9")
	if _, ok := lim2.buckets["5.5.5.5"]; !ok {
		t.Error("一个还没攒回额度的来源被当成闲置丢掉了 —— " +
			"持续打洪水的 IP 每过一个闲置期就白得一整桶额度")
	}
}

func ipOf(i int) string {
	return "10.0." + itoa(i/256) + "." + itoa(i%256)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
