// Package rpc 是服务间调用的传输层：内网 gin 引擎（服务端）与签名 HTTP 客户端。
//
// 它是「一套代码，两种部署」（docs/电商系统-微服务拆分方案.md）的阶段 0：
// 单体形态下它什么都不承载（没配 KEEL_INTERNAL_ADDR 就不监听），拆分形态下
// core 经它调 inventory。**它不知道任何业务**：路由由调用方挂到 Routes 的分组上，
// 这里只管三件事 —— 这个请求是不是自己人发的（HMAC 签名）、它替哪个租户说话
// （X-Keel-Merchant-ID → ctx）、以及失败时调用方能不能分清「肯定没做」和「不知道」。
//
// # 为什么是自己签的 HMAC，而不是 mTLS 或者干脆不鉴权
//
//   - 不鉴权：内网端口一旦被错误地映射出去（compose 里多写一行 ports 就够了），
//     任何人都能以任意租户的身份扣、还库存。这类错误不报错，只在被利用时才看得见。
//   - mTLS：要一套证书签发与轮换，而本项目的承诺是 `docker compose up` 一条命令。
//     共享密钥 + HMAC 达到同一个目的（「只有配了同一份密钥的进程能调」），
//     运维成本是一个环境变量。
//
// 签名的明文里有租户头（见 canonical）。这不是多余：只签方法、路径、正文的话，
// 截获一个合法请求、改掉 X-Keel-Merchant-ID 再在 5 分钟窗口内重放，
// 就是一次「以别家店的身份」的调用 —— 而租户正是 RLS 唯一认的东西。
//
// 已知边界：5 分钟窗口内**原样**重放是可能的（没有 nonce 表）。原样重放拿到的是
// 同一个租户、同一个请求体的同一个操作，挡它的是业务层本来就有的幂等
// （SAGA 分支的子事务屏障、下单的幂等键）。为它加一张 nonce 表，等于每个内部
// 调用多一次写库，而那次写库防的是一个已经被幂等挡住的东西。
package rpc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// 环境变量名。它们在 app 里读、在错误信息里出现，所以是常量。
const (
	// EnvInternalAddr 是内网 HTTP 服务的监听地址（如 ":8090"）。空 = 不监听。
	// KEEL_ROLE=inventory 时必须配（那时它是这个进程唯一的端口）。
	EnvInternalAddr = "KEEL_INTERNAL_ADDR"

	// EnvInternalSecret 是服务间共享密钥。只要本进程跑内网服务、或配了远端客户端，
	// 就必须配，而且所有进程同一个值。至少 MinSecretLen 字节。
	EnvInternalSecret = "KEEL_INTERNAL_SECRET"

	// EnvInventoryURL 是库存服务内网地址（如 "http://inventory:8090"）。
	// 空 = 库存在本进程内（单体）。拆分部署用，阶段 1 起生效。
	EnvInventoryURL = "KEEL_INVENTORY_URL"
)

// 请求头。
const (
	HeaderMerchantID = "X-Keel-Merchant-ID"
	HeaderTimestamp  = "X-Keel-Timestamp"
	HeaderSignature  = "X-Keel-Signature"
)

// MinSecretLen 与 KEEL_AUTH_SECRET 同一个下限：HMAC-SHA256 的密钥短于输出长度
// 就是在白扔安全余量，而「一个好记的短口令」正是最常见的配法。
const MinSecretLen = 32

// MaxSkew 是签名时间戳允许的偏差。两边都是自己的进程、都跑 NTP，
// 5 分钟宽到足以吸收时钟漂移与排队，窄到让截获的请求很快作废。
const MaxSkew = 5 * time.Minute

// 两把派生密钥的用途标签。
//
// 签名与分支令牌（见 BranchToken）都从同一个 KEEL_INTERNAL_SECRET 派生，
// 但**必须分开派生**：分支令牌会以明文出现在 SAGA 分支的 URL 里，而那个 URL
// 被协调器持久化进它的存储（trans_branch_op）、打进它的日志。直接拿原始密钥
// 当令牌，读得到协调器日志的人就能签任意内部请求；拿原始密钥当签名密钥、
// 再拿它的某个固定变换当令牌也一样危险。HMAC(secret, 标签) 是单向的：
// 令牌泄露推不出签名密钥，反过来也推不出。
const (
	labelSign        = "keel/internal-sign/v1"
	labelBranchToken = "keel/saga-branch-token/v1"
)

func derive(secret, label string) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(label))
	return m.Sum(nil)
}

// canonical 是被签名的明文。
//
// 各段用换行分隔而不是直接拼接：直接拼的话 path="/a" query="b=1" 与
// path="/ab" query="=1" 会签出同一个值。正文进的是它的 SHA-256 而不是原文，
// 让明文长度与正文无关（正文可能不小，而 HMAC 对长输入没有额外好处）。
//
// query 用的是线上那串原样的 RawQuery，不做排序规范化：两端都是本仓库的代码，
// 中间没有会重排参数的代理；规范化只会多出一处两端要各实现一遍、
// 实现不一致时症状是「偶发 401」的逻辑。
func canonical(method, path, rawQuery, merchant, ts string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		strings.ToUpper(method), path, rawQuery, merchant, ts, hex.EncodeToString(sum[:]),
	}, "\n")
}

// Sign 返回签名（十六进制）。导出是给测试与将来的非 Go 调用方对照用的；
// 业务代码走 Client，不需要自己签。
func Sign(secret, method, path, rawQuery, merchant, ts string, body []byte) string {
	m := hmac.New(sha256.New, derive(secret, labelSign))
	m.Write([]byte(canonical(method, path, rawQuery, merchant, ts, body)))
	return hex.EncodeToString(m.Sum(nil))
}

// verify 校验签名与时间戳。返回的字符串是给日志看的拒绝原因（不回给调用方）。
func verify(secret, method, path, rawQuery, merchant, ts, sig string, body []byte, now time.Time) (bool, string) {
	if ts == "" || sig == "" {
		return false, "缺少签名头"
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false, "时间戳不是整数秒"
	}
	d := now.Sub(time.Unix(sec, 0))
	if d > MaxSkew || d < -MaxSkew {
		return false, "时间戳超出允许偏差"
	}
	want := Sign(secret, method, path, rawQuery, merchant, ts, body)
	// hmac.Equal 是常数时间比较；== 会按第一个不同字节提前返回，
	// 泄露「前几位对了」这件事。
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(sig))) {
		return false, "签名不匹配"
	}
	return true, ""
}

// BranchToken 是 SAGA 分支路由的准入令牌（见 Routes.Saga 的注释）。
// 由 KEEL_INTERNAL_SECRET 单向派生，截 32 个十六进制字符（128 位）。
func BranchToken(secret string) string {
	return hex.EncodeToString(derive(secret, labelBranchToken))[:32]
}
