package rpc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keel/keel/internal/rpc"
)

// ReadJSON 的读超时、熔断器与错误分类，对着一个行为可控的对面走一遍。
//
// 最要紧的一条是分类：熔断器打开时返回的是 ErrCircuitOpen（请求没发出去，确定失败），
// **不是** ErrUnknown —— 原有的「确定失败 vs 结果未知」那一刀不能因为多了熔断器而变形。
func TestReadJSONTimeoutAndBreaker(t *testing.T) {
	var mode atomic.Value // "slow" / "ok" / "400"
	mode.Store("slow")
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load().(string) {
		case "slow":
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		case "400":
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := rpc.NewClient(srv.URL, secret, 5*time.Second,
		rpc.WithReadTimeout(100*time.Millisecond), rpc.WithBreaker(3, 300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out struct{ OK bool }

	// 读超时：对面卡 2 秒，读在 100ms 附近就回来，而且是结果未知。
	start := time.Now()
	err = c.ReadJSON(ctx, "/x", struct{}{}, &out)
	if !rpc.IsUnknown(err) {
		t.Fatalf("读超时应当是 ErrUnknown，实得 %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("读超时没生效：等了 %s（读超时 100ms，写超时 5s）", d)
	}
	// 再失败两次：打开。
	for i := 0; i < 2; i++ {
		_ = c.ReadJSON(ctx, "/x", struct{}{}, &out)
	}
	if got := c.BreakerState(); got != "open" {
		t.Fatalf("连续 3 次超时应当熔断，实得 %s", got)
	}

	// 打开：不发请求、立刻失败，分类是 ErrCircuitOpen 而不是 ErrUnknown。
	before := hits.Load()
	start = time.Now()
	err = c.ReadJSON(ctx, "/x", struct{}{}, &out)
	if !errors.Is(err, rpc.ErrCircuitOpen) || rpc.IsUnknown(err) {
		t.Fatalf("熔断时应当是 ErrCircuitOpen 且不是 ErrUnknown，实得 %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("熔断时还在等：%s", d)
	}
	if hits.Load() != before {
		t.Fatal("熔断时请求还是发出去了")
	}
	// 写不经过熔断器：读的熔断器开着，写照样发出去、照样成功。
	mode.Store("ok")
	if err := c.PostJSON(ctx, "/x", struct{}{}, &out); err != nil {
		t.Fatalf("写不该被读的熔断器拦下：%v", err)
	}

	// 冷却之后半开：探测成功 → 关闭。
	time.Sleep(350 * time.Millisecond)
	if got := c.BreakerState(); got != "half-open" {
		t.Fatalf("冷却之后应当半开，实得 %s", got)
	}
	if err := c.ReadJSON(ctx, "/x", struct{}{}, &out); err != nil || !out.OK {
		t.Fatalf("半开探测应当成功：%v", err)
	}
	if got := c.BreakerState(); got != "closed" {
		t.Fatalf("探测成功之后应当关闭，实得 %s", got)
	}

	// 4xx 是对面回答了：分类不变（ErrBadRequest，确定失败），也不计入熔断。
	mode.Store("400")
	for i := 0; i < 5; i++ {
		err := c.ReadJSON(ctx, "/x", struct{}{}, &out)
		if !errors.Is(err, rpc.ErrBadRequest) || rpc.IsUnknown(err) {
			t.Fatalf("400 应当是 ErrBadRequest，实得 %v", err)
		}
	}
	if got := c.BreakerState(); got != "closed" {
		t.Fatalf("4xx 被算成了失败，熔断器 %s", got)
	}

	// 调用方自己取消不计入熔断。
	mode.Store("slow")
	for i := 0; i < 5; i++ {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		_ = c.ReadJSON(cctx, "/x", struct{}{}, &out)
		cancel()
	}
	if got := c.BreakerState(); got != "closed" {
		t.Fatalf("调用方取消被算成了失败，熔断器 %s", got)
	}
}
