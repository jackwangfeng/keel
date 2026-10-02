package handler_test

// 第二期上 Shopify 之前的两件前置事：限流不计失败次数、适配器错误写库前脱敏凭据。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/channel"
	"github.com/keel/keel/internal/tenant"
)

// 限流 25 次（超过推送任务的 max_attempts = 20）：任务不进死信，限流解除后推上去。
func TestChannelPushRateLimitDoesNotDeadLetter(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	activeFakeBinding(t, rig, cs)

	rig.fake.FailNextWith(25, &channel.RetryableError{RateLimited: true, After: time.Millisecond, Err: errors.New("throttled")})
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	deadline := time.Now().Add(60 * time.Second)
	for rig.fake.FailuresLeft() > 0 {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		// 限流的退避下限是 1 秒：把 run_after 拨过去，免得测试等 25 秒。
		adminExec(t, `UPDATE jobs SET run_after = now() - interval '1 second'
		               WHERE merchant_id = $1 AND queue = 'channel.listing.push' AND status = 0`, cs.MerchantID)
		if n := adminQueryInt64(t, `SELECT count(*) FROM jobs WHERE merchant_id = $1 AND queue = 'channel.listing.push' AND status = 3`, cs.MerchantID); n != 0 {
			t.Fatalf("限流期间有 %d 条推送任务进了死信", n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 60 秒限流还剩 %d 次没用掉", rig.fake.FailuresLeft())
		}
	}
	rig.waitPushed(t, ctx, cs.NorthStore, cs.DressSKU, availOf(t, cs), "限流解除之后")
	if n := adminQueryInt64(t, `SELECT max(attempts) FROM jobs WHERE merchant_id = $1 AND queue = 'channel.listing.push'`, cs.MerchantID); n > 2 {
		t.Fatalf("限流计进了失败次数：attempts 最大 %d", n)
	}
}

// 适配器的错误里带了凭据：channel_listings.last_error 与 jobs.last_error 都不能有。
func TestChannelAdapterErrorsAreRedacted(t *testing.T) {
	cs := newCouponShop(t)
	rig := newChannelRig(t)
	ctx := tenant.NewContext(context.Background(), cs.MerchantID)
	b := activeFakeBinding(t, rig, cs)
	const secret = "super-secret-value-123"
	if err := rig.svc.SetSecrets(ctx, b.ID, []byte(`{"webhook_secret":"`+secret+`"}`)); err != nil {
		t.Fatal(err)
	}
	rig.fake.FailNextWith(1, errors.New("平台说 client_secret="+secret+" 不对"))
	adjust(t, rig.local, cs.MerchantID, cs.NorthStore, cs.DressSKU, -1)
	// stock.changed 经协调器异步到达：等那一次编排的失败真被用掉。
	deadline := time.Now().Add(20 * time.Second)
	for rig.fake.FailuresLeft() > 0 {
		if err := rig.svc.Drain(ctx); err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("等了 20 秒编排的失败还没被用掉")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var lastErr string
	if err := testPool.QueryRow(context.Background(), `SELECT coalesce(string_agg(last_error, '|'), '') FROM jobs
		WHERE merchant_id = $1 AND queue = 'channel.listing.push'`, cs.MerchantID).Scan(&lastErr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastErr, "***") {
		t.Fatalf("jobs.last_error = %q，期望留下脱敏后的错误", lastErr)
	}
	if strings.Contains(lastErr, secret) {
		t.Fatalf("jobs.last_error 里有凭据：%q", lastErr)
	}
	rig.waitPushed(t, ctx, cs.NorthStore, cs.DressSKU, availOf(t, cs), "失败重试之后")
}
