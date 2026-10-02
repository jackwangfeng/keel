package dtm

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 远程客户端对着**真起的** dtmrs 服务端跑（scripts/fetch-dtmrs.sh 构建到 third_party/dtmrs/bin/dtmrs）。
// 不用假服务端：这一层的价值全在「请求体、错误、状态串和真协调器对得上」，假的只能证明和自己对得上。

const remoteTestToken = "remote-test-token-0123456789"

func dtmrsBin(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(filepath.Dir(file), "..", "..", "third_party", "dtmrs", "bin", "dtmrs")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("没有 dtmrs 服务端二进制（%s）：先跑 scripts/fetch-dtmrs.sh", bin)
	}
	return bin
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startDtmrs 起一个 sqlite 存储的 dtmrs，返回连好的客户端。
func startDtmrs(t *testing.T) *Remote {
	t.Helper()
	addr := freePort(t)
	cmd := exec.Command(dtmrsBin(t))
	cmd.Env = append(os.Environ(),
		"DTMRS_DB=sqlite:"+filepath.Join(t.TempDir(), "dtm.db"),
		"DTMRS_ADDR="+addr,
		"DTMRS_AUTH_TOKEN="+remoteTestToken,
		"RUST_LOG=warn",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dtmrs 15 秒内没起来（%s）", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, err := NewRemote(RemoteConfig{Endpoint: "http://" + addr, Token: remoteTestToken})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type remoteCall struct{ path, gid, branch, op, body string }

// branchServer 是分支那一侧：按路径决定回 200 还是 409，记下每一次调用。
type branchServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []remoteCall
	fail  map[string]bool
}

func newBranchServer(t *testing.T, fail ...string) *branchServer {
	b := &branchServer{fail: map[string]bool{}}
	for _, f := range fail {
		b.fail[f] = true
	}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		b.mu.Lock()
		b.calls = append(b.calls, remoteCall{r.URL.Path, q.Get("gid"), q.Get("branch_id"), q.Get("op"), string(body)})
		fail := b.fail[r.URL.Path]
		b.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"dtm_result":"FAILURE"}`))
			return
		}
		_, _ = w.Write([]byte(`{"dtm_result":"SUCCESS"}`))
	}))
	t.Cleanup(b.Close)
	return b
}

func (b *branchServer) paths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, c := range b.calls {
		out = append(out, c.path)
	}
	return out
}

func (b *branchServer) find(path string) (remoteCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c.path == path {
			return c, true
		}
	}
	return remoteCall{}, false
}

func TestRemoteSagaSucceedsAndCompensates(t *testing.T) {
	r := startDtmrs(t)
	b := newBranchServer(t, "/b")

	gid := fmt.Sprintf("saga-ok-%d", time.Now().UnixNano())
	if err := r.SubmitSagaSteps(gid,
		Step{Action: b.URL + "/a", Compensate: b.URL + "/a_undo", Payload: `{"n":1}`},
	); err != nil {
		t.Fatal(err)
	}
	st, err := r.WaitFinal(gid, 10000)
	if err != nil || st != "succeed" {
		t.Fatalf("成功的 SAGA：%q %v", st, err)
	}
	c, ok := b.find("/a")
	if !ok || c.gid != gid || c.op != "action" || !strings.Contains(c.body, `"n":1`) {
		t.Errorf("分支收到的请求不对：%+v（全部 %v）", c, b.paths())
	}

	gid2 := fmt.Sprintf("saga-fail-%d", time.Now().UnixNano())
	if err := r.SubmitSagaSteps(gid2,
		Step{Action: b.URL + "/a", Compensate: b.URL + "/a_undo"},
		Step{Action: b.URL + "/b", Compensate: b.URL + "/b_undo"},
	); err != nil {
		t.Fatal(err)
	}
	st, err = r.WaitFinal(gid2, 10000)
	if err != nil || st != "failed" {
		t.Fatalf("第二步失败的 SAGA 应当 failed：%q %v", st, err)
	}
	if _, ok := b.find("/a_undo"); !ok {
		t.Errorf("第一步应当被补偿：%v", b.paths())
	}
	if s, err := r.Status(gid2); err != nil || s != "failed" {
		t.Errorf("Status = %q %v", s, err)
	}
	if _, err := r.Status("no-such-gid"); err == nil {
		t.Error("不存在的 gid 应当报错")
	}
}

func TestRemoteMsgTopicFanOutWithPayload(t *testing.T) {
	r := startDtmrs(t)
	b := newBranchServer(t)

	if err := r.Subscribe("stock.zero_crossing", b.URL+"/sub1", "test"); err != nil {
		t.Fatal(err)
	}
	if err := r.Subscribe("stock.zero_crossing", b.URL+"/sub1", "test"); err != nil {
		t.Errorf("重复订阅应当按成功处理：%v", err)
	}
	if err := r.Subscribe("stock.zero_crossing", b.URL+"/sub2", "test"); err != nil {
		t.Fatal(err)
	}

	gid := fmt.Sprintf("msg-%d", time.Now().UnixNano())
	payload := `{"store_id":3,"sku_ids":[11,12]}`
	if err := r.PrepareMsgEx(gid, []string{TopicPrefix + "stock.zero_crossing"}, []string{payload},
		b.URL+"/query", 10, false); err != nil {
		t.Fatal(err)
	}
	if err := r.SubmitMsg(gid); err != nil {
		t.Fatal(err)
	}
	if st, err := r.WaitFinal(gid, 10000); err != nil || st != "succeed" {
		t.Fatalf("消息应当投递完：%q %v（调用 %v）", st, err, b.paths())
	}
	for _, p := range []string{"/sub1", "/sub2"} {
		c, ok := b.find(p)
		if !ok || c.body != payload || c.gid != gid {
			t.Errorf("订阅者 %s 没收到载荷：%+v", p, c)
		}
	}

	// 没有订阅者的主题：不放行时 prepare 报错；放行时照常成功、提交后直接完成。
	gid2 := fmt.Sprintf("msg-empty-%d", time.Now().UnixNano())
	if err := r.PrepareMsgEx(gid2, []string{TopicPrefix + "nobody.listens"}, []string{`{}`}, b.URL+"/query", 10, false); err == nil {
		t.Error("没有订阅者、不放行时 prepare 应当报错")
	}
	gid3 := fmt.Sprintf("msg-allow-%d", time.Now().UnixNano())
	if err := r.PrepareMsgEx(gid3, []string{TopicPrefix + "nobody.listens"}, []string{`{}`}, b.URL+"/query", 10, true); err != nil {
		t.Fatalf("放行时 prepare 应当成功：%v", err)
	}
	if err := r.SubmitMsg(gid3); err != nil {
		t.Fatal(err)
	}
	if st, err := r.WaitFinal(gid3, 10000); err != nil || st != "succeed" {
		t.Errorf("空主题放行后应当直接完成：%q %v", st, err)
	}

	// 作废：prepare 之后 abort，订阅者不该收到。
	gid4 := fmt.Sprintf("msg-abort-%d", time.Now().UnixNano())
	if err := r.PrepareMsgEx(gid4, []string{b.URL + "/never"}, nil, b.URL+"/query", 10, false); err != nil {
		t.Fatal(err)
	}
	if err := r.AbortMsg(gid4); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, ok := b.find("/never"); ok {
		t.Error("作废的消息不该投递")
	}
}

func TestRemoteRejectsBadTokenAndBadConfig(t *testing.T) {
	r := startDtmrs(t)
	bad, err := NewRemote(RemoteConfig{Endpoint: r.base, Token: "wrong-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.SubmitSagaSteps("saga-bad-token", Step{Action: "http://127.0.0.1:1/a"}); err == nil {
		t.Error("令牌不对应当被拒")
	}
	if _, err := NewRemote(RemoteConfig{Endpoint: r.base}); err == nil {
		t.Error("没有令牌应当拒绝构造")
	}
	if _, err := NewRemote(RemoteConfig{Endpoint: "dtmrs:36789", Token: "x"}); err == nil {
		t.Error("不是 http(s) 地址应当拒绝构造")
	}
	if err := r.PrepareMsgEx("m", []string{"http://x/a"}, []string{`{}`, `{}`}, "http://x/q", 10, false); err == nil {
		t.Error("载荷与地址个数不等应当报错")
	}
}

// 在途请求一多，连接要复用，不能每条都新建（默认 Transport 每个 host 只留 2 条空闲连接，
// 拆分形态压测下单 c=32 / 64 时把临时端口耗光，见 remoteTransport 的注释）。
// 8 路并发、每路 50 次查询：新建的连接数应停在并发数附近，而不是随请求数涨到上百。
func TestRemoteReusesConnectionsUnderConcurrency(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"succeed"}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()
	r, err := NewRemote(RemoteConfig{Endpoint: srv.URL, Token: remoteTestToken, MaxInflight: 8})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := r.Status(fmt.Sprintf("g-%d", j)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if conns > 16 {
		t.Fatalf("400 次查询新建了 %d 条连接（应 ≤ 16）：连接没有复用", conns)
	}
}
