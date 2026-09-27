package service

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// 口令登录的失败锁定（审查发现：POST /auth/login 没有任何限次，200 次错密码之后
// 正确密码照样立即登录 —— 猜口令没有上限）。
//
// 按「店 + 手机号」计，而不是按 IP：
//   - 部署在反向代理后面、又没配 KEEL_TRUSTED_PROXIES 时，全站访客是同一个 IP，
//     按 IP 锁等于一个人输错五次、全站都登不上；
//   - 猜口令的人换 IP 很便宜，换不了的是他要猜的那个号。
//
// 不存在的手机号**同样计数、同样锁**：只锁存在的号，就等于告诉对方「这个号在这家店有账号」。
//
// 计数在库里（login_failures，00065）。最初放在进程内存里，多实例实测：3 个实例要连错
// 15 次才锁、任何一次重启就全部解锁 —— 所以挪进了 Postgres，所有实例共用一行、重启不丢。
const (
	loginMaxFailures = 5
	loginFailWindow  = 15 * time.Minute
	loginLockFor     = 15 * time.Minute
)

// ErrLoginLocked：这个号最近失败太多次，暂时锁定。RetryAfter 是还要等多久。
type ErrLoginLocked struct{ RetryAfter time.Duration }

func (e *ErrLoginLocked) Error() string {
	return fmt.Sprintf("这个手机号登录失败次数过多，请 %d 分钟后再试", int(e.RetryAfter.Minutes())+1)
}

// EnvLoginLockExempt 是不参与失败锁定的手机号（逗号分隔）。
//
// 只给「一个号公开给所有人用」的演示环境：演示买家的口令写在 README 与种子里，
// 任何访客故意连错五次就能把它锁 15 分钟，而客户端 e2e 与其他访客都靠它登录。
// 豁免的号**不计数、不锁定**；别的号照常锁。正式环境不要配。
const EnvLoginLockExempt = "KEEL_LOGIN_LOCK_EXEMPT_PHONES"

func loginLockExempt(phone string) bool {
	for _, p := range strings.Split(os.Getenv(EnvLoginLockExempt), ",") {
		if p = strings.TrimSpace(p); p != "" && p == phone {
			return true
		}
	}
	return false
}
