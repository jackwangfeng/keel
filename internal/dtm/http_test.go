package dtm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/rpc"
)

const testSecret = "0123456789abcdef0123456789abcdef-dtm"

// call 是分支被调用的一次记录。
type call struct {
	name, gid, branchID, op, payload string
}

type recorder struct {
	mu    sync.Mutex
	calls []call
}

func (r *recorder) branch(name string, ret int) BranchFuncEx {
	return func(gid, branchID, op, payload string) int {
		r.mu.Lock()
		r.calls = append(r.calls, call{name, gid, branchID, op, payload})
		r.mu.Unlock()
		return ret
	}
}

func (r *recorder) find(name, op string) (call, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.name == name && c.op == op {
			return c, true
		}
	}
	return call{}, false
}

// remote 起一个「库存服务」：真实的内网引擎（rpc.NewRouter，带分支令牌准入），
// 分支经 MountBranches 挂在 Saga 分组上。返回它的解析器 —— 编排方就用它生成地址，
// 于是令牌从生成、经 dtmrs 追加参数、到准入校验，走的是生产上那一整条路。
func remote(t *testing.T, branches map[string]BranchFuncEx) BranchResolver {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: testSecret})
	MountBranches(routes.Saga, branches)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	res, err := NewBranchResolver(srv.URL, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// 一步进程内、一步 HTTP，都带载荷，正向跑完就提交。
//
// 载荷里放了引号、反斜杠与中文：它在 Go → JSON → Rust → 存储 → C → Go（进程内）
// 与 Go → JSON → Rust → 存储 → HTTP 正文 → Go（远端）两条路上都得一个字节不差。
func TestSagaWithLocalAndHTTPBranchCommits(t *testing.T) {
	var rec recorder
	res := remote(t, map[string]BranchFuncEx{
		"stock":      rec.branch("stock", Success),
		"stock_undo": rec.branch("stock_undo", Success),
	})
	local := BranchResolver{}

	tc, err := StartEx(tempDSN(t), 0, nil, map[string]BranchFuncEx{
		"order":      rec.branch("order", Success),
		"order_undo": rec.branch("order_undo", Success),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	gid, err := OrderGID(7, "20260927000001")
	if err != nil {
		t.Fatal(err)
	}
	orderPayload := `{"order_no":"20260927000001","note":"含 \"引号\" 与 \\ 反斜杠"}`
	stockPayload := `{"items":[{"sku_id":11,"qty":2}]}`
	err = tc.SubmitSagaSteps(gid,
		Step{Action: local.BranchURL("order"), Compensate: local.BranchURL("order_undo"), Payload: orderPayload},
		Step{Action: res.BranchURL("stock"), Compensate: res.BranchURL("stock_undo"), Payload: stockPayload},
	)
	if err != nil {
		t.Fatal(err)
	}
	status, err := tc.WaitFinal(gid, 15000)
	if err != nil {
		t.Fatalf("等待终态失败: %v", err)
	}
	if status != "succeed" {
		t.Fatalf("终态 %q，期望 succeed（调用记录 %+v）", status, rec.calls)
	}

	o, ok := rec.find("order", "action")
	if !ok || o.payload != orderPayload || o.gid != gid {
		t.Fatalf("进程内分支收到 %+v，期望 gid=%s payload=%s", o, gid, orderPayload)
	}
	s, ok := rec.find("stock", "action")
	if !ok || s.payload != stockPayload || s.gid != gid || s.branchID == "" {
		t.Fatalf("HTTP 分支收到 %+v，期望 gid=%s payload=%s", s, gid, stockPayload)
	}
	if _, undone := rec.find("order_undo", "compensate"); undone {
		t.Fatal("全部成功却调了补偿")
	}
}

// HTTP 分支明确失败 → 协调器补偿已经做过的进程内分支。
//
// 这条证明 409 + FAILURE 正文被 dtmrs 读成 Failure（而不是 Unknown 无限重试），
// 也证明补偿收到的是同一份载荷（正向与补偿共用）。
func TestHTTPBranchFailureCompensatesLocalBranch(t *testing.T) {
	var rec recorder
	res := remote(t, map[string]BranchFuncEx{
		"stock":      rec.branch("stock", Failure),
		"stock_undo": rec.branch("stock_undo", Success),
	})
	local := BranchResolver{}

	tc, err := StartEx(tempDSN(t), 0, nil, map[string]BranchFuncEx{
		"order":      rec.branch("order", Success),
		"order_undo": rec.branch("order_undo", Success),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()

	gid, err := OrderGID(7, "20260927000002")
	if err != nil {
		t.Fatal(err)
	}
	orderPayload := `{"order_no":"20260927000002"}`
	err = tc.SubmitSagaSteps(gid,
		Step{Action: local.BranchURL("order"), Compensate: local.BranchURL("order_undo"), Payload: orderPayload},
		Step{Action: res.BranchURL("stock"), Compensate: res.BranchURL("stock_undo"), Payload: `{"items":[]}`},
	)
	if err != nil {
		t.Fatal(err)
	}
	status, err := tc.WaitFinal(gid, 15000)
	if err != nil {
		t.Fatalf("等待终态失败: %v", err)
	}
	if status != "failed" {
		t.Fatalf("终态 %q，期望 failed（调用记录 %+v）", status, rec.calls)
	}
	u, ok := rec.find("order_undo", "compensate")
	if !ok {
		t.Fatalf("HTTP 分支失败后没有补偿进程内分支（调用记录 %+v）", rec.calls)
	}
	if u.payload != orderPayload {
		t.Fatalf("补偿收到的载荷是 %q，期望与正向相同 %q", u.payload, orderPayload)
	}
}

// 适配器的回法，逐项对着 dtmrs 的判定规则（先看正文、再看状态码）。
func TestHTTPBranchResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name      string
		fn        BranchFuncEx
		target    string
		code      int
		wantWord  string // 正文里必须有的
		forbidden string // 正文里绝不能有的
	}{
		{"成功", func(_, _, _, _ string) int { return Success },
			"/b?gid=g&branch_id=01&op=action", 200, "SUCCESS", "FAILURE"},
		{"失败", func(_, _, _, _ string) int { return Failure },
			"/b?gid=g&branch_id=01&op=action", 409, "FAILURE", ""},
		{"未知", func(_, _, _, _ string) int { return Unknown },
			"/b?gid=g&branch_id=01&op=action", 500, "", "FAILURE"},
		{"不认识的返回值", func(_, _, _, _ string) int { return 99 },
			"/b?gid=g&branch_id=01&op=action", 500, "", "FAILURE"},
		{"panic", func(_, _, _, _ string) int { panic("boom") },
			"/b?gid=g&branch_id=01&op=action", 500, "", "FAILURE"},
		{"缺参数", func(_, _, _, _ string) int { return Success },
			"/b?gid=g&op=action", 400, "", "FAILURE"},
		{"op 不对", func(_, _, _, _ string) int { return Success },
			"/b?gid=g&branch_id=01&op=prepare", 400, "", "FAILURE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.POST("/b", HTTPBranch(tc.fn))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.target, strings.NewReader("{}")))
			body := w.Body.String()
			if w.Code != tc.code {
				t.Fatalf("状态码 %d，期望 %d（%s）", w.Code, tc.code, body)
			}
			if tc.wantWord != "" && !strings.Contains(body, tc.wantWord) {
				t.Fatalf("正文 %s 里没有 %s", body, tc.wantWord)
			}
			if tc.forbidden != "" && strings.Contains(body, tc.forbidden) {
				t.Fatalf("正文 %s 里出现了 %s —— dtmrs 会把它当成业务失败触发补偿", body, tc.forbidden)
			}
			if strings.Contains(body, "ONGOING") {
				t.Fatalf("正文 %s 里出现了 ONGOING", body)
			}
		})
	}
}

// 空载荷在两条路径上都是 ""：dtmrs 的 HTTP 驱动把空载荷发成 "{}"。
func TestEmptyPayloadIsNormalized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var got string
	r := gin.New()
	r.POST("/b", HTTPBranch(func(_, _, _, p string) int { got = p; return Success }))
	for _, body := range []string{"{}", "", "  "} {
		got = "unset"
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/b?gid=g&branch_id=01&op=action", strings.NewReader(body)))
		if w.Code != 200 || got != "" {
			t.Fatalf("正文 %q：分支收到 %q（%d），期望空串", body, got, w.Code)
		}
	}
	s, err := StepsJSON(Step{Action: "local://a", Payload: "{}"})
	if err != nil || strings.Contains(s, "payload") {
		t.Fatalf("空载荷不该编进步骤：%s %v", s, err)
	}
}

func TestBranchResolver(t *testing.T) {
	var zero BranchResolver
	if zero.Remote() || zero.BranchURL("stock_deduct") != "local://stock_deduct" {
		t.Fatalf("零值应当全部进程内，得到 %s", zero.BranchURL("stock_deduct"))
	}
	local, err := NewBranchResolver("", "")
	if err != nil || local.BranchURL("x") != "local://x" {
		t.Fatalf("没配远端时应当是进程内：%v %s", err, local.BranchURL("x"))
	}

	r, err := NewBranchResolver("http://inventory:8090/", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	want := "http://inventory:8090/internal/v1/saga/stock_deduct?bt=" + rpc.BranchToken(testSecret)
	if !r.Remote() || r.BranchURL("stock_deduct") != want {
		t.Fatalf("远端地址 %s，期望 %s", r.BranchURL("stock_deduct"), want)
	}
	if strings.Contains(r.BranchURL("x"), testSecret) {
		t.Fatal("分支地址里出现了原始密钥 —— 它会被协调器持久化、打进日志")
	}

	for _, bad := range []string{"inventory:8090", "ftp://x", "http://x?a=1"} {
		if _, err := NewBranchResolver(bad, testSecret); err == nil {
			t.Fatalf("%q 应当被拒", bad)
		}
	}
	if _, err := NewBranchResolver("http://inventory:8090", "short"); err == nil {
		t.Fatal("配了远端却没有够长的密钥，应当被拒")
	}
}

func TestStartExRejectsDuplicateNames(t *testing.T) {
	_, err := StartEx(tempDSN(t), 0,
		map[string]BranchFunc{"a": func(_, _, _ string) int { return Success }},
		map[string]BranchFuncEx{"a": func(_, _, _, _ string) int { return Success }})
	if err == nil {
		t.Fatal("两组注册里重名应当拒绝启动")
	}
}
