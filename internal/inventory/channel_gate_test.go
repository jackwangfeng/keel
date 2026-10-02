package inventory

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// stock.changed 的判据：首前值与末后值不同就算变了（与跨不跨 0 无关）。
func TestStockCrossingsChanged(t *testing.T) {
	var c stockCrossings
	c.record(1, 10, 5, 4)  // 变了
	c.record(1, 11, 3, 3)  // 核对行，没变
	c.record(1, 12, 10, 9) // 先 10 → 9……
	c.record(1, 12, 9, 10) // ……又回 10：净效果没变
	c.record(2, 10, 0, 1)  // 另一家店，跨 0 也算变
	got := c.changed()
	want := map[int64][]int64{1: {10}, 2: {10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed() = %v，期望 %v", got, want)
	}
	var nilRec *stockCrossings
	if nilRec.changed() != nil {
		t.Fatal("nil 的 changed() 应为 nil")
	}
}

type fakeLookup struct {
	on    bool
	err   error
	calls int
}

func (f *fakeLookup) ChannelMerchantEnabled(context.Context) (bool, error) {
	f.calls++
	return f.on, f.err
}

func TestChannelGateOffNeverQueries(t *testing.T) {
	for _, g := range []*ChannelGate{nil, NewChannelGate(false)} {
		f := &fakeLookup{on: true}
		ok, err := g.allows(context.Background(), f, 7)
		if ok || err != nil || f.calls != 0 || g.Queries() != 0 {
			t.Fatalf("开关关闭：allows=%v err=%v 查了 %d 次 / 计数 %d，期望不放行且一次都不查", ok, err, f.calls, g.Queries())
		}
	}
}

func TestChannelGateCachesPerMerchant(t *testing.T) {
	g := NewChannelGate(true)
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	f := &fakeLookup{on: true}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if ok, err := g.allows(ctx, f, 7); !ok || err != nil {
			t.Fatalf("第 %d 次：allows=%v err=%v", i, ok, err)
		}
	}
	if f.calls != 1 || g.Queries() != 1 {
		t.Fatalf("TTL 内查了 %d 次（计数 %d），期望 1", f.calls, g.Queries())
	}
	if _, _ = g.allows(ctx, f, 8); f.calls != 2 {
		t.Fatalf("另一家商家没有单独查：%d", f.calls)
	}
	now = now.Add(channelGateTTL + time.Second)
	f.on = false
	if ok, _ := g.allows(ctx, f, 7); ok || f.calls != 3 {
		t.Fatalf("TTL 过期后：allows=%v 查了 %d 次，期望重查且不放行", ok, f.calls)
	}
	g.Remember(7, true) // 本进程收到了「开」的消息：立刻生效，不等 TTL
	if ok, _ := g.allows(ctx, f, 7); !ok || f.calls != 3 {
		t.Fatalf("Remember 之后：allows=%v 查了 %d 次，期望直接放行", ok, f.calls)
	}
	g.Forget(7)
	if _, _ = g.allows(ctx, f, 7); f.calls != 4 {
		t.Fatalf("Forget 之后没有重查：%d", f.calls)
	}
}

func TestChannelGateLookupErrorIsReturnedNotCached(t *testing.T) {
	g := NewChannelGate(true)
	f := &fakeLookup{err: errors.New("库不可用")}
	if _, err := g.allows(context.Background(), f, 7); err == nil {
		t.Fatal("查询失败却没有报错")
	}
	f.err, f.on = nil, true
	if ok, _ := g.allows(context.Background(), f, 7); !ok || f.calls != 2 {
		t.Fatalf("失败不该被缓存：allows=%v 查了 %d 次", ok, f.calls)
	}
}
