package rpc_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/outcome"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/tenant"
	"github.com/keel/keel/internal/traceid"
)

func merchantID(t *testing.T, code string) int64 {
	t.Helper()
	id, err := tenant.NewResolver(pool, tenant.Config{}).ByCodeForPlatform(context.Background(), code)
	if err != nil {
		t.Fatalf("找不到种子商家 %s: %v", code, err)
	}
	return id
}

// categoryIDs 在 ctx 的租户下查可见类目 id（升序）。不收 t：它也跑在
// httptest 服务端的 goroutine 里，那里不能 t.Fatal。
func categoryIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	err := repository.New(pool).WithTenant(ctx, func(tx repository.Tx) error {
		nodes, err := tx.ListVisibleCategories(ctx)
		for _, n := range nodes {
			ids = append(ids, n.ID)
		}
		return err
	})
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, err
}

// 租户经内网请求透传之后，repository.WithTenant 与 RLS 一行不改就能工作。
//
// 端到端：Client 从 ctx 取租户 → 头 → 签名 → 服务端 RequireTenant → ctx →
// WithTenant → set_config → RLS。断言的是**行**而不是「handler 看见了一个数字」：
// 拿同一家店在进程内直接查出来的类目 id 作标准答案，内网那一路必须逐个相同，
// 而另一家店的结果必须与它不相交 —— 相交说明 RLS 没起作用（或者租户串了）。
func TestTenantPropagatesToRLS(t *testing.T) {
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: secret, Ready: pool.Ping})
	routes.Tenant.GET("/categories", func(c *gin.Context) {
		ids, err := categoryIDs(c.Request.Context())
		if err != nil {
			problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, err.Error())
			return
		}
		c.JSON(http.StatusOK, ids)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()
	cl, err := rpc.NewClient(srv.URL, secret, 0)
	if err != nil {
		t.Fatal(err)
	}

	a, b := merchantID(t, "shop-a"), merchantID(t, "shop-b")
	ctxA, ctxB := tenant.NewContext(context.Background(), a), tenant.NewContext(context.Background(), b)
	wantA, err := categoryIDs(ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if len(wantA) == 0 {
		t.Fatal("种子里 shop-a 没有可见类目，这条测试分不出 RLS 有没有起作用")
	}

	var gotA, gotB []int64
	if err := cl.GetJSON(ctxA, "/internal/v1/categories", nil, &gotA); err != nil {
		t.Fatal(err)
	}
	if err := cl.GetJSON(ctxB, "/internal/v1/categories", nil, &gotB); err != nil {
		t.Fatal(err)
	}
	if !equal(gotA, wantA) {
		t.Fatalf("内网那一路查到 %v，进程内直接查是 %v", gotA, wantA)
	}
	seen := map[int64]bool{}
	for _, id := range gotA {
		seen[id] = true
	}
	for _, id := range gotB {
		if seen[id] {
			t.Fatalf("shop-b 经内网看见了 shop-a 的类目 %d —— RLS 没生效或租户串了", id)
		}
	}

	// 没有租户的 ctx：客户端不猜，服务端 400。
	err = cl.GetJSON(context.Background(), "/internal/v1/categories", nil, &gotA)
	if !errors.Is(err, rpc.ErrBadRequest) || rpc.IsUnknown(err) {
		t.Fatalf("没有租户时期望 ErrBadRequest，得到 %v", err)
	}

	// readyz 走的是传进去的 Ready（这里是真实的池）。
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz 得到 %d", resp.StatusCode)
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 内网调用把调用方 context 里的跟踪号带到库存服务。没带就不生成，由对面自己发一个。
func TestClientForwardsTraceID(t *testing.T) {
	const want = "0123456789abcdef0123456789abcdef"
	var got string
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: secret})
	routes.Signed.POST("/echo-trace", func(c *gin.Context) {
		got = traceid.From(c.Request.Context())
		c.Status(http.StatusNoContent)
	})
	srv := httptest.NewServer(r)
	defer srv.Close()
	cl, err := rpc.NewClient(srv.URL, secret, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := traceid.With(context.Background(), want)
	if err := cl.PostJSON(ctx, "/internal/v1/echo-trace", map[string]int{}, nil); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("库存服务看见的跟踪号是 %q，期望 %s", got, want)
	}
}

