package dtm

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/tenant"
)

// 这一组测试要真的把协调器跑起来（sqlite 落到 t.TempDir()），不 mock。
// mock 掉 cgo 边界就等于不测 cgo 边界，而这一层里出问题的全在那条边界上。

func tempDSN(t *testing.T) string {
	t.Helper()
	return "sqlite:" + filepath.Join(t.TempDir(), "dtm.db")
}

const oneStep = `[{"action":"local://act","compensate":"local://act_undo"}]`

// 分支只拿到三个字符串，而它必须能靠这三个字符串把租户找回来。
//
// 这条是硬约束一的**端到端**证明，跟 gid_test.go 里那些纯函数测试不重复：
// 那边证明的是「解析器严格」，这边证明的是「协调器真的只递这三个字符串过来，
// 而 gid 在那一路上一个字节都没变」。中间隔着 Go→C→Rust→C→Go 四道边界，
// 任何一段把 gid 截断、改编码、或者换成 branch_id，都只有这条测试看得见。
func TestBranchRecoversTenantFromGID(t *testing.T) {
	gid, err := OrderGID(7, "20260926000001")
	if err != nil {
		t.Fatal(err)
	}

	type seen struct {
		gid, branchID, op string
		merchantID        int64
		parseErr          error
	}
	var mu sync.Mutex
	var got []seen

	record := func(g, b, op string) int {
		ctx, mid, _, err := TenantContextFromGID(context.Background(), g)
		mu.Lock()
		defer mu.Unlock()
		s := seen{gid: g, branchID: b, op: op, merchantID: mid, parseErr: err}
		if err == nil {
			// 租户确实进了 ctx —— repository.WithTenant 就是从这里取的。
			if v, e := tenant.FromContext(ctx); e != nil || v != mid {
				s.parseErr = e
			}
		}
		got = append(got, s)
		return Success
	}

	tc, err := Start(tempDSN(t), 0, map[string]BranchFunc{
		"act":      record,
		"act_undo": record,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	if err := tc.SubmitSaga(gid, oneStep); err != nil {
		t.Fatal(err)
	}
	status, err := tc.WaitFinal(gid, 10000)
	if err != nil {
		t.Fatalf("等待终态失败: %v", err)
	}
	if status != "succeed" {
		t.Fatalf("事务终态是 %q，期望 succeed", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("分支一次都没被调用 —— 注册没生效？")
	}
	for _, s := range got {
		if s.parseErr != nil {
			t.Fatalf("分支拿到的 gid %q 解析不出租户: %v —— "+
				"gid 在 Go→C→Rust→C→Go 这条路上被改过", s.gid, s.parseErr)
		}
		if s.gid != gid {
			t.Fatalf("分支拿到的 gid 是 %q，提交时是 %q", s.gid, gid)
		}
		if s.merchantID != 7 {
			t.Fatalf("分支解析出的租户是 %d，期望 7", s.merchantID)
		}
		if s.branchID == "" || s.op == "" {
			t.Fatalf("分支拿到的 branch_id/op 是空的：%+v", s)
		}
	}
	t.Logf("分支被调用 %d 次，每次都从 gid 复原出租户 7", len(got))
}

// 同时进到 dtmrs 里的阻塞调用数必须被信号量卡住。
//
// 守的不是「代码里有个 channel」，是**真的有上限**。上限失效的后果不是变慢：
// 每个阻塞的 cgo 调用占一个 OS 线程，超出 Go 的线程上限（默认 10000）时
// runtime 不返回错误，是 `fatal error: thread exhaustion` —— 进程直接死，
// 而触发它的只是一波正常的下单高峰。
//
// 做法：让分支睡一会儿，然后一批 goroutine 同时 WaitFinal。cap 是 3，
// 16 个并发等待里最多只能有 3 个真的进到 cgo 里。
func TestBlockingCallsAreBounded(t *testing.T) {
	const cap3, waiters = 3, 16

	gid, err := OrderGID(1, "20260926000002")
	if err != nil {
		t.Fatal(err)
	}

	tc, err := Start(tempDSN(t), cap3, map[string]BranchFunc{
		"act": func(string, string, string) int {
			time.Sleep(300 * time.Millisecond)
			return Success
		},
		"act_undo": func(string, string, string) int { return Success },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	if tc.semCap() != cap3 {
		t.Fatalf("信号量容量是 %d，期望 %d", tc.semCap(), cap3)
	}
	if err := tc.SubmitSaga(gid, oneStep); err != nil {
		t.Fatal(err)
	}

	// 用一道闸让 16 个 goroutine 同时冲进去；一个个慢慢来的话，
	// 峰值可能只有 1，测试就失去了区分力。
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if _, err := tc.WaitFinal(gid, 10000); err != nil {
				t.Errorf("WaitFinal 失败: %v", err)
			}
		}()
	}
	close(gate)
	wg.Wait()

	peak := tc.peakInflight()
	if peak > int64(cap3) {
		t.Fatalf("同时进入 dtmrs 的阻塞调用峰值是 %d，超过了上限 %d —— "+
			"信号量没有生效，一波高峰就能把 OS 线程用光", peak, cap3)
	}
	// 阳性对照：峰值必须真的顶到上限，否则「没超过」只是因为压根没并发起来，
	// 这条断言也就没有区分力。
	if peak < int64(cap3) {
		t.Fatalf("峰值只有 %d，没顶到上限 %d —— 这一轮根本没并发起来，"+
			"上面那条断言这次什么也没证明", peak, cap3)
	}
	t.Logf("%d 个并发等待，进入 dtmrs 的峰值 %d（上限 %d）", waiters, peak, cap3)
}

// Close 必须幂等。
//
// Run 里既有 defer 也可能有显式收尾，两次 dtmrs_close 是一次 double free：
// 进程在退出的最后一刻崩掉，而那时日志已经打完，看起来像正常退出。
func TestCloseIsIdempotent(t *testing.T) {
	tc, err := Start(tempDSN(t), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	tc.Close()
	tc.Close()
	tc.Close()
}

// 存储用不了时，dtm.Start 必须报错。
//
// 名字刻意不叫「Start 失败时把协调器关掉」——那句话这条测试证明不了：
// Close 有没有被调用在进程外面看不见，写成那个名字就是一个比实际断言更强的承诺。
// 收尾那段代码看得见（Start 里两处 tc.Close()），但守它的不是这里。
//
// 这条真正钉住的是启动顺序赖以成立的那条实测事实：**Open 不碰数据库，Start 才碰**。
// 所以「存储用不了」这类错误一定从 Start 出来，app.Run 才敢把它排在监听之前 ——
// 排在后面的话，一个连不上存储的进程会先开始收请求。
//
// 用一个落不下去的 sqlite 路径，不用连不上的 Postgres：后者实测要 30 秒才
// 放弃（dtmrs 自己的重试窗口，connect_timeout 管不着），而这 30 秒会加在
// 每一次 CI 上。
func TestStartFailsWhenStoreIsUnusable(t *testing.T) {
	_, err := Start("sqlite:"+filepath.Join(t.TempDir(), "没有这个目录", "dtm.db"), 0, nil)
	if err == nil {
		t.Fatal("落不下去的 sqlite 路径居然启动成功了")
	}
	t.Logf("如期失败：%v", err)
}
