package rpc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/rpc"
)

// KEEL_INTERNAL_SECRET_PREVIOUS：轮换期间「当前 + 仍接受的旧密钥」这条路径。
// 不碰库——同一类不需要 RLS 的验签测试，见 server_test.go 顶部的分工说明。

const previousSecret = "fedcba9876543210fedcba9876543210-test"

// rotatingRouter 建一个「当前=secret，仍接受=previousSecret」的内网引擎，
// 挂一条 Signed 回显与一条 Saga 分支，好让请求 HMAC 与分支令牌两条都能测。
func rotatingRouter() (*gin.Engine, rpc.Routes) {
	return rpc.NewRouter(rpc.ServerConfig{Secret: secret, PreviousSecrets: []string{previousSecret}})
}

// 旧密钥仍要能验请求、能验分支令牌——这正是轮换第 2 步（KEEL_INTERNAL_SECRET
// 已经翻到新值、旧值还在 PREVIOUS 里）要保证的：还没来得及重签的调用方、
// 以及已经持久化的旧 SAGA 分支地址，都不能因为这一次轮换突然 401。
func TestPreviousSecretVerifiesRequestsAndBranchTokens(t *testing.T) {
	r, routes := rotatingRouter()
	routes.Signed.POST("/echo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	routes.Saga.POST("/demo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	body := []byte(`{}`)
	req := signedReq(http.MethodPost, "/internal/v1/echo", signOpts{secret: previousSecret, sendBody: body})
	if w := serve(r, req); w.Code != http.StatusOK {
		t.Fatalf("旧密钥签的请求被拒：%d %s", w.Code, w.Body)
	}

	good := "/internal/v1/saga/demo?bt=" + rpc.BranchToken(previousSecret) + "&gid=g&op=action"
	if w := serve(r, httptest.NewRequest(http.MethodPost, good, nil)); w.Code != http.StatusOK {
		t.Fatalf("旧密钥派生的分支令牌被拒：%d %s", w.Code, w.Body)
	}
}

// 既不是当前也不是旧密钥的一律拒绝——PreviousSecrets 只扩大「仍接受」的集合，
// 不是关掉验证。
func TestUnknownSecretRejected(t *testing.T) {
	r, routes := rotatingRouter()
	routes.Signed.POST("/echo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	routes.Saga.POST("/demo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	other := "another-secret-another-secret-xx"
	req := signedReq(http.MethodPost, "/internal/v1/echo", signOpts{secret: other, sendBody: []byte(`{}`)})
	if w := serve(r, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("既非当前也非旧密钥的请求得到 %d，期望 401", w.Code)
	}

	bad := "/internal/v1/saga/demo?bt=" + rpc.BranchToken(other) + "&gid=g&op=action"
	if w := serve(r, httptest.NewRequest(http.MethodPost, bad, nil)); w.Code != http.StatusUnauthorized {
		t.Fatalf("既非当前也非旧密钥派生的分支令牌得到 %d，期望 401", w.Code)
	}
}

// 签名永远只用当前密钥：Client 只认它自己手里的那一个值，不知道、也不需要知道
// 谁是「当前」谁是「旧」。用 previousSecret 建的 Client 能调通，靠的完全是
// 服务端把它配成了 PreviousSecrets——对 Client 来说这个值就是它唯一的密钥。
func TestClientSignsOnlyWithItsOwnSecret(t *testing.T) {
	r, routes := rotatingRouter()
	routes.Signed.POST("/echo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	srv := httptest.NewServer(r)
	defer srv.Close()

	oldClient, err := rpc.NewClient(srv.URL, previousSecret, time.Second)
	if err != nil {
		t.Fatalf("NewClient(旧密钥): %v", err)
	}
	if err := oldClient.PostJSON(context.Background(), "/internal/v1/echo", map[string]any{}, nil); err != nil {
		t.Fatalf("用旧密钥构造的 Client 调用失败（旧密钥应当仍被接受）：%v", err)
	}

	currentClient, err := rpc.NewClient(srv.URL, secret, time.Second)
	if err != nil {
		t.Fatalf("NewClient(当前密钥): %v", err)
	}
	if err := currentClient.PostJSON(context.Background(), "/internal/v1/echo", map[string]any{}, nil); err != nil {
		t.Fatalf("用当前密钥构造的 Client 调用失败：%v", err)
	}
}

func TestParsePreviousSecrets(t *testing.T) {
	if got, err := rpc.ParsePreviousSecrets(""); err != nil || got != nil {
		t.Fatalf("空串应当解出 nil, nil，得到 %v, %v", got, err)
	}
	if got, err := rpc.ParsePreviousSecrets("   "); err != nil || got != nil {
		t.Fatalf("纯空白应当解出 nil, nil，得到 %v, %v", got, err)
	}

	raw := previousSecret + " , " + secret + ",,"
	got, err := rpc.ParsePreviousSecrets(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	want := []string{previousSecret, secret}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("解出 %v，期望 %v（逗号分隔、去空白、跳过空项）", got, want)
	}

	if _, err := rpc.ParsePreviousSecrets("too-short"); err == nil {
		t.Fatal("短于 MinSecretLen 的一项应当报错")
	}
	if _, err := rpc.ParsePreviousSecrets(previousSecret + ",too-short"); err == nil {
		t.Fatal("一组里有一项短于 MinSecretLen 也应当报错，即便另一项合格")
	}
}
