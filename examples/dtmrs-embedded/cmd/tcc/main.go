// tcc 验证 dtmrs 0.11 新导出的 TCC 能力。
//
// 0.8 的 C ABI 只有 dtmrs_submit_saga，没有 TCC —— 那曾是本项目
// 「预售定金 / 多仓调拨等需要预留语义的场景」唯一的拦路石。0.11 解决了。
//
//	go run ./cmd/tcc          # 全部 Try 成功 -> Submit -> Confirm
//	go run ./cmd/tcc fail     # 第二个 Try 失败 -> Abort -> 逆序 Cancel
package main

import (
	"fmt"
	"os"
	"sync"

	"github.com/keel/examples/dtmrs-embedded/dtmrs"
)

var (
	mu    sync.Mutex
	trace []string
)

func note(s string) { mu.Lock(); trace = append(trace, s); mu.Unlock() }

func main() {
	shouldFail := len(os.Args) > 1 && os.Args[1] == "fail"
	db := "/tmp/dtmrs_tcc_demo.db"
	os.Remove(db)

	tc, err := dtmrs.Open("sqlite:" + db)
	must(err)
	defer tc.Close()

	branch := func(name string) dtmrs.BranchFunc {
		return func(gid, branchID, op string) int {
			note(fmt.Sprintf("%-16s branch=%s op=%s", name, branchID, op))
			return dtmrs.Success
		}
	}
	for _, n := range []string{
		"stock_confirm", "stock_cancel", "coupon_confirm", "coupon_cancel",
	} {
		must(tc.Register(n, branch(n)))
	}
	must(tc.Start())

	gid := "tcc-1"
	must(tc.TccBegin(gid))

	// 一阶段由调用方自己跑。纪律：先 register 再跑 Try——
	// 反过来的话 Try 冻结的资源 TC 不知道，回滚时没人收尾。
	type step struct {
		id, confirm, cancel, name string
		tryOK                     bool
	}
	steps := []step{
		{"01", "local://stock_confirm", "local://stock_cancel", "库存预留", true},
		{"02", "local://coupon_confirm", "local://coupon_cancel", "优惠券锁定", !shouldFail},
	}

	aborted := false
	for _, st := range steps {
		must(tc.TccRegister(gid, st.id, st.confirm, st.cancel))
		note(fmt.Sprintf("Try %-12s branch=%s -> %v", st.name, st.id, st.tryOK))
		if !st.tryOK {
			fmt.Printf("第 %s 步 Try 失败，Abort\n", st.id)
			must(tc.Abort(gid))
			aborted = true
			break
		}
	}
	if !aborted {
		fmt.Println("全部 Try 成功，Submit")
		must(tc.Submit(gid))
	}

	status, err := tc.WaitFinal(gid, 10000)
	must(err)
	fmt.Printf("\nstatus=%s\n", status)
	mu.Lock()
	for _, l := range trace {
		fmt.Println("   ", l)
	}
	mu.Unlock()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
