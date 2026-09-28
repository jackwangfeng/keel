package handler

import (
	"testing"
	"time"
)

// 每把密钥一个固定窗口：窗口内第 limit+1 次被拒并告诉还要等多久；窗口过去重新计数；密钥之间互不影响。
func TestKeyLimiter(t *testing.T) {
	l := newKeyLimiter(3, time.Minute)
	t0 := time.Unix(1_000_000, 0)
	for i := 0; i < 3; i++ {
		if _, ok := l.allow(1, t0.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("第 %d 次就被拒了", i+1)
		}
	}
	wait, ok := l.allow(1, t0.Add(10*time.Second))
	if ok || wait != 50*time.Second {
		t.Fatalf("第 4 次应被拒并等 50 秒：ok=%v wait=%v", ok, wait)
	}
	if _, ok := l.allow(2, t0.Add(10*time.Second)); !ok {
		t.Fatal("另一把密钥被连带限流了")
	}
	if _, ok := l.allow(1, t0.Add(61*time.Second)); !ok {
		t.Fatal("窗口过去之后没有重新计数")
	}
}
