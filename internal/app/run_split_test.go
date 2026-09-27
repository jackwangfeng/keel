package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
)

// 拆分部署（KEEL_ROLE 等）的启动路径。阶段 0 的承诺是两条：
// 默认形态与今天逐字节相同（上面那组测试照旧全绿就是证据），以及
// 新配置要么按说的起、要么在监听之前带着一句说明拒绝启动。

const internalSecret = "0123456789abcdef0123456789abcdef-app"

// multiSpy 记下 Run 交给 listen 的每一个 (地址, handler)。
//
// 等 want 个服务都到齐了才让 listen 返回：Run 在两个服务里任何一个返回时
// 就返回，第一个立刻返回的话第二个可能还没被调用，测试就看不见它。
type multiSpy struct {
	mu       sync.Mutex
	handlers map[string]http.Handler
	want     int
	all      chan struct{}
}

func newMultiSpy(want int) *multiSpy {
	return &multiSpy{handlers: map[string]http.Handler{}, want: want, all: make(chan struct{})}
}

func (s *multiSpy) listen(addr string, h http.Handler) error {
	s.mu.Lock()
	s.handlers[addr] = h
	if len(s.handlers) == s.want {
		close(s.all)
	}
	s.mu.Unlock()
	select {
	case <-s.all:
	case <-time.After(10 * time.Second):
	}
	return nil
}

func (s *multiSpy) get(addr string) http.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handlers[addr]
}

func status(h http.Handler, path string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w.Code
}

// inventory 只起内网服务：探针在，公网业务路由不在；而且不需要协调器的存储
// （KEEL_DTM_DSN 清空了照样起得来 —— 协调器在 core）。
func TestRunInventoryRoleServesOnlyInternal(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvDTMDSN, "")
	t.Setenv(app.EnvRole, "inventory")
	t.Setenv(app.EnvInternalAddr, "127.0.0.1:18090")
	t.Setenv(app.EnvInternalSecret, internalSecret)

	// 探针要在 listen 里面打：Run 返回时库存池已经关了，那之后 /readyz 必然 503。
	// 反过来这也顺带证明了 /readyz 查的是一个真实的池。
	var addrs []string
	codes := map[string]int{}
	listen := func(addr string, h http.Handler) error {
		addrs = append(addrs, addr)
		for _, p := range []string{"/healthz", "/version", "/readyz", "/api/v1/products"} {
			codes[p] = status(h, p)
		}
		return nil
	}
	if err := app.Run(context.Background(), listen); err != nil {
		t.Fatalf("inventory 形态启动失败：%v", err)
	}
	if len(addrs) != 1 || addrs[0] != "127.0.0.1:18090" {
		t.Fatalf("inventory 形态应当只监听 %s，实际 %v", app.EnvInternalAddr, addrs)
	}
	for _, p := range []string{"/healthz", "/version", "/readyz"} {
		if codes[p] != http.StatusOK {
			t.Fatalf("%s 得到 %d", p, codes[p])
		}
	}
	if codes["/api/v1/products"] != http.StatusNotFound {
		t.Fatalf("inventory 形态上 /api/v1/products 得到 %d，期望 404", codes["/api/v1/products"])
	}
}

// 单体 + KEEL_INTERNAL_ADDR：两个端口都起，公网那个与今天一样。
func TestRunAllRoleWithInternalAddrServesBoth(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvInternalAddr, "127.0.0.1:18091")
	t.Setenv(app.EnvInternalSecret, internalSecret)

	s := newMultiSpy(2)
	if err := app.Run(context.Background(), s.listen); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	pub, in := s.get("127.0.0.1:0"), s.get("127.0.0.1:18091")
	if pub == nil || in == nil {
		t.Fatalf("两个端口没有都监听：public=%v internal=%v", pub != nil, in != nil)
	}
	if c := status(in, "/healthz"); c != http.StatusOK {
		t.Fatalf("内网 /healthz 得到 %d", c)
	}
	// 内网接口不在公网引擎上。
	if c := status(pub, "/internal/v1/anything"); c != http.StatusNotFound {
		t.Fatalf("公网引擎上 /internal/v1 得到 %d，期望 404", c)
	}
	// 未签名的内网请求被拒（没有路由时是 401 还是 404 取决于中间件顺序；
	// 这里只要求它不是 2xx）。
	if c := status(in, "/internal/v1/anything"); c < 400 {
		t.Fatalf("未签名的内网请求得到 %d", c)
	}
}

func TestRunRefusesBadSplitConfig(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"拼错的角色", map[string]string{app.EnvRole: "inventroy"},
			[]string{app.EnvRole, "inventroy"}},
		{"inventory 没有内网地址", map[string]string{app.EnvRole: "inventory", app.EnvInternalSecret: internalSecret},
			[]string{app.EnvInternalAddr}},
		{"内网地址没有密钥", map[string]string{app.EnvInternalAddr: "127.0.0.1:18092"},
			[]string{app.EnvInternalSecret}},
		{"密钥太短", map[string]string{app.EnvInternalAddr: "127.0.0.1:18092", app.EnvInternalSecret: "short"},
			[]string{app.EnvInternalSecret}},
		{"远端地址没有密钥", map[string]string{app.EnvInventoryURL: "http://inventory:8090"},
			[]string{app.EnvInternalSecret}},
		{"远端地址格式不对", map[string]string{app.EnvInventoryURL: "inventory:8090", app.EnvInternalSecret: internalSecret},
			[]string{"inventory:8090"}},
		// 阶段 1a 的决定：core 的库存调用只走远端，没配地址就不起来（见 split.go 的 validate）。
		{"core 没有远端地址", map[string]string{app.EnvRole: "core"},
			[]string{app.EnvInventoryURL}},
		{"inventory 配了远端地址", map[string]string{app.EnvRole: "inventory", app.EnvInternalAddr: "127.0.0.1:18092",
			app.EnvInternalSecret: internalSecret, app.EnvInventoryURL: "http://inventory:8090"},
			[]string{app.EnvInventoryURL}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env(t, "", "example.com")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var s spy
			err := app.Run(context.Background(), s.listen)
			if err == nil {
				t.Fatal("应当拒绝启动")
			}
			if s.called {
				t.Fatal("拒绝启动之前已经开始监听")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("错误信息里没有 %q：%v", w, err)
				}
			}
		})
	}
}

// KEEL_INVENTORY_DSN 指向一个能绕过 RLS 的角色时拒绝启动 —— 库存池挂着同一道 Guard。
func TestRunRefusesRLSBypassingInventoryDSN(t *testing.T) {
	env(t, "", "example.com")
	t.Setenv(app.EnvInventoryDSN, db.AdminDSN())
	var s spy
	err := app.Run(context.Background(), s.listen)
	if err == nil || !strings.Contains(err.Error(), app.EnvInventoryDSN) {
		t.Fatalf("库存 DSN 是超级用户时应当拒绝启动，得到 %v", err)
	}
	if s.called {
		t.Fatal("拒绝启动之前已经开始监听")
	}
}
