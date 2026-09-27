package handler_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/problem"
)

// 口令登录的失败锁定（审查发现 200 次错密码之后正确密码照样登录）。
// 用一家新开的店与新建的买家：锁定状态在进程内存里、整个包共用一个路由，
// 拿种子买家来锁会把别的测试一起锁住。
func TestPasswordLoginLocksAPhoneAfterRepeatedFailures(t *testing.T) {
	cs := newCouponShop(t)
	phone := fmt.Sprintf("137%08d", time.Now().UnixNano()%100_000_000)
	hash, err := auth.HashPassword("right-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	adminQueryInt64(t, `INSERT INTO users (merchant_id, phone, nickname, password_hash, status)
	                    VALUES ($1, $2, '锁定测试', $3, 1) RETURNING id`, cs.MerchantID, phone, hash)
	loginBody := func(pw string) string { return `{"phone":"` + phone + `","password":"` + pw + `"}` }

	for i := 1; i <= 5; i++ {
		if w := post(t, cs.Host, "/api/v1/auth/login", loginBody("wrong"), ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错密码应 401，得到 %d %s", i, w.Code, w.Body.String())
		}
	}
	// 第六次即使密码对了也是 429：锁定期间连口令都不核对，否则锁定挡不住「猜中那一下」。
	w := post(t, cs.Host, "/api/v1/auth/login", loginBody("right-pass-1"), "")
	p := problemOf(t, w, http.StatusTooManyRequests)
	if p.Type != problem.TypeRateLimited {
		t.Fatalf("锁定的 Problem type 是 %q，期望 %q", p.Type, problem.TypeRateLimited)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 没有 Retry-After —— 客户端不知道要等多久")
	}

	// 不存在的号同样计数、同样锁：只锁存在的号，等于告诉对方「这个号有账号」。
	ghost := fmt.Sprintf("136%08d", time.Now().UnixNano()%100_000_000)
	for i := 1; i <= 5; i++ {
		post(t, cs.Host, "/api/v1/auth/login", `{"phone":"`+ghost+`","password":"x"}`, "")
	}
	if w := post(t, cs.Host, "/api/v1/auth/login", `{"phone":"`+ghost+`","password":"x"}`, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("不存在的号连错五次之后应同样 429，得到 %d —— 两种号的表现不同就能拿来枚举账号", w.Code)
	}

	// 别的号不受影响（按号锁，不是按 IP 锁）。
	if w := post(t, hostA, "/api/v1/auth/login",
		`{"phone":"`+seedPhone+`","password":"`+seedPassword+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("一个号被锁之后别的号应照常登录，得到 %d %s", w.Code, w.Body.String())
	}
}

// 豁免名单里的号不计数、不锁：演示环境把一个买家号公开给所有人，谁都能故意连错五次把它锁住。
func TestLoginLockExemptPhoneIsNeverLocked(t *testing.T) {
	cs := newCouponShop(t)
	phone := fmt.Sprintf("135%08d", time.Now().UnixNano()%100_000_000)
	hash, err := auth.HashPassword("right-pass-2")
	if err != nil {
		t.Fatal(err)
	}
	adminQueryInt64(t, `INSERT INTO users (merchant_id, phone, nickname, password_hash, status)
	                    VALUES ($1, $2, '豁免测试', $3, 1) RETURNING id`, cs.MerchantID, phone, hash)
	t.Setenv("KEEL_LOGIN_LOCK_EXEMPT_PHONES", "10000000000, "+phone)
	for i := 0; i < 8; i++ {
		post(t, cs.Host, "/api/v1/auth/login", `{"phone":"`+phone+`","password":"wrong"}`, "")
	}
	if w := post(t, cs.Host, "/api/v1/auth/login", `{"phone":"`+phone+`","password":"right-pass-2"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("豁免的号连错 8 次之后正确口令应能登录，得到 %d %s", w.Code, w.Body.String())
	}
}