// 错误映射：调用方必须分得清「肯定没做」和「不知道」。
func TestClientErrorMapping(t *testing.T) {
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: secret})
	g := routes.Signed
	g.POST("/ok", func(c *gin.Context) {
		var in map[string]int
		_ = c.ShouldBindJSON(&in)
		c.JSON(http.StatusOK, gin.H{"double": in["n"] * 2})
	})
	g.GET("/q", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"a": c.Query("a")}) })
	g.GET("/404", func(c *gin.Context) {
		problem.Write(c, http.StatusNotFound, problem.TypeNotFound, "不存在")
	})
	g.POST("/409", func(c *gin.Context) {
		problem.Write(c, http.StatusConflict, problem.TypeInsufficientStock, "库存不足")
	})
	g.POST("/422", func(c *gin.Context) {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "不合法")
	})
	g.POST("/500", func(c *gin.Context) {
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "内部错误")
	})
	g.POST("/busy", func(c *gin.Context) { problem.WriteBusy(c) })
	g.POST("/503", func(c *gin.Context) {
		problem.Write(c, http.StatusServiceUnavailable, problem.TypeInternal, "服务未就绪")
	})
	g.POST("/garbage", func(c *gin.Context) { c.String(http.StatusOK, "not json") })
	g.POST("/slow", func(c *gin.Context) { time.Sleep(500 * time.Millisecond); c.JSON(http.StatusOK, gin.H{}) })
	srv := httptest.NewServer(r)
	defer srv.Close()

	cl, err := rpc.NewClient(srv.URL, secret, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var out struct {
		Double int `json:"double"`
	}
	if err := cl.PostJSON(ctx, "/internal/v1/ok", map[string]int{"n": 21}, &out); err != nil || out.Double != 42 {
		t.Fatalf("阳性对照失败：%v %+v", err, out)
	}
	var q struct{ A string }
	if err := cl.GetJSON(ctx, "/internal/v1/q", url.Values{"a": {"x y&z"}}, &q); err != nil || q.A != "x y&z" {
		t.Fatalf("带 query 的 GET 失败（签名没覆盖到转义后的 query？）：%v %+v", err, q)
	}

	cases := []struct {
		path    string
		kind    error
		unknown bool
		ptype   string
	}{
		{"/internal/v1/404", rpc.ErrNotFound, false, problem.TypeNotFound},
		{"/internal/v1/409", rpc.ErrConflict, false, problem.TypeInsufficientStock},
		{"/internal/v1/422", rpc.ErrUnprocessable, false, problem.TypeInvalidRequest},
		{"/internal/v1/500", rpc.ErrUnknown, true, problem.TypeInternal},
		// busy：对面的事务确定回滚了 —— 确定失败，不是结果未知；别的 503 仍是结果未知。
		{"/internal/v1/busy", rpc.ErrBusy, false, problem.TypeBusy},
		{"/internal/v1/503", rpc.ErrUnknown, true, problem.TypeInternal},
		{"/internal/v1/garbage", rpc.ErrUnknown, true, ""},
		{"/internal/v1/slow", rpc.ErrUnknown, true, ""},
		{"/internal/v1/nope", rpc.ErrNotFound, false, problem.TypeNotFound},
	}
	for _, tc := range cases {
		var err error
		if tc.path == "/internal/v1/404" {
			err = cl.GetJSON(ctx, tc.path, nil, nil)
		} else {
			// out 非 nil：/garbage 那条要的正是「2xx 却解不出响应」。
			var sink map[string]any
			err = cl.PostJSON(ctx, tc.path, map[string]int{}, &sink)
		}
		if !errors.Is(err, tc.kind) {
			t.Fatalf("%s：期望 %v，得到 %v", tc.path, tc.kind, err)
		}
		if rpc.IsUnknown(err) != tc.unknown {
			t.Fatalf("%s：IsUnknown=%v，期望 %v", tc.path, rpc.IsUnknown(err), tc.unknown)
		}
		var e *rpc.Error
		if !errors.As(err, &e) || e.Type != tc.ptype {
			t.Fatalf("%s：problem type 期望 %q，得到 %+v", tc.path, tc.ptype, e)
		}
	}

	// busy 的链上带着 outcome.ErrPeerBusy：core 的公网兜底认的是它。
	if err := cl.PostJSON(ctx, "/internal/v1/busy", map[string]int{}, nil); !outcome.IsDBBusy(err) {
		t.Fatalf("busy 的错误链上没有 outcome.ErrPeerBusy：%v", err)
	}

	// 写调用给请求记「落地」：成功与结果未知记，确定失败（4xx、busy）不记。
	// 记错了的后果：记少了，core 后一步撞超时会对一个库存已经写进去的请求说「没有生效」。
	for _, tc := range []struct {
		path    string
		durable bool
	}{
		{"/internal/v1/ok", true},
		{"/internal/v1/500", true},
		{"/internal/v1/busy", false},
		{"/internal/v1/409", false},
	} {
		rctx := outcome.Track(ctx)
		_ = cl.PostJSON(rctx, tc.path, map[string]int{}, nil)
		if got := outcome.MaybeDurable(rctx); got != tc.durable {
			t.Errorf("%s 之后 MaybeDurable = %v，期望 %v", tc.path, got, tc.durable)
		}
	}

	// 密钥不一致：对面明确拒绝，确定失败。
	bad, _ := rpc.NewClient(srv.URL, "another-secret-another-secret-xx", 0)
	if err := bad.PostJSON(ctx, "/internal/v1/ok", map[string]int{}, nil); !errors.Is(err, rpc.ErrUnauthorized) || rpc.IsUnknown(err) {
		t.Fatalf("错误密钥期望 ErrUnauthorized，得到 %v", err)
	}

	// 连不上：结果未知。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	down, _ := rpc.NewClient(dead, secret, 0)
	if err := down.PostJSON(ctx, "/internal/v1/ok", map[string]int{}, nil); !rpc.IsUnknown(err) {
		t.Fatalf("连不上时期望 ErrUnknown，得到 %v", err)
	}
}

func TestNewClientValidates(t *testing.T) {
	if _, err := rpc.NewClient("http://x:1", "short", 0); err == nil {
		t.Fatal("短密钥应当被拒")
	}
	for _, u := range []string{"", "inventory:8090", "ftp://x"} {
		if _, err := rpc.NewClient(u, secret, 0); err == nil {
			t.Fatalf("地址 %q 应当被拒", u)
		}
	}
}
