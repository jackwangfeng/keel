// Package auth 承载买家身份：口令哈希、令牌签发与校验、以及把当前用户放进
// context 的那道中间件。
//
// 它与 internal/tenant 是**两个独立的输入**，这一点是整个包的设计前提：
// 租户来自 Host（或单商家部署的默认商家），用户来自令牌。两者独立，于是天然
// 存在一个攻击面 —— 拿 shop-a 签发的令牌去访问 shop-b。处理方式见 token.go
// 与 middleware.go，那是本包最要紧的两段注释。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ---------------------------------------------------------------------------
// 为什么是 argon2id，以及为什么是这几个参数
// ---------------------------------------------------------------------------
//
// ### 选型：argon2id
//
// 三个候选各自的要害：
//
//   - **bcrypt**：久经考验，Go 侧一个函数就能用完。但它只用 4 KiB 内存，
//     而口令破解早就是显存与 ASIC 的生意 —— 内存占用小意味着一块显卡能同时
//     跑上万路。另有一条更具体的坑：bcrypt **静默截断到 72 字节**，
//     一个用密码管理器生成 80 字符口令的用户，后 8 个字符不参与校验，
//     而这件事不报错、不可观测。
//   - **scrypt**：内存硬，但参数只有一个耦合的 N/r/p 三元组，调起来容易把
//     「更安全」调成「更慢但不更难破」。它也没有一个被广泛实现的编码格式。
//   - **argon2id**：PHC 竞赛的胜者，RFC 9106 的推荐默认。id 变体同时防
//     侧信道（i 的长处）与时空折中攻击（d 的长处）——口令校验发生在
//     一台可能与别人共享的机器上，两个方向都要。
//
// 选 argon2id。代价诚实写出来：golang.org/x/crypto/argon2 只给一个 KDF 函数，
// **编码格式要自己写**（下面那五十行），而 bcrypt 自带。这是本文件里
// 唯一一处「为了更好的算法多写代码」的取舍。
//
// ### 参数：m=19456 KiB, t=2, p=1
//
// 取的是 OWASP 的 Argon2id **最低推荐档**（19 MiB / 2 次迭代 / 1 路并行），
// 不是 RFC 9106 的 m=64 MiB, t=3, p=4。理由是这套部署的形态：
//
//   - 内存是**放大**的：一次登录占 19 MiB，并发 100 次登录就是 1.9 GB。
//     而 /auth/login **今天没有任何限流**（契约里 /auth/sms/code 有 429，
//     login 没有）。换成 64 MiB，同样的并发是 6.4 GB —— 在 README 承诺的
//     「小商家 docker compose up」那台机器上，这是一条真实可达的 OOM 路径，
//     而它同时也是一个不需要任何凭据就能触发的拒绝服务面。
//   - p=1 而不是 4：每次登录只用一核。p=4 在并发登录下抢的是同一批核，
//     吞吐不增反降，而单次延迟的改善对抗离线破解的收益远小于它的账面数字。
//
// 本机实测（AMD64，容器外，单次 HashPassword）：16 ms 与 22 ms 两次。
// 这个量级对登录体感是无感的，而对离线爆破是 19 MiB × 每一次尝试。
//
// **所以真正的前置是限流，不是把参数调高。** 限流到位之前把 m 调到 64 MiB，
// 换来的是「离线破解贵 3.4 倍」与「在线打垮服务便宜 3.4 倍」，不划算。
// 这笔账记在这里：登录限流落地之后，这组参数应当重新评估一次。
//
// ### 参数为什么跟着每一行一起存
//
// PHC 字符串把 m/t/p 写在哈希里，校验时**从存储里读**而不是用本文件的常量。
// 于是将来调高参数不会让任何一个老用户登不进来：老行按老参数校验，
// 新行按新参数写入。反过来（校验时用当前常量）的症状是「改了个常量，
// 全站口令登录集体 401」，而且改的人多半不会预见到。
const (
	argonMemoryKiB = 19456 // 19 MiB
	argonTime      = 2
	argonThreads   = 1
	argonKeyLen    = 32
	argonSaltLen   = 16
)

// ErrPasswordMismatch 是「口令不对」。它与「这个哈希读不懂」分开，
// 因为后者是数据损坏或写入 bug，不该被当成一次普通的登录失败吞掉。
var ErrPasswordMismatch = errors.New("口令不匹配")

