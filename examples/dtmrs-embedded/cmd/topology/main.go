// topology 证明本项目最核心的架构主张：**部署形态是配置，不是重写。**
//
// 同一段事务编排代码跑两遍，唯一的差别是库存分支的地址字符串：
//
//	local  —— 库存分支是进程内的 Go 函数调用
//	remote —— 库存分支是另一个服务的 HTTP 端点
//
// 业务逻辑、编排结构、补偿关系全部一字不改。
//
//	go run ./cmd/topology local
//	go run ./cmd/topology remote
package main

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/keel/examples/dtmrs-embedded/dtmrs"
)

var (
	mu   sync.Mutex
	seen []string
)

func note(s string) { mu.Lock(); seen = append(seen, s); mu.Unlock() }

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "local" && os.Args[1] != "remote") {
		fmt.Fprintln(os.Stderr, "用法: topology local|remote")
		os.Exit(2)
	}
	mode := os.Args[1]
	db := "/tmp/dtmrs_topology_" + mode + ".db"
	os.Remove(db)

	// 一个「远程库存服务」。只有 remote 形态会用到它。
	go serveStock()

	tc, err := dtmrs.Open("sqlite:" + db)
	must(err)
	defer tc.Close()

	inproc := func(name string) dtmrs.BranchFunc {
		return func(gid, branchID, op string) int {
			note(fmt.Sprintf("进程内调用  %-18s branch=%s op=%s", name, branchID, op))
			return dtmrs.Success
		}
	}
	for _, n := range []string{
		"deduct_stock", "deduct_stock_undo", "create_order", "create_order_undo",
	} {
		must(tc.Register(n, inproc(n)))
	}
	must(tc.Start())

	// ================= 两种形态之间唯一的差别 =================
	stockAction, stockCompensate := "local://deduct_stock", "local://deduct_stock_undo"
	if mode == "remote" {
		stockAction = "http://127.0.0.1:18089/stock/deduct"
		stockCompensate = "http://127.0.0.1:18089/stock/undo"
	}
	// =========================================================

	// 以下到函数结尾，两种形态完全一致。
	steps := fmt.Sprintf(
		`[{"action":%q,"compensate":%q},`+
			`{"action":"local://create_order","compensate":"local://create_order_undo"}]`,
		stockAction, stockCompensate)

	must(tc.SubmitSaga("topo-1", steps))
	status, err := tc.WaitFinal("topo-1", 10000)
	must(err)

	fmt.Printf("mode=%-7s status=%s\n", mode, status)
	mu.Lock()
	sort.Strings(seen)
	for _, l := range seen {
		fmt.Println("   ", l)
	}
	mu.Unlock()
}

func serveStock() {
	mux := http.NewServeMux()
	ok := func(tag string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			note(fmt.Sprintf("经由 HTTP   %-18s branch=%s op=%s", tag,
				r.URL.Query().Get("branch_id"), r.URL.Query().Get("op")))
			w.WriteHeader(http.StatusOK)
			// dtmrs 按这个字段判定分支结果
			fmt.Fprint(w, `{"dtm_result":"SUCCESS"}`)
		}
	}
	mux.HandleFunc("/stock/deduct", ok("deduct_stock"))
	mux.HandleFunc("/stock/undo", ok("deduct_stock_undo"))
	srv := &http.Server{Addr: "127.0.0.1:18089", Handler: mux,
		ReadHeaderTimeout: 5 * time.Second}
	_ = srv.ListenAndServe()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
