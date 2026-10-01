package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 行被别人锁着时，后台写接口撞上 statement_timeout（57014）要回 503 busy + Retry-After，
// 而且**确实没有生效**：放开锁之后拿同一个 Idempotency-Key 重试，是一次真正的首次执行
// （不是幂等回放）—— 第一次那个事务连同幂等键的抢占一起回滚了。
//
// 2026-10 破坏性测试：退款单行被锁时 POST /admin/refunds/{no}/audit 等满 15 秒回裸 500，
// 订单行被锁时 POST /admin/orders/{no}/shipments 同样。这里把语句超时调到 500ms，
// 用一条管理员连接真的 FOR UPDATE 住那一行。
func TestRowLockTimeoutIsBusyAndNotApplied(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "dbbusy")

	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	refundNo := cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1})).RefundNo
	shipNo := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil).OrderNo

	// 被测路由挂在一个 500ms 语句超时的池上。与 cmd/keel 走同一个 app.Router。
	t.Setenv(db.EnvDBStatementTimeout, "500ms")
	pool, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	engine := app.Router(pool,
		tenant.NewResolver(pool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{})

	post := func(path, body, key string) *httptest.ResponseRecorder {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = cs.Host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+cs.Token)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		return w
	}

	// lock 在管理员连接上开一个事务锁住那一行，返回放锁的函数。
	lock := func(sql, arg string) func() {
		t.Helper()
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, db.AdminDSN())
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql, arg); err != nil {
			t.Fatal(err)
		}
		return func() {
			_ = tx.Rollback(ctx)
			_ = conn.Close(ctx)
		}
	}

	cases := []struct {
		name, lockSQL, lockArg, path, body string
		okStatus                           int
	}{
		{"审核退款单（退款单行被锁）", `SELECT 1 FROM refunds WHERE refund_no = $1 FOR UPDATE`, refundNo,
			"/api/v1/admin/refunds/" + refundNo + "/audit", `{"action":"approve"}`, http.StatusOK},
		{"发货（订单行被锁）", `SELECT 1 FROM orders WHERE order_no = $1 FOR UPDATE`, shipNo,
			"/api/v1/admin/orders/" + shipNo + "/shipments",
			`{"carrier_code":"sf","tracking_no":"SF` + uniqueKey() + `"}`, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := freshIdemKey()
			release := lock(c.lockSQL, c.lockArg)
			start := time.Now()
			w := post(c.path, c.body, key)
			release()
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("行被锁时期望 503，实得 %d（%s）", w.Code, w.Body.String())
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("等了 %s 才回：语句超时没按 500ms 生效？", d)
			}
			var p struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.Type != problem.TypeBusy {
				t.Fatalf("type = %q（%v），期望 %s；body=%s", p.Type, err, problem.TypeBusy, w.Body.String())
			}
			if !strings.Contains(p.Detail, "没有生效") {
				t.Errorf("detail 没说「没有生效」：%q", p.Detail)
			}
			if got := w.Header().Get("Retry-After"); got == "" {
				t.Error("busy 没带 Retry-After")
			}

			// 放锁之后同一把钥匙重试：成功，而且不是回放 —— 第一次确实什么都没留下。
			w = post(c.path, c.body, key)
			if w.Code != c.okStatus {
				t.Fatalf("放锁之后重试期望 %d，实得 %d（%s）", c.okStatus, w.Code, w.Body.String())
			}
			if w.Header().Get("Idempotency-Replayed") == "true" {
				t.Fatal("重试拿到的是幂等回放：第一次那个撞了超时的请求其实生效了")
			}
		})
	}
}
