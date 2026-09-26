package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/auth"
)

func mid(v int64) *int64 { return &v }

// 阳性对照：两种后台令牌都签得出、验得回。
//
// 它排在所有「必须被拒」的断言前面：没有它，把 IssueStaff 写成恒错
// 也能让下面一大半测试全绿。
func TestStaffTokenRoundTrips(t *testing.T) {
	s := newSigner(t)

	t.Run("商家级", func(t *testing.T) {
		tok, err := s.IssueStaff(mid(7), 42, auth.StaffSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ParseStaff(tok)
		if err != nil {
			t.Fatal(err)
		}
		if got.MerchantID != 7 || got.StaffID != 42 {
			t.Fatalf("解出来是 mid=%d staff=%d，期望 7/42", got.MerchantID, got.StaffID)
		}
		if got.Platform {
			t.Error("商家级令牌被解成了平台级")
		}
	})

	t.Run("平台级", func(t *testing.T) {
		tok, err := s.IssueStaff(nil, 42, auth.StaffSessionTTL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ParseStaff(tok)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Platform {
			t.Fatal("平台级令牌没被解成平台级 —— 那个操作员从此哪个域名都进不去")
		}
		if got.MerchantID != 0 {
			t.Errorf("平台级令牌的 mid 是 %d，期望 0", got.MerchantID)
		}
	})
}

// 租户比对的两支：商家级只认自己那家店，平台级认所有。
//
// 两支必须一起验。只验第一支的话，一个「永远返回 false」的实现会全绿，
// 而平台操作员登不进任何一家店；只验第二支的话，一个「永远返回 true」的
// 实现会全绿，而那就是跨店越权。
func TestStaffTenantMatchHasBothBranches(t *testing.T) {
	s := newSigner(t)

	shopA, err := s.IssueStaff(mid(7), 42, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := s.ParseStaff(shopA)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.TenantMatches(7) {
		t.Error("A 店的令牌在 A 店被拒了")
	}
	if ca.TenantMatches(8) {
		t.Error("A 店的令牌在 B 店通过了 —— 后台 token 能读全部订单与客户手机号")
	}

	platform, err := s.IssueStaff(nil, 42, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := s.ParseStaff(platform)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []int64{7, 8} {
		if !cp.TenantMatches(m) {
			t.Errorf("平台级令牌在 merchant %d 上被拒了 —— 平台级按定义就是跨租户的", m)
		}
	}
}

// 「有租户」与「是平台级」必须恰好成立一个。
//
// 这条验的是签名之后那道形状校验。伪造一串**两者都成立**的令牌（用同一把
// 密钥重新签，所以签名是对的）：如果 ParseStaff 不查这一条，它会解出一个
// mid=7 且 Platform=true 的声明，而 TenantMatches 的平台分支让它在**任何**
// Host 上通过 —— 「A 店的令牌在 B 店能用」换了个说法。
func TestStaffTokenClaimingBothTenantAndPlatformIsRejected(t *testing.T) {
	s := newSigner(t)

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"既有租户又声称平台级", map[string]any{
			"mid": 7, "uid": 42, "typ": "staff", "plt": true,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}},
		{"既没有租户也不是平台级", map[string]any{
			"mid": 0, "uid": 42, "typ": "staff",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := resign(t, tc.payload)
			// 阳性对照：这串东西的**签名是对的**。不然下面那个错误可能只是
			// ErrBadSignature，而这条测试就什么也没验到。
			if _, err := s.ParseStaff(tok); errors.Is(err, auth.ErrBadSignature) {
				t.Fatalf("伪造的令牌签名不对，这条测试没在检查形状校验")
			}
			if _, err := s.ParseStaff(tok); !errors.Is(err, auth.ErrStaffTokenShape) {
				t.Fatalf("期望 ErrStaffTokenShape，实际: %v", err)
			}
		})
	}
}

// 签发侧也拒绝那两种形状 —— 它们连签都不该签得出来。
func TestStaffTokenWithoutAnIdentityCannotBeIssued(t *testing.T) {
	s := newSigner(t)
	if _, err := s.IssueStaff(mid(0), 42, auth.StaffSessionTTL); err == nil {
		t.Error("签出了一串 mid=0 却不是平台级的令牌 —— 它对任何租户都不成立，" +
			"症状是「这个人怎么登录都是 401」")
	}
	if _, err := s.IssueStaff(mid(7), 0, auth.StaffSessionTTL); err == nil {
		t.Error("签出了一串没有 staff_id 的令牌")
	}
}

// 买家令牌与后台令牌不能互相冒充。typ 进签名，所以这条不靠任何额外字段。
func TestBuyerAndStaffTokensDoNotCrossParse(t *testing.T) {
	s := newSigner(t)

	buyer, err := s.Issue(7, 42, 1, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParseStaff(buyer); !errors.Is(err, auth.ErrWrongKind) {
		t.Fatalf("买家令牌被 ParseStaff 接受了（%v）—— staff 与 users 的 id "+
			"来自同一种自增序列，撞上就是读到另一个人的身份", err)
	}

	staff, err := s.IssueStaff(mid(7), 42, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ParseKind(staff, auth.KindAccess); !errors.Is(err, auth.ErrWrongKind) {
		t.Fatalf("后台令牌被当成买家 access_token 接受了（%v）", err)
	}
	// Parse（不核 kind）也必须拒：后台令牌的 plt 字段对买家没有意义，
	// 而一串 plt=true 的令牌走到买家那条路上说明签发侧把两种身份接串了。
	platform, err := s.IssueStaff(nil, 42, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Parse(platform); err == nil {
		t.Fatal("平台级后台令牌被买家的 Parse 接受了")
	}
}

// 过期的后台令牌被拒，而且错误是 ErrTokenExpired 而不是别的。
func TestExpiredStaffTokenIsRejected(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issuer := newSigner(t).WithClock(func() time.Time { return base })
	tok, err := issuer.IssueStaff(mid(7), 42, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 阳性对照：同一串令牌在签发的那一刻是好的。
	if _, err := issuer.ParseStaff(tok); err != nil {
		t.Fatalf("刚签出来就验不过: %v", err)
	}
	late := newSigner(t).WithClock(func() time.Time { return base.Add(time.Hour + time.Second) })
	if _, err := late.ParseStaff(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("期望 ErrTokenExpired，实际: %v", err)
	}
	// 边界：恰好到点也算过期（没有容差）。
	exact := newSigner(t).WithClock(func() time.Time { return base.Add(time.Hour) })
	if _, err := exact.ParseStaff(tok); !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("恰好到期时期望 ErrTokenExpired，实际: %v", err)
	}
}

// 一次性 token 的明文不可预测，而且进库的只有它的 sha256。
func TestOpaqueTokensAreDistinctAndOnlyHashedFormIsStorable(t *testing.T) {
	a, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次取到同一串一次性 token —— 熵源坏了")
	}
	if len(a) < 40 {
		t.Errorf("一次性 token 只有 %d 个字符，太短了 —— 它换得出后台全权", len(a))
	}
	h := auth.HashStaffToken(a)
	if h == a || len(h) != 64 || strings.Contains(h, a) {
		t.Errorf("HashStaffToken 的结果 %q 看上去不是一串 sha256 十六进制", h)
	}
	if auth.HashStaffToken(a) != h {
		t.Error("同一串 token 两次算出不同的 hash")
	}
}

// resign 用签名器的密钥重新签一段自定义载荷，得到一串**签名合法**的令牌。
//
// 它只能这么做是因为测试和被测代码共用同一把密钥。这正是它的用处：
// 把「签名对不对」这个变量固定住，好让断言落在签名之后那几道校验上。
func resign(t *testing.T, payload map[string]any) string {
	t.Helper()
	b64 := base64.RawURLEncoding
	head := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	signing := head + "." + b64.EncodeToString(body)
	// Signer 没有导出 mac，所以这里用**同一把密钥**自己算一次 HMAC。
	// testKey 就是 newSigner 用的那一把，于是签出来的东西签名是合法的 ——
	// 而那正是这个辅助函数存在的理由：把「签名对不对」这个变量固定住，
	// 好让断言落在签名**之后**那几道校验上。
	m := hmac.New(sha256.New, testKey)
	m.Write([]byte(signing))
	return signing + "." + b64.EncodeToString(m.Sum(nil))
}
