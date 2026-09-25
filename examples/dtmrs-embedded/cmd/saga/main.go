// saga 演示嵌入式协调器的四种行为：正向提交、逆序补偿、拉取式异步分支、
// 以及未注册分支在提交期就被拒绝。
//
// 加 RESUME=1 与参数 fail 可以演示崩溃恢复：
//
//	go run ./cmd/saga fail      # 提交后不 close 直接退出（模拟崩溃）
//	RESUME=1 go run ./cmd/saga  # 重启，未完成的事务被驱动到终态
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/keel/examples/dtmrs-embedded/dtmrs"
)

func main() {
	db := "/tmp/dtmrs_saga_demo.db"
	resume := os.Getenv("RESUME") == "1"
	if !resume {
		os.Remove(db)
	}
	crash := len(os.Args) > 1 && os.Args[1] == "fail"

	dsn := os.Getenv("DTMRS_DSN")
	if dsn == "" {
		dsn = "sqlite:" + db
	}
	fmt.Println("### backend:", dsn)

	tc, err := dtmrs.Open(dsn)
	must(err)

	var mu sync.Mutex
	var trace []string
	var calls int64
	note := func(s string) { mu.Lock(); trace = append(trace, s); mu.Unlock() }

	// 分支就是普通 Go 函数，不是 HTTP handler —— 这正是嵌入式形态的意义。
	branch := func(tag string, result int) dtmrs.BranchFunc {
		return func(gid, branchID, op string) int {
			atomic.AddInt64(&calls, 1)
			fmt.Printf("  [分支] gid=%s branch=%s op=%s\n", gid, branchID, op)
			note(tag)
			return result
		}
	}
	must(tc.Register("deduct", branch("deduct", dtmrs.Success)))
	must(tc.Register("deduct_undo", branch("deduct_undo", dtmrs.Success)))
	must(tc.Register("ship_undo", branch("ship_undo", dtmrs.Success)))
	// 这个分支拒绝 —— 必须触发逆序补偿
	must(tc.Register("ship_bad", branch("ship_bad->FAILURE", dtmrs.Failure)))
	must(tc.RegisterPull("async_charge"))

	must(tc.Start())

	if resume {
		fmt.Println("(RESUME) 进程重启，等待崩溃前未完成的 go-5 被驱动到终态")
		st, err := tc.WaitFinal("go-5", 10000)
		fmt.Println("  status:", st, errStr(err))
		tc.Close()
		return
	}

	// Go 侧的拉取循环：任务随便丢 goroutine，不受 C 回调栈约束
	stop := make(chan struct{})
	var pulled int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for {
				task, ok := tc.NextTask()
				if !ok {
					break
				}
				atomic.AddInt64(&pulled, 1)
				note("pull:" + task)
				var t struct {
					TaskID uint64 `json:"task_id"`
				}
				must(json.Unmarshal([]byte(task), &t))
				go func(id uint64) {
					time.Sleep(20 * time.Millisecond) // 模拟异步业务
					tc.Reply(id, dtmrs.Success)
				}(t.TaskID)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	step := func(n, title, gid, steps string) {
		fmt.Printf("(%s) %s\n", n, title)
		if err := tc.SubmitSaga(gid, steps); err != nil {
			fmt.Println("  提交被拒绝:", err)
			return
		}
		st, err := tc.WaitFinal(gid, 8000)
		fmt.Println("  status:", st, errStr(err))
	}

	step("1", "正向提交，全部是 local:// 进程内分支", "go-1",
		`[{"action":"local://deduct","compensate":"local://deduct_undo"}]`)
	step("2", "第二步拒绝 -> 逆序补偿", "go-2",
		`[{"action":"local://deduct","compensate":"local://deduct_undo"},`+
			`{"action":"local://ship_bad","compensate":"local://ship_undo"}]`)
	step("3", "拉取式分支，由 Go 的 goroutine 驱动", "go-3",
		`[{"action":"local://async_charge","compensate":"local://deduct_undo"}]`)
	step("4", "未注册的 local:// 名字在提交期就被拒绝", "go-4",
		`[{"action":"local://nope","compensate":"local://nope2"}]`)

	if crash {
		fmt.Println("(5) 模拟事务中途崩溃（提交后不 close 直接退出）")
		must(tc.SubmitSaga("go-5",
			`[{"action":"local://deduct","compensate":"local://deduct_undo"}]`))
		os.Exit(7)
	}

	close(stop)
	wg.Wait()
	tc.Close()

	fmt.Printf("\n--- 分支调用 %d 次，拉取任务 %d 个\n",
		atomic.LoadInt64(&calls), atomic.LoadInt64(&pulled))
	for _, l := range trace {
		fmt.Println("   ", l)
	}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return "(err: " + err.Error() + ")"
}
