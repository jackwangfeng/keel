package auth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/keel/keel/internal/auth"
)

// 正确口令验得过、错误口令验不过。
//
// 两个方向都要断言：只测「正确的过」的话，一个恒真的比对（把
// subtle.ConstantTimeCompare 的结果直接写成 1）照样全绿，而那是本文件
// 要防的第一件事。
func TestPasswordRoundTrip(t *testing.T) {
	const pw = "keel-dev-2026"
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.VerifyPassword(h, pw); err != nil {
		t.Fatalf("正确口令验不过: %v", err)
	}
	if err := auth.VerifyPassword(h, pw+"x"); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("错误口令的 err 是 %v，期望 ErrPasswordMismatch", err)
	}
	if err := auth.VerifyPassword(h, ""); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("空口令居然验过了（err=%v）—— 那意味着一个没设密码的账号"+
			"可以用空串登录", err)
	}
}

// 同一个口令哈希两次，结果必须不同（盐是随机的）。
//
// 相同的话，库里「哪些人用了同一个口令」一眼可见，撞库的性价比高一个数量级。
func TestSamePasswordHashesDifferently(t *testing.T) {
	a, err := auth.HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.HashPassword("same")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("同一个口令两次哈希结果相同 —— 盐没起作用")
	}
	// 但两条都得验得过。
	for _, h := range []string{a, b} {
		if err := auth.VerifyPassword(h, "same"); err != nil {
			t.Fatalf("%s 验不过: %v", h, err)
		}
	}
}

// 哈希串长成 PHC 的样子，参数就是本项目定的那一组。
//
// 断言格式而不只是「能验回来」：参数被悄悄调弱（m=8）时，上面那两条测试
// 一个都不会红 —— 哈希照样自洽，只是再也挡不住离线爆破。
func TestHashCarriesItsParameters(t *testing.T) {
	h, err := auth.HashPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	const want = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(h, want) {
		t.Fatalf("哈希是 %q，期望以 %q 开头 —— 参数变了就要在这里显式改一次，"+
			"而不是悄悄生效", h, want)
	}
}

// 存储里被改弱的参数必须被拒，而不是照着算。
//
// 攻击面具体：一个能改库的人把某一行的 m 改成 8、t 改成 1，那一行的校验
// 就从 19 MiB 降到 8 KiB —— 而参数「跟着每一行走」这个设计恰恰让这件事
// 变得可能。所以读参数的那一步必须带下限。
func TestWeakenedParametersAreRejected(t *testing.T) {
	h, err := auth.HashPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	weak := strings.Replace(h, "m=19456,t=2,p=1", "m=8,t=1,p=1", 1)
	if weak == h {
		t.Fatal("阳性对照失败：参数段没被替换，这条测试改的是别的东西")
	}
	err = auth.VerifyPassword(weak, "x")
	if err == nil {
		t.Fatal("被改弱参数的哈希居然验过了")
	}
	if errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("被改弱参数的哈希报的是「口令不匹配」（%v）—— "+
			"那会把一次数据被改写伪装成一次普通的登录失败", err)
	}
}

// 读不懂的哈希不能被降级成「口令不匹配」。
func TestCorruptHashIsNotAMismatch(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-phc-string",
		"$argon2i$v=19$m=19456,t=2,p=1$YWJj$YWJj",  // 不是 id 变体
		"$argon2id$v=13$m=19456,t=2,p=1$YWJj$YWJj", // 版本不对
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$YWJj",  // 盐不是 base64
	} {
		err := auth.VerifyPassword(bad, "x")
		if err == nil {
			t.Fatalf("%q 居然验过了", bad)
		}
		if errors.Is(err, auth.ErrPasswordMismatch) {
			t.Fatalf("%q 报成了「口令不匹配」—— 数据损坏会伪装成一整批用户"+
				"「密码突然错了」，而日志里一个字都没有", bad)
		}
	}
}

// VerifyNobody 永远失败，而且**真的跑了一遍 KDF**。
//
// 后半句用时间断言会很脆（CI 上的抖动远大于 20 ms 的量级），所以这里只断言
// 前半句，把后半句留给注释与代码。它仍然值得一条测试：把 VerifyNobody 写成
// `return ErrPasswordMismatch` 是一个看上去无害的「优化」，而那会让登录接口
// 重新变成一个按响应时间工作的手机号枚举器。
func TestVerifyNobodyAlwaysFails(t *testing.T) {
	if err := auth.VerifyNobody(); !errors.Is(err, auth.ErrPasswordMismatch) {
		t.Fatalf("VerifyNobody 返回 %v，期望 ErrPasswordMismatch", err)
	}
}
