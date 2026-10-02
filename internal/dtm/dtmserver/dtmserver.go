// Package dtmserver 是测试里真起一个**独立部署的** dtmrs 协调器（微服务形态）的夹具。
//
// 只给 _test.go 用。二进制由 scripts/fetch-dtmrs.sh 构建到 third_party/dtmrs/bin/dtmrs（与嵌入式的
// libdtmrs 同一个 tag）。不用假的协调器：要验的正是「请求体、回调形状、主题展开、状态串和真协调器对得上」。
package dtmserver

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/keel/keel/internal/dtm"
)

// Token 是夹具起的协调器的 DTMRS_AUTH_TOKEN。
const Token = "dtmserver-test-token-0123456789"

// Bin 返回 dtmrs 服务端二进制的路径；没有就让测试失败（不跳过：缺它说明环境没按 CONTRIBUTING 装好）。
func Bin(t testing.TB) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	bin := filepath.Join(filepath.Dir(file), "..", "..", "..", "third_party", "dtmrs", "bin", "dtmrs")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("没有 dtmrs 服务端二进制（%s）：先跑 scripts/fetch-dtmrs.sh", bin)
	}
	return bin
}

// Start 起一个 sqlite 存储的 dtmrs（测试结束时杀掉），返回连好的客户端与它的地址。
func Start(t testing.TB) (*dtm.Remote, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cmd := exec.Command(Bin(t))
	cmd.Env = append(os.Environ(),
		"DTMRS_DB=sqlite:"+filepath.Join(t.TempDir(), "dtm.db"),
		"DTMRS_ADDR="+addr,
		"DTMRS_AUTH_TOKEN="+Token,
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
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dtmrs 15 秒内没起来（%s）", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, err := dtm.NewRemote(dtm.RemoteConfig{Endpoint: "http://" + addr, Token: Token})
	if err != nil {
		t.Fatal(err)
	}
	return r, "http://" + addr
}
