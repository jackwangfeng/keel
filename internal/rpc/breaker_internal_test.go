package rpc

import (
	"testing"
	"time"
)

// 熔断器的状态机，用假时钟走一遍：关闭 → 连续失败打开 → 冷却后半开只放一个 → 探测失败重新打开
// → 再冷却 → 探测成功关闭。
func TestBreakerStateMachine(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newBreaker(3, 10*time.Second)
	b.now = func() time.Time { return now }

	fail := func() {
		t.Helper()
		if !b.allow() {
			t.Fatalf("状态 %s 下不该拒绝", b.state())
		}
		b.done(outcomeFailure)
	}

	// 关闭：失败两次还没到阈值；一次成功把计数清零。
	fail()
	fail()
	if !b.allow() {
		t.Fatal("没到阈值就拒绝了")
	}
	b.done(outcomeSuccess)
	fail()
	fail()
	if b.state() != "closed" {
		t.Fatalf("成功清零之后又失败两次，应当还是关闭，实得 %s", b.state())
	}
	// 调用方取消不算失败。
	if !b.allow() {
		t.Fatal("关闭状态拒绝了")
	}
	b.done(outcomeIgnored)
	if b.state() != "closed" {
		t.Fatalf("调用方取消被算成了失败：%s", b.state())
	}

	// 第三次连续失败：打开。
	fail()
	if b.state() != "open" {
		t.Fatalf("连续 3 次失败应当打开，实得 %s", b.state())
	}
	if b.allow() {
		t.Fatal("打开状态放行了")
	}

	// 冷却到了：半开，只放一个。
	now = now.Add(10 * time.Second)
	if b.state() != "half-open" {
		t.Fatalf("冷却之后应当半开，实得 %s", b.state())
	}
	if !b.allow() {
		t.Fatal("半开不放探测")
	}
	if b.allow() {
		t.Fatal("半开放了第二个请求 —— 探测还没回来")
	}
	// 探测失败：重新打开，重新计冷却。
	b.done(outcomeFailure)
	if b.state() != "open" || b.allow() {
		t.Fatalf("探测失败应当重新打开，实得 %s", b.state())
	}
	now = now.Add(9 * time.Second)
	if b.allow() {
		t.Fatal("冷却没重新计：探测失败 9 秒后就放行了")
	}

	// 半开的探测被调用方取消：什么都没学到，下一个请求接着探。
	now = now.Add(time.Second)
	if !b.allow() {
		t.Fatal("冷却之后不放探测")
	}
	b.done(outcomeIgnored)
	if !b.allow() {
		t.Fatal("探测被取消之后，下一个请求应当可以接着探")
	}
	// 探测成功：关闭，计数清零。
	b.done(outcomeSuccess)
	if b.state() != "closed" {
		t.Fatalf("探测成功应当关闭，实得 %s", b.state())
	}
	fail()
	fail()
	if b.state() != "closed" {
		t.Fatalf("关闭之后计数没清零：%s", b.state())
	}
}