// b64 是 PHC 用的无填充 base64。
var b64 = base64.RawStdEncoding

// HashPassword 生成一条 PHC 格式的 argon2id 哈希：
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// 盐每次都新取 16 字节随机数：同一个口令在两行里必须长得不一样，
// 否则「哪些用户用了同一个口令」从库里一眼可见，撞库的性价比会高一个数量级。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("取随机盐失败: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt,
		argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(sum)), nil
}

// VerifyPassword 按 encoded 里记的参数重算并比对。
//
// 比对走 subtle.ConstantTimeCompare：普通的 bytes.Equal 在第一个不同的字节上
// 就返回，逐字节的时间差理论上能被用来一个字节一个字节地试出哈希。
// 这条路径在现实中很难走通（网络抖动远大于这点差异），但代价是一个函数名。
func VerifyPassword(encoded, password string) error {
	mem, iter, par, salt, want, err := decodePHC(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(password), salt, iter, mem, par, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

// dummyHash 是一条真的、但谁也不知道原文的哈希，用来在「手机号不存在」
// 或「这个账号没有设置密码」时**照样跑一遍 KDF**。
//
// 不跑的话，登录接口就是一个手机号枚举器：号码存在时响应慢 24 ms，
// 不存在时立刻返回，两者的差别比任何网络抖动都大。业务上两条路的响应完全一样
// （都是 401），而时间把它们分开了。
//
// 它在 init 里算一次（口令是随机的，进程外没人知道），之后每次调用只是
// 重算一遍 KDF —— 与真实校验同参数、同代价。
var dummyHash string

func init() {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// 取不到随机数是个不该发生的环境故障。这里没有 error 可以返回，
		// 而带着一个空口令继续跑意味着时间侧信道静默失效。
		panic("auth: 初始化口令哈希探针失败: " + err.Error())
	}
	h, err := HashPassword(string(secret))
	if err != nil {
		panic("auth: 初始化口令哈希探针失败: " + err.Error())
	}
	dummyHash = h
}

// VerifyNobody 在「没有可比对的哈希」时烧掉与一次真实校验相同的时间。
// 它永远返回 ErrPasswordMismatch —— 调用点的写法因此和成功路径同形。
func VerifyNobody() error {
	_ = VerifyPassword(dummyHash, "")
	return ErrPasswordMismatch
}

// decodePHC 拆 PHC 字符串。任何一处不认识都返回错误而不是「当作不匹配」：
// 读不懂的哈希是数据损坏或写入 bug，把它降级成一次普通的登录失败，
// 会让一整批用户「密码突然错了」，而日志里一个字都没有。
func decodePHC(s string) (mem uint32, iter uint32, par uint8, salt, sum []byte, err error) {
	parts := strings.Split(s, "$")
	// 开头的 $ 让 Split 产生一个空的第 0 段，所以是 6 段。
	if len(parts) != 6 || parts[0] != "" {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希不是 PHC 格式（%d 段）", len(parts))
	}
	if parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希算法是 %q，本项目只签发 argon2id", parts[1])
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希的版本段读不懂: %q", parts[2])
	}
	if version != argon2.Version {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希的 argon2 版本是 %d，本机的库是 %d",
			version, argon2.Version)
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iter, &par); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希的参数段读不懂: %q", parts[3])
	}
	// 参数从存储里读（见文件头），但不能无条件相信它：一行被改成
	// m=8,t=1,p=1 的哈希会让校验瞬间完成，而那正是把这行改掉的人想要的。
	// 下限就是本文件当前的参数 —— 比它弱的一律拒绝，而不是照着算。
	if mem < argonMemoryKiB || iter < argonTime {
		return 0, 0, 0, nil, nil, fmt.Errorf(
			"口令哈希的参数（m=%d,t=%d）弱于本项目的下限（m=%d,t=%d），拒绝用它校验",
			mem, iter, argonMemoryKiB, argonTime)
	}
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希的盐不是 base64: %w", err)
	}
	if sum, err = b64.DecodeString(parts[5]); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("口令哈希的摘要不是 base64: %w", err)
	}
	if len(salt) == 0 || len(sum) == 0 {
		return 0, 0, 0, nil, nil, errors.New("口令哈希的盐或摘要是空的")
	}
	return mem, iter, par, salt, sum, nil
}
