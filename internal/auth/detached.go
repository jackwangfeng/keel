package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// 分离签名：给一段**不是令牌**的消息签个名，签名与消息分开放。
//
// ===========================================================================
// 为什么不复用 Issue/Parse 那一套
// ===========================================================================
//
// 那一套签的是 Claims —— 一个带 merchant_id / user_id / sid / exp / kind 的
// 身份载荷，而它的每一个字段在校验时都有含义。第一个用得上分离签名的地方
// （GET /uploads/{upload_id} 跳转过去的那个限时地址，service/upload.go）
// 要签的东西里**一个身份都没有**：它说的是「这个文件在这个时刻之前可以被
// 读」，而不是「持有者是谁」。把它塞进 Claims 要么留一堆零值字段（于是
// Parse 里那些「字段缺失说明签发侧有 bug」的拒绝全都要开口子），要么给
// Kind 再加一个取值（于是一串文件地址在形状上等同于一串会话令牌）。
//
// ===========================================================================
// domain 参数不是装饰
// ===========================================================================
//
// 同一把密钥签两种消息时，若两种消息的字节可能相等，一种的签名就能拿去当
// 另一种用。domain 是前缀隔离：它连同一个不可能出现在消息里的分隔符（\x00）
// 一起进 HMAC，于是 ("upload-blob", "1:2:3") 与 ("something", "else") 的
// 输入永远不可能撞上。它是参数而不是常量，因为第二个用途出现时，作者要被
// 迫回答「我这条消息和已有那条会不会混」。
func (s *Signer) SignDetached(domain, msg string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyDetached 校验一个分离签名。
//
// 比对走 hmac.Equal（常数时间）。用 == 的话，攻击者能按「服务端多久才拒绝」
// 一个字节一个字节地把签名试出来 —— 这条路上尤其真实：文件地址是给浏览器
// 直接打的，攻击者可以随便发多少次，而且没有账号、没有限流锚点。
func (s *Signer) VerifyDetached(domain, msg, sig string) bool {
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(msg))
	return hmac.Equal(want, m.Sum(nil))
}
