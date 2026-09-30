package app_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
)

// 优雅停机：停机信号（ctx 取消）到来时，在途请求要做完、后台任务要退出、Run 本身要返回。
//
// 走的是真的 app.Serve（真的 http.Server 与 Shutdown），不是 spy —— spy 立刻返回，
// 测不出「Shutdown 有没有等在途请求」。handler 外面包一层只在测试里存在的 /test/slow，
// 它在停机信号发出之后还要再睡一会儿才写响应：Shutdown 不等的话，这个请求会拿到连接错误。
//
// 后台任务那一半的断言靠时间：宽限期设成 60 秒，而要求 Run 在 15 秒内返回。
// 有一个后台任务不守 ctx 的话，停机会在「等后台任务」那一步卡满 60 秒 —— 这条就红。
func TestRunShutsDownGracefully(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvShutdownGrace, "60s")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()

	started := make(chan struct{})
	release := make(chan struct{})
	listening := make(chan struct{})
	listen := func(ctx context.Context, _ string, h http.Handler) error {
		mux := http.NewServeMux()
		mux.Handle("/", h)
		mux.HandleFunc("/test/slow", func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			_, _ = io.WriteString(w, "done")
		})
		close(listening)
		return app.Serve(ctx, ln, mux)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx, listen) }()

	select {
	case <-listening:
	case err := <-runErr:
		t.Fatalf("Run 没走到监听就返回了：%v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run 30 秒内没走到监听")
	}

	// 探针：healthz 只表示进程活着；readyz 要 ping 通库。
	for _, p := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatalf("%s：%v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 得到 %d，期望 200", p, resp.StatusCode)
		}
	}

	type result struct {
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/test/slow")
		if err != nil {
			slow <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		slow <- result{string(b), err}
	}()
	<-started

	// 发停机信号，然后等 Shutdown 真的关掉监听套接字，再放那个在途请求走。
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 200*time.Millisecond)
		if err != nil {
			break // 停止接新连接了
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("停机信号发出 10 秒后还在接新连接")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-runErr:
		t.Fatalf("在途请求还没做完 Run 就返回了（%v）—— Shutdown 没等在途请求", err)
	default:
	}
	close(release)

	if r := <-slow; r.err != nil || r.body != "done" {
		t.Fatalf("在途请求没做完：body=%q err=%v", r.body, r.err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("优雅停机后 Run 应返回 nil，实得 %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("停机信号发出后 15 秒 Run 还没返回（宽限期 60 秒）—— 有后台任务不守 ctx，或收尾顺序卡住了")
	}
}
