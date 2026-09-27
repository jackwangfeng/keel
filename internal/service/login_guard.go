package service

import (
	"fmt"
	"sync"
	"time"
)

// 口令登录的失败锁定（审查发现：POST /auth/login 没有任何限次，200 次错密码之后
// 正确密码照样立即登录 —— 猜口令没有上限）。
//
// 按「店 + 手机号」计，而不是按 IP：
//   - 部署在反向代理后面、又没配 KEEL_TRUSTED_PROXIES 时，全站访客是同一个 IP，
//     按 IP 锁等于一个人输错五次、全站都登不上；
//   - 猜口令的人换 IP 很便宜，换不了的是他要猜的那个号。
// 不存在的手机号**同样计数、同样锁**：只锁存在的号，就等于告诉对方「这个号在这家店有账号」。
//
// 计数在进程内存里：多实例部署时每个实例各有一份（与 /search 限流同一个取舍，部署指南
// 「多实例部署」一节写着）。代价是锁定阈值放宽为 实例数 × loginMaxFailures，仍然有界。
const (
	loginMaxFailures = 5
	loginFailWindow  = 15 * time.Minute
	loginLockFor     = 15 * time.Minute
	// 闲置条目的清理阈值：超过这么多条才顺手扫一次过期的，免得每次都遍历。
	loginGuardSweepAt = 10000
)

// ErrLoginLocked：这个号最近失败太多次，暂时锁定。RetryAfter 是还要等多久。
type ErrLoginLocked struct{ RetryAfter time.Duration }

func (e *ErrLoginLocked) Error() string {
	return fmt.Sprintf("这个手机号登录失败次数过多，请 %d 分钟后再试", int(e.RetryAfter.Minutes())+1)
}

type loginFailures struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

type loginGuard struct {
	mu  sync.Mutex
	m   map[string]*loginFailures
	now func() time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{m: map[string]*loginFailures{}, now: time.Now}
}

func loginGuardKey(merchantID int64, phone string) string {
	return fmt.Sprintf("%d:%s", merchantID, phone)
}

// check 在核对口令**之前**调用：锁定中直接拒绝，连 KDF 都不跑。
func (g *loginGuard) check(key string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.m[key]
	if f == nil {
		return nil
	}
	if now := g.now(); now.Before(f.lockedUntil) {
		return &ErrLoginLocked{RetryAfter: f.lockedUntil.Sub(now)}
	}
	return nil
}

// fail 记一次口令错误；到阈值就锁。
func (g *loginGuard) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if len(g.m) >= loginGuardSweepAt {
		for k, f := range g.m {
			if now.After(f.lockedUntil) && now.Sub(f.windowStart) > loginFailWindow {
				delete(g.m, k)
			}
		}
	}
	f := g.m[key]
	if f == nil || now.Sub(f.windowStart) > loginFailWindow {
		f = &loginFailures{windowStart: now}
		g.m[key] = f
	}
	f.count++
	if f.count >= loginMaxFailures {
		f.lockedUntil = now.Add(loginLockFor)
		f.count = 0
		f.windowStart = now
	}
}

// succeed 登录成功就清零：记住的是「连续」失败，不是历史总数。
func (g *loginGuard) succeed(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, key)
}
