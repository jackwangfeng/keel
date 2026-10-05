package traceid

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizeRejectsAnythingBut32Hex(t *testing.T) {
	if got := Normalize("abc"); got != "" {
		t.Fatalf("短的不该收下：%q", got)
	}
	if got := Normalize("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("大写应折成小写，得到 %q", got)
	}
	if got := Normalize("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); got != "" {
		t.Fatalf("非十六进制不该收下：%q", got)
	}
	if got := Normalize("0123456789abcdef0123456789abcdef\n"); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("末尾换行应被去掉后收下，得到 %q", got)
	}
	if got := Normalize("0123456789abcdef\n0123456789abcdef"); got != "" {
		t.Fatalf("中间换行不该收下：%q", got)
	}
}

func TestIncomingPrefersHeaderThenTraceparent(t *testing.T) {
	want := "0123456789abcdef0123456789abcdef"
	if got := Incoming(want, "00-ffffffffffffffffffffffffffffffff-0123456789abcdef-01"); got != want {
		t.Fatalf("X-Trace-Id 优先，得到 %s", got)
	}
	parent := "00-" + want + "-0123456789abcdef-01"
	if got := Incoming("", parent); got != want {
		t.Fatalf("traceparent 的 trace-id 应被收下，得到 %s", got)
	}
	if got := Incoming("nope", "not-a-parent"); len(got) != 32 {
		t.Fatalf("都不合法时应新生成 32 位，得到 %q", got)
	}
}

func TestAppendLeavesLocalAndExistingAlone(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	if got := Append("local://stock_deduct", id); got != "local://stock_deduct" {
		t.Fatalf("进程内地址不该改：%s", got)
	}
	raw := "http://inventory:8090/internal/v1/saga/stock_deduct?bt=abc"
	got := Append(raw, id)
	if got != raw+"&trace="+id {
		t.Fatalf("应原样接在已有 query 后面，得到 %s", got)
	}
	if again := Append(got, id); again != got {
		t.Fatalf("重复追加：%s", again)
	}
}

func TestHoldLetsAnotherGoroutineSeeTheID(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	ctx := With(context.Background(), id)
	unbind := Hold(ctx, "order-1-n")
	if got := From(Context(context.Background(), "order-1-n")); got != id {
		t.Fatalf("分支侧应看见 %s，得到 %q", id, got)
	}
	// 分支调用自己再挂一次，不应在返回时把提交方的记录撤掉。
	release := Adopt("order-1-n", id)
	release()
	if got := From(Context(context.Background(), "order-1-n")); got != id {
		t.Fatalf("Adopt 不应撤掉 Hold 的记录，得到 %q", got)
	}
	unbind()
	if got := From(Context(context.Background(), "order-1-n")); got != "" {
		t.Fatalf("Hold 结束应撤掉，得到 %q", got)
	}
}

func TestMiddlewareEchoesOrGenerates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, From(c.Request.Context()))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	want := "0123456789abcdef0123456789abcdef"
	req.Header.Set(Header, want)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get(Header) != want || w.Body.String() != want {
		t.Fatalf("沿用调用方的号：头 %q 正文 %q", w.Header().Get(Header), w.Body.String())
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := w.Header().Get(Header); len(got) != 32 || w.Body.String() != got {
		t.Fatalf("没带时应生成并回写：头 %q 正文 %q", got, w.Body.String())
	}
}

func TestLogHandlerAddsTraceID(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(Wrap(slog.NewTextHandler(&buf, nil)))
	ctx := With(context.Background(), "0123456789abcdef0123456789abcdef")
	log.InfoContext(ctx, "hello")
	if !strings.Contains(buf.String(), "trace_id=0123456789abcdef0123456789abcdef") {
		t.Fatalf("日志里没有跟踪号：%s", buf.String())
	}
}
