package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
)

var testKey = []byte("keel-test-secret-key-32-bytes-long!!")

func newSigner(t *testing.T) *auth.Signer {
	t.Helper()
	return auth.NewSigner(testKey)
}

// 签发出来的令牌能被同一个密钥验回来，而且字段一个不差。
//
// 这是阳性对照：下面每一条「必须被拒」的断言，都依赖「正常情况下它是能过的」。
// 没有这一条的话，把 Parse 写成 `return Claims{}, ErrBadSignature` 也能让
// 所有拒绝类断言全绿。
func TestIssuedTokenRoundTrips(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Issue(7, 42, 99, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Parse(tok)
	if err != nil {
		t.Fatalf("自己签的令牌验不过: %v", err)
	}
	if got.MerchantID != 7 || got.UserID != 42 || got.SessionID != 99 {
		t.Fatalf("声明对不上: %+v", got)
	}
	if got.Kind != auth.KindAccess {
		t.Fatalf("类型是 %q", got.Kind)
	}
}

// 令牌里的租户**不可伪造**：改掉载荷里的 mid，签名就不成立。
//
// 这条是「签名覆盖租户」那个选择的直接落点（token.go 文件头）。它不是
// 「解析出来的 mid 变了」这种弱断言 —— 那样的话一个不验签的实现也能过。
func TestTamperedMerchantIDBreaksSignature(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Issue(7, 42, 99, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["mid"] != float64(7) {
		t.Fatalf("阳性对照失败：载荷里没有 mid=7，实际 %v —— "+
			"字段改名之后这条测试改的就是一个不存在的东西了", payload["mid"])
	}
	payload["mid"] = 8 // 把自己搬到另一家店
	tampered, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tampered) + "." + parts[2]

	if _, err := s.Parse(forged); !errors.Is(err, auth.ErrBadSignature) {
		t.Fatalf("改掉 mid 的令牌居然被接受了（err=%v）—— "+
			"租户信息可伪造，那么「令牌锁死在租户上」整条设计不成立", err)
	}
}

// 过期的令牌必须被拒，而且拒的是 ErrTokenExpired 这一种。
func TestExpiredTokenIsRejected(t *testing.T) {
	base := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	issuer := newSigner(t).WithClock(func() time.Time { return base })
	tok, err := issuer.Issue(7, 42, 99, auth.KindAccess, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 还没到期：能验过（阳性对照）。
	early := newSigner(t).WithClock(func() time.Time { return base.Add(59 * time.Minute) })
	if _, err := early.Parse(tok); err != nil {
		t.Fatalf("有效期内的令牌被拒了: %v", err)
	}

	// 过期一秒：必须拒。
	late := newSigner(t).WithClock(func() time.Time { return base.Add(time.Hour + time.Second) })
	if _, err := late.Parse(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("过期令牌的 err 是 %v，期望 ErrTokenExpired", err)
	}

	// 边界：恰好到期的那一刻也算过期（ExpiresAt 必须严格大于 now）。
	// 「小于等于」和「小于」在这里差的是一个刻度，而把它写成容差
	// （比如宽限 5 分钟）会让 AccessTTL 那段关于「泄露窗口多长」的推理失效。
	exact := newSigner(t).WithClock(func() time.Time { return base.Add(time.Hour) })
	if _, err := exact.Parse(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("恰好到期的令牌 err 是 %v，期望 ErrTokenExpired", err)
	}
}

// 另一个密钥签出来的令牌验不过。
//
// 它同时是「多实例必须共用密钥」那条告警（internal/app 的 EnvAuthSecret）
// 在代码里的证据：两个实例各自随机取密钥，就是这条测试描述的情形。
func TestTokenFromAnotherKeyIsRejected(t *testing.T) {
	other := auth.NewSigner([]byte("another-32-byte-long-secret-key!!!!!"))
	tok, err := other.Issue(7, 42, 99, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newSigner(t).Parse(tok); !errors.Is(err, auth.ErrBadSignature) {
		t.Fatalf("别的密钥签的令牌被接受了（err=%v）", err)
	}
}

// refresh_token 不能当 access_token 用。
//
// 两者只差一个 typ 和一个有效期，而 refresh 的寿命是 access 的 360 倍。
// 没有这条校验的话，一串泄露的 refresh_token 就是一把三十天的万能钥匙。
func TestRefreshTokenIsNotAnAccessToken(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Issue(7, 42, 0, auth.KindRefresh, auth.RefreshTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParseKind(tok, auth.KindAccess); !errors.Is(err, auth.ErrWrongKind) {
		t.Fatalf("refresh 令牌当 access 用时 err 是 %v，期望 ErrWrongKind", err)
	}
	if _, err := s.ParseKind(tok, auth.KindRefresh); err != nil {
		t.Fatalf("阳性对照失败：refresh 令牌当 refresh 用也被拒了: %v", err)
	}
}

// alg=none 这一类历史漏洞：算法由服务端写死，令牌里的 alg 只是个必须相等的常量。
func TestAlgNoneIsRejected(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(
		`{"mid":7,"uid":42,"sid":1,"typ":"access","iat":1,"exp":99999999999}`))
	for _, tok := range []string{
		header + "." + payload + ".",   // 空签名
		header + "." + payload + ".Cg", // 随手一段签名
		header + "." + payload,         // 两段
	} {
		if _, err := newSigner(t).Parse(tok); err == nil {
			t.Fatalf("alg=none 的令牌被接受了: %q", tok)
		}
	}
}

// 没有租户的令牌**签不出来**。
//
// 拦在签发侧而不是校验侧：一串 mid=0 的令牌一旦存在，它在每一家店的
// 「claims.MerchantID != merchantID」比较里都是不相等的 —— 今天没事。
// 但只要有一处比较写成了「零值放行」，它就成了一把万能钥匙。不签出来最省心。
func TestTokenWithoutTenantCannotBeIssued(t *testing.T) {
	if _, err := newSigner(t).Issue(0, 42, 1, auth.KindAccess, auth.AccessTTL); err == nil {
		t.Fatal("签出了一串没有租户的令牌")
	}
}

// 同一秒内为同一个用户签两串 refresh_token，必须得到两串不同的东西。
//
// 它们的 hash 是 user_tokens 的唯一索引（uk_user_tokens_hash）。相同的话，
// 「同一秒内登录两次」会撞唯一约束变成 500 —— 一个本地几乎复现不出来的故障。
func TestTokensIssuedInTheSameSecondDiffer(t *testing.T) {
	fixed := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	s := newSigner(t).WithClock(func() time.Time { return fixed })
	a, err := s.Issue(7, 42, 0, auth.KindRefresh, auth.RefreshTTL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Issue(7, 42, 0, auth.KindRefresh, auth.RefreshTTL)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("同一秒签出的两串 refresh_token 完全相同 —— " +
			"它们的 sha256 会撞 uk_user_tokens_hash，第二次登录变成 500")
	}
}
