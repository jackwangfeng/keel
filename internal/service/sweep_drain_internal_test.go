package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
)

// 超时关单的「一轮跑满就接着跑」（sweep.go 的 Run / drain），对着一个假仓储走一遍。
//
// 假仓储只有一家商户、一堆孤儿草稿（status 0）：孤儿草稿那条路只碰 ListExpired* 与
// CloseExpiredDraftOrder 两个方法、外加库存服务的 OrderTrail，最好假。两类订单在预算上
// 是同一个口径（sweepTenant 返回动过的笔数），所以连跑的判据用草稿验就够了。

type drainRepo struct {
	mu      sync.Mutex
	drafts  []string // 还没关掉的草稿单号
	closed  int
	rounds  int  // ListExpiredDraftOrders 被调了几次 = 跑了几轮（单商户、每轮一次）
	failAll bool // 每一笔关单都失败（库存服务 / 数据库出事的那种）
}

func (r *drainRepo) ActiveMerchants(context.Context) ([]int64, error) { return []int64{1}, nil }

func (r *drainRepo) WithTenant(_ context.Context, fn func(repository.Tx) error) error {
	return fn(drainTx{r: r})
}

func (r *drainRepo) state() (closed, rounds, left int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed, r.rounds, len(r.drafts)
}

// drainTx 只实现孤儿草稿那条路用得到的三个方法；其余方法落到内嵌的 nil 接口上 ——
// 被调到就 panic，说明 sweep 的路径变了、这个假仓储不再够用。
type drainTx struct {
	repository.Tx
	r *drainRepo
}

func (drainTx) ListExpiredPendingOrders(context.Context, int32) ([]repository.ExpiredOrder, error) {
	return nil, nil
}

func (t drainTx) ListExpiredDraftOrders(_ context.Context, limit int32) ([]repository.ExpiredOrder, error) {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	t.r.rounds++
	n := min(int(limit), len(t.r.drafts))
	out := make([]repository.ExpiredOrder, 0, n)
	for _, no := range t.r.drafts[:n] {
		out = append(out, repository.ExpiredOrder{OrderNo: no})
	}
	return out, nil
}

func (t drainTx) CloseExpiredDraftOrder(_ context.Context, orderNo string) error {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	if t.r.failAll {
		return errors.New("假的：关单失败")
	}
	for i, no := range t.r.drafts {
		if no == orderNo {
			t.r.drafts = append(t.r.drafts[:i], t.r.drafts[i+1:]...)
			t.r.closed++
			return nil
		}
	}
	return repository.ErrOrderNotClaimed
}

// noTrail 是库存服务的替身：孤儿草稿在库存里一行流水都没有（正常情形）。
type noTrail struct{ inventory.Service }

func (noTrail) OrderTrail(context.Context, string) ([]inventory.TrailEntry, error) { return nil, nil }

func newDrainRepo(n int) *drainRepo {
	r := &drainRepo{}
	for i := 0; i < n; i++ {
		r.drafts = append(r.drafts, fmt.Sprintf("D%05d", i))
	}
	return r
}

func newDrainSweeper(r *drainRepo, budget, maxRounds int) *SweepService {
	return NewSweepService(r, noTrail{}, SweepConfig{
		PerTenantCap: budget, RoundBudget: budget,
		Interval:         time.Hour, // 测试里绝不会等到第二次唤醒
		MaxRoundsPerWake: maxRounds, BurstPause: time.Millisecond,
	}, nil)
}

// runFor 跑 Run 直到 cond 成立（或 5 秒），再多看 100ms 确认它真的停下了（在按 Interval 睡）。
func runFor(t *testing.T, s *SweepService, r *drainRepo, cond func(closed, rounds, left int) bool) (int, int, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, rd, l := r.state()
		if cond(c, rd, l) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("5 秒内没等到：closed=%d rounds=%d left=%d", c, rd, l)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	return r.state()
}

// 积压 55 笔、每轮 10 笔：一次唤醒连跑 6 轮（5 轮跑满 + 1 轮只剩 5 笔）就清完，然后去睡。
// 修复前每分钟一轮，这 55 笔要 6 分钟。
func TestSweepDrainsBacklogInOneWake(t *testing.T) {
	r := newDrainRepo(55)
	closed, rounds, left := runFor(t, newDrainSweeper(r, 10, 20), r,
		func(_, _, left int) bool { return left == 0 })
	if closed != 55 || left != 0 {
		t.Fatalf("一次唤醒之后关了 %d 笔、剩 %d 笔，期望全部 55 笔", closed, left)
	}
	if rounds != 6 {
		t.Fatalf("跑了 %d 轮，期望 6 轮（第 6 轮没跑满，就该停下去睡）", rounds)
	}
}

// 积压超过「单次唤醒上限 × 每轮预算」：连跑到上限就停，剩下的等下一次唤醒 ——
// 总上限是这个任务在一次唤醒里对数据库的压力上限。
func TestSweepDrainStopsAtMaxRoundsPerWake(t *testing.T) {
	r := newDrainRepo(500)
	closed, rounds, left := runFor(t, newDrainSweeper(r, 10, 20), r,
		func(_, rounds, _ int) bool { return rounds >= 20 })
	if rounds != 20 || closed != 200 || left != 300 {
		t.Fatalf("rounds=%d closed=%d left=%d，期望连跑 20 轮、关 200 笔、剩 300 笔等下一次唤醒", rounds, closed, left)
	}
}

// 一轮里全部失败（库存服务不在之类）：不连跑 —— 下一轮扫到的还是同一批，连跑只是把失败重复 20 遍。
func TestSweepDoesNotBurstOnAllFailures(t *testing.T) {
	r := newDrainRepo(100)
	r.failAll = true
	closed, rounds, _ := runFor(t, newDrainSweeper(r, 10, 20), r,
		func(_, rounds, _ int) bool { return rounds >= 1 })
	if rounds != 1 || closed != 0 {
		t.Fatalf("全部失败时跑了 %d 轮（关了 %d 笔），期望只跑 1 轮就去睡", rounds, closed)
	}
}
