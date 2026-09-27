package rpc_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/rpc"
	"github.com/keel/keel/internal/tenant"
)

const secret = "0123456789abcdef0123456789abcdef-test"

// echoRouter 建一个内网引擎，挂一条会把收到的正文与租户原样回显的接口。
// 回显正文是为了顺带证明：验签中间件读完正文之后把它放回去了。
func echoRouter(t *testing.T) *gin.Engine {
	t.Helper()
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: secret})
	routes.Signed.POST("/echo", func(c *gin.Context) {
		b, _ := io.ReadAll(c.Request.Body)
		c.String(http.StatusOK, string(b))
	})
	routes.Tenant.GET("/whoami", func(c *gin.Context) {
		id, err := tenant.FromContext(c.Request.Context())
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.String(http.StatusOK, strconv.FormatInt(id, 10))
	})
	return r
}

type signOpts struct {
	secret    string
	ts        time.Time
	merchant  string
	signBody  []byte // 签名用的正文；nil 表示与发送的相同
	sendBody  []byte
	signQuery string // 签名用的 query；"" 表示与发送的相同
}

func signedReq(method, target string, o signOpts) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(o.sendBody))
	if o.secret == "" {
		o.secret = secret
	}
	if o.ts.IsZero() {
		o.ts = time.Now()
	}
	sb := o.signBody
	if sb == nil {
		sb = o.sendBody
	}
	q := o.signQuery
	if q == "" {
		q = req.URL.RawQuery
	}
	ts := strconv.FormatInt(o.ts.Unix(), 10)
	if o.merchant != "" {
		req.Header.Set(rpc.HeaderMerchantID, o.merchant)
	}
	req.Header.Set(rpc.HeaderTimestamp, ts)
	req.Header.Set(rpc.HeaderSignature,
		rpc.Sign(o.secret, method, req.URL.EscapedPath(), q, o.merchant, ts, sb))
	return req
}

func serve(r http.Handler, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 阳性对照排第一：没有它，下面每一条「401」都可能只是因为这条路由根本不通。
func TestSignedRequestIsAccepted(t *testing.T) {
	r := echoRouter(t)
	body := []byte(`{"sku_id":7,"qty":2}`)
	w := serve(r, signedReq(http.MethodPost, "/internal/v1/echo?x=1", signOpts{sendBody: body}))
	if w.Code != http.StatusOK {
		t.Fatalf("合法签名被拒：%d %s", w.Code, w.Body)
	}
	if w.Body.String() != string(body) {
		t.Fatalf("handler 收到的正文是 %q，期望 %q —— 验签读完正文后没放回去", w.Body, body)
	}
}

func TestSignatureRejections(t *testing.T) {
	r := echoRouter(t)
	body := []byte(`{"sku_id":7,"qty":2}`)
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"篡改正文", signedReq(http.MethodPost, "/internal/v1/echo",
			signOpts{sendBody: []byte(`{"sku_id":7,"qty":200}`), signBody: body})},
		{"篡改 query", signedReq(http.MethodPost, "/internal/v1/echo?x=2",
			signOpts{sendBody: body, signQuery: "x=1"})},
		{"密钥不对", signedReq(http.MethodPost, "/internal/v1/echo",
			signOpts{sendBody: body, secret: "another-secret-another-secret-xx"})},
		{"时间戳过期", signedReq(http.MethodPost, "/internal/v1/echo",
			signOpts{sendBody: body, ts: time.Now().Add(-rpc.MaxSkew - time.Minute)})},
		{"时间戳在未来", signedReq(http.MethodPost, "/internal/v1/echo",
			signOpts{sendBody: body, ts: time.Now().Add(rpc.MaxSkew + time.Minute)})},
		{"没有签名头", httptest.NewRequest(http.MethodPost, "/internal/v1/echo", bytes.NewReader(body))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serve(r, tc.req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("期望 401，得到 %d %s", w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("401 的 Content-Type 是 %q，期望 Problem JSON", ct)
			}
		})
	}
}

