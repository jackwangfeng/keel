package handler_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 退款审核 / 收货在事务里调沙箱渠道，那一步要读这家店的回调密钥并验签。
//
// 以前那两次读走的是 Repo（池上另一条连接）：一个请求手里攥着事务那一条，又去池上
// 要第二条。池小到与并发数相当时，每个请求都拿着一条、等着另一条，谁也不放 ——
// 池默认 max(4, CPU)，4 个并发审核就能把自己锁死，直到请求超时。
//
// 这条测试把池卡到 2 条，并发 8 个（4 个审核 + 4 个收货），每个请求带 15 秒的截止。
// 修复前它会以一批超时（或 500）红；修复后每个请求都只占一条连接，全部 200 并入账到 40。
func TestConcurrentRefundAuditsDoNotExhaustATinyPool(t *testing.T) {
	cs := newCouponShop(t)
	b := cs.newBuyer(t, "rpool")

	// 夹具在包级 testEngine 上造：4 张待审核的仅退款（未发货），
	// 4 张已审核通过、停在 20 待买家退货的退货退款。
	const n = 4
	var audits, receipts []string
	for i := 0; i < n; i++ {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		_, lines := cs.lines(t, b, o.OrderNo)
		audits = append(audits, cs.mustApply(t, b, o.OrderNo, refundBody(1, [2]int64{lines[cs.DressSKU].Id, 1})).RefundNo)
	}
	for i := 0; i < n; i++ {
		o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
		wantStatus(t, cs.ship(t, o.OrderNo, "sf", "SF"+uniqueKey()), http.StatusCreated, "发货")
		wantStatus(t, orderAction(t, cs.Host, o.OrderNo, "confirm", b.Token, "cf-"+uniqueKey()), http.StatusOK, "确认收货")
		_, lines := cs.lines(t, b, o.OrderNo)
		r := cs.mustApply(t, b, o.OrderNo, refundBody(2, [2]int64{lines[cs.DressSKU].Id, 1}))
		if got := cs.mustApprove(t, r.RefundNo); got.Status != 20 {
			t.Fatalf("退货退款审核通过应到 20，实得 %d", got.Status)
		}
		receipts = append(receipts, r.RefundNo)
	}

	// 被测的这一套路由挂在一个只有 2 条连接的池上。与 cmd/keel 走同一个 app.Router。
	t.Setenv(db.EnvDBMaxConns, "2")
	pool, err := db.NewPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 2 {
		t.Fatalf("池上限没生效：%d", got)
	}
	engine := app.Router(pool,
		tenant.NewResolver(pool, tenant.Config{BaseDomain: baseDomain}), testSigner, testOrders,
		service.PaymentConfig{Sandbox: true}, conceptEmbedder{})

	post := func(path, body string) (int, string) {
		// 截止时间挂在请求上：池自锁时 Acquire 会一直等，没有截止的话这条测试会挂到
		// go test 的全局超时，而那时的报错不会指向这里。
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
		r.Host = cs.Host
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+cs.Token)
		r.Header.Set("Idempotency-Key", freshIdemKey())
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, r)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return -1, "请求超时（池被自己占死了）"
		}
		return w.Code, w.Body.String()
	}

	type result struct {
		refundNo string
		code     int
		body     string
	}
	results := make([]result, 2*n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			no := audits[i]
			code, body := post("/api/v1/admin/refunds/"+no+"/audit", `{"action":"approve"}`)
			results[i] = result{no, code, body}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			no := receipts[i]
			code, body := post("/api/v1/admin/refunds/"+no+"/receipt", "")
			results[n+i] = result{no, code, body}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, r := range results {
		if r.code != http.StatusOK {
			t.Fatalf("退款单 %s：期望 200，实得 %d（%s）", r.refundNo, r.code, r.body)
		}
		// 沙箱渠道在同一个事务里入账：只有 30 → 40 真的走完，才说明事务里那次验签读到了密钥。
		if st := refundStatusOf(t, r.refundNo); st != 40 {
			t.Fatalf("退款单 %s 应由沙箱渠道入账到 40，实得 %d", r.refundNo, st)
		}
	}
}
