package worker_test

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/testdb"
	"github.com/keel/keel/internal/worker"
)

// 选主要真的咨询锁，所以本包连库（keel_test_worker）。只用它的锁，不读写任何表。
func TestMain(m *testing.M) {
	os.Exit(testdb.Main(m, testdb.Package{Name: "worker"}))
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("10 秒内没等到：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// panic 不崩进程：记下来、退避重启；Stop 等任务真的退出。
func TestPanicIsRecoveredAndTaskRestarted(t *testing.T) {
	var runs atomic.Int32
	exited := make(chan struct{})
	r := worker.New(worker.Config{MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	r.Add(worker.Task{Name: "flaky", Run: func(ctx context.Context) {
		if runs.Add(1) <= 2 {
			var m map[string]int
			m["boom"] = 1 // 真的 panic：写 nil map
		}
		<-ctx.Done()
		close(exited)
	}})
	// 意外返回（ctx 没取消就 return）同样重启。
	var quits atomic.Int32
	r.Add(worker.Task{Name: "quitter", Run: func(ctx context.Context) {
		if quits.Add(1) == 1 {
			return
		}
		<-ctx.Done()
	}})
	r.Start(context.Background())

	eventually(t, "panic 两次之后第三次跑起来", func() bool { return runs.Load() == 3 })
	eventually(t, "意外返回之后被重启", func() bool { return quits.Load() == 2 })
	if !r.Stop(5 * time.Second) {
		t.Fatal("Stop 没在宽限期内等到任务退出")
	}
	select {
	case <-exited:
	default:
		t.Fatal("Stop 返回了，任务却还没退出 —— 没等")
	}
}

// Stop 的宽限期：不守 ctx 的任务不能把停机卡死。
func TestStopDoesNotHangOnATaskThatIgnoresCtx(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	r := worker.New(worker.Config{})
	r.Add(worker.Task{Name: "stubborn", Run: func(context.Context) { <-block }})
	r.Start(context.Background())
	start := time.Now()
	if r.Stop(100 * time.Millisecond) {
		t.Fatal("任务明明没退出，Stop 却报告全部退出了")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Stop 被卡住了 %s", d)
	}
}

// instance 是一个「实例」：一个 Runner，一条选主连接（记下它的后端 pid，好在测试里掐断它）。
type instance struct {
	r       *worker.Runner
	pid     atomic.Uint32
	running atomic.Int32 // 选主任务此刻在不在跑（0 / 1）
	starts  atomic.Int32 // 选主任务启动过几次
	workers atomic.Int32 // 并行任务此刻在跑的份数
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	in := &instance{}
	in.r = worker.New(worker.Config{
		RetryInterval: 50 * time.Millisecond,
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			c, err := pgx.Connect(ctx, db.DSN())
			if err == nil {
				in.pid.Store(c.PgConn().PID())
			}
			return c, err
		},
	})
	in.r.Add(worker.Task{Name: "test.scan", Leader: true, Run: func(ctx context.Context) {
		in.running.Add(1)
		in.starts.Add(1)
		defer in.running.Add(-1)
		<-ctx.Done()
	}})
	in.r.Add(worker.Task{Name: "test.consume", Run: func(ctx context.Context) {
		in.workers.Add(1)
		defer in.workers.Add(-1)
		<-ctx.Done()
	}})
	return in
}

// 两个实例：选主任务只在一个上跑，并行任务两边都跑；持锁的实例停掉之后另一个接手。
func TestLeaderTaskRunsOnExactlyOneInstance(t *testing.T) {
	a, b := newInstance(t), newInstance(t)
	a.r.Start(context.Background())
	eventually(t, "a 当选", func() bool { return a.running.Load() == 1 })
	b.r.Start(context.Background())
	defer b.r.Stop(5 * time.Second)

	eventually(t, "并行任务两边都在跑", func() bool { return a.workers.Load() == 1 && b.workers.Load() == 1 })
	// 给 b 几轮竞选的机会：它必须一直抢不到。
	time.Sleep(300 * time.Millisecond)
	if b.running.Load() != 0 {
		t.Fatal("两个实例同时在跑选主任务")
	}

	// a 停机（连接关闭即释放锁）：b 在几轮之内接手。
	if !a.r.Stop(5 * time.Second) {
		t.Fatal("a 没在宽限期内停下")
	}
	if a.running.Load() != 0 {
		t.Fatal("a 停了，选主任务还在跑")
	}
	eventually(t, "b 接手", func() bool { return b.running.Load() == 1 })
}

// 选主连接断了（库重启、被 pg_terminate_backend）：锁随会话没了，本实例要停掉选主任务，
// 然后重连、重新竞选 —— 而不是带着一把已经不存在的锁继续跑。
func TestLeaderReElectsAfterConnectionLoss(t *testing.T) {
	a := newInstance(t)
	a.r.Start(context.Background())
	defer a.r.Stop(5 * time.Second)
	eventually(t, "a 当选", func() bool { return a.running.Load() == 1 })
	oldPID := a.pid.Load()

	admin, err := pgx.Connect(context.Background(), db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var ok bool
	if err := admin.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, int32(oldPID)).Scan(&ok); err != nil || !ok {
		t.Fatalf("掐断选主连接失败：%v %v", ok, err)
	}

	eventually(t, "断线之后换了一条新连接并重新当选", func() bool {
		return a.pid.Load() != oldPID && a.starts.Load() == 2 && a.running.Load() == 1
	})
}

// 没有选主连接（Connect 为 nil）：选主任务宁可不跑，也不能每个实例都跑；并行任务照跑。
func TestLeaderTasksDoNotRunWithoutAConnection(t *testing.T) {
	var leader, plain atomic.Int32
	r := worker.New(worker.Config{})
	r.Add(worker.Task{Name: "x", Leader: true, Run: func(ctx context.Context) { leader.Add(1); <-ctx.Done() }})
	r.Add(worker.Task{Name: "y", Run: func(ctx context.Context) { plain.Add(1); <-ctx.Done() }})
	r.Start(context.Background())
	defer r.Stop(time.Second)
	eventually(t, "并行任务在跑", func() bool { return plain.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if leader.Load() != 0 {
		t.Fatal("没有选主连接却跑了选主任务")
	}
}

// Supervise 单独给任务内部的 goroutine 用：同样接住 panic。
func TestSuperviseRecoversInsideTaskGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Supervise(ctx, nil, "inner", time.Millisecond, time.Millisecond, func(ctx context.Context) {
			if n.Add(1) == 1 {
				panic("inner boom")
			}
			<-ctx.Done()
		})
	}()
	eventually(t, "内部 goroutine panic 之后重启", func() bool { return n.Load() == 2 })
	cancel()
	wg.Wait()
}