// 租户头在签名覆盖范围内：截获一个合法请求、只改 X-Keel-Merchant-ID 重放，
// 必须 401。签名若不含租户头，这条会拿到 200 和「另一家店」的身份。
func TestMerchantHeaderIsCoveredBySignature(t *testing.T) {
	r := echoRouter(t)
	req := signedReq(http.MethodGet, "/internal/v1/whoami", signOpts{merchant: "1"})
	if w := serve(r, req); w.Code != http.StatusOK || w.Body.String() != "1" {
		t.Fatalf("阳性对照失败：%d %s", w.Code, w.Body)
	}
	req = signedReq(http.MethodGet, "/internal/v1/whoami", signOpts{merchant: "1"})
	req.Header.Set(rpc.HeaderMerchantID, "2")
	if w := serve(r, req); w.Code != http.StatusUnauthorized {
		t.Fatalf("改了租户头的请求得到 %d %s，期望 401", w.Code, w.Body)
	}
}

func TestTenantRoutesRequireMerchantHeader(t *testing.T) {
	r := echoRouter(t)
	for _, m := range []string{"", "abc", "0", "-3"} {
		w := serve(r, signedReq(http.MethodGet, "/internal/v1/whoami", signOpts{merchant: m}))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("租户头 %q 得到 %d %s，期望 400", m, w.Code, w.Body)
		}
	}
	// 不要求租户的分组不受影响。
	w := serve(r, signedReq(http.MethodPost, "/internal/v1/echo", signOpts{sendBody: []byte("{}")}))
	if w.Code != http.StatusOK {
		t.Fatalf("Signed 分组不该要求租户：%d %s", w.Code, w.Body)
	}
}

func TestSagaRoutesUseBranchToken(t *testing.T) {
	r, routes := rpc.NewRouter(rpc.ServerConfig{Secret: secret})
	routes.Saga.POST("/demo", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	good := "/internal/v1/saga/demo?bt=" + rpc.BranchToken(secret) + "&gid=g&op=action"
	if w := serve(r, httptest.NewRequest(http.MethodPost, good, nil)); w.Code != http.StatusOK {
		t.Fatalf("带正确令牌的分支请求被拒：%d %s", w.Code, w.Body)
	}
	for _, target := range []string{
		"/internal/v1/saga/demo?gid=g",
		"/internal/v1/saga/demo?bt=" + rpc.BranchToken("another-secret-another-secret-xx"),
		// 原始密钥本身不是令牌：令牌是单向派生的。
		"/internal/v1/saga/demo?bt=" + secret,
	} {
		w := serve(r, httptest.NewRequest(http.MethodPost, target, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s 得到 %d，期望 401", target, w.Code)
		}
		// dtmrs 的判定先看正文里有没有 FAILURE / ONGOING，再看状态码。
		// 令牌错时必须落到 Unknown（重试），正文里不能出现这两个词。
		if b := w.Body.String(); strings.Contains(b, "FAILURE") || strings.Contains(b, "ONGOING") {
			t.Fatalf("令牌错误的响应里带了 dtmrs 会误判的词：%s", b)
		}
	}
	if rpc.BranchToken(secret) == rpc.BranchToken(secret+"x") {
		t.Fatal("不同密钥派生出了同一个令牌")
	}
}

func TestProbesBypassAuth(t *testing.T) {
	r := echoRouter(t)
	for _, p := range []string{"/healthz", "/version", "/readyz"} {
		if w := serve(r, httptest.NewRequest(http.MethodGet, p, nil)); w.Code != http.StatusOK {
			t.Fatalf("%s 得到 %d，探针不该经过验签", p, w.Code)
		}
	}
	if w := serve(r, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)); w.Code != http.StatusNotFound {
		t.Fatalf("内网引擎上不该有公网业务路由，/api/v1/products 得到 %d", w.Code)
	}
}
