package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// 反向代理后面的限流要按真实客户端 IP 计，而裸跑时 X-Forwarded-For 必须一个字都不信。
//
// 两头各有一种静默的坏法：不信代理，全部访客挤进代理那一个桶，一个人刷搜索所有人一起 429；
// 信错了（gin 默认信任全部来源），攻击者每个请求换一个 X-Forwarded-For，限流形同虚设。

// limitedEngine 挂一个每 IP 只放 1 次（不补充）的闸门，proxies 原样交给 trustProxies。
func limitedEngine(t *testing.T, proxies string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := trustProxies(r, proxies); err != nil {
		t.Fatalf("trustProxies(%q): %v", proxies, err)
	}
	r.GET("/x", rateLimitByIP(newIPRateLimiter(0.0001, 1)), func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func hit(r *gin.Engine, remote, xff string) int {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remote + ":40000"
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestRateLimitIgnoresForwardedForWhenNoProxyIsTrusted(t *testing.T) {
	r := limitedEngine(t, "")
	if c := hit(r, "10.0.0.9", "1.1.1.1"); c != http.StatusOK {
		t.Fatalf("第一次应放行，得到 %d", c)
	}
	// 换一个自报的 X-Forwarded-For 不该换来一个新桶。
	if c := hit(r, "10.0.0.9", "2.2.2.2"); c != http.StatusTooManyRequests {
		t.Fatalf("没有受信代理时 X-Forwarded-For 不该被采信，得到 %d", c)
	}
}

func TestRateLimitCountsRealClientsBehindATrustedProxy(t *testing.T) {
	r := limitedEngine(t, "172.16.0.0/12, 127.0.0.1")
	if c := hit(r, "172.18.0.1", "1.1.1.1"); c != http.StatusOK {
		t.Fatalf("访客 A 第一次应放行，得到 %d", c)
	}
	if c := hit(r, "172.18.0.1", "2.2.2.2"); c != http.StatusOK {
		t.Fatalf("同一个代理后面的访客 B 应有自己的桶，得到 %d", c)
	}
	if c := hit(r, "172.18.0.1", "1.1.1.1"); c != http.StatusTooManyRequests {
		t.Fatalf("访客 A 第二次应被限，得到 %d", c)
	}
	// 不在受信名单里的来源，自报的头照样不信。
	if c := hit(r, "203.0.113.5", "3.3.3.3"); c != http.StatusOK {
		t.Fatalf("直连来源第一次应放行，得到 %d", c)
	}
	if c := hit(r, "203.0.113.5", "4.4.4.4"); c != http.StatusTooManyRequests {
		t.Fatalf("非受信来源自报的 X-Forwarded-For 不该被采信，得到 %d", c)
	}
}

func TestTrustProxiesRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "10.0.0.1,,"} {
		if err := trustProxies(gin.New(), bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}
