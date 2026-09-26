package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// 后台令牌：和买家那一套同形，差别只有两处，而两处都是必要的
// ---------------------------------------------------------------------------
//
// 同形的部分照抄 token.go，不另发明：三段 base64、HS256、typ 进签名、
// 先验签再读载荷、过期无容差。理由是那些性质每一条都有代价换来的论证，
// 重写一遍只会得到一份慢慢跑偏的复制品。
//
// 两处差别：
//
// ### 一、租户可以「不是任何一家店」
//
// 数据模型 §14 的两级身份：merchant_id 非空 = 属于某个商家；
// **NULL = 平台级操作员**，他能建商家、做跨租户运维。于是「令牌锁死在租户上」
// 这条规矩在这里要分两支说：
//
//   - 商家级令牌：与买家那条一字不差 —— mid 进签名，中间件比对
//     「令牌里的租户 == 本请求的租户」，不等就是 401，而且拒绝发生在任何一次
//     查库之前。A 店的后台 token 在 B 店必须被拒，理由比买家那边更硬：
//     后台 token 能读全部订单与客户手机号、能改价、能发起退款。
//   - 平台级令牌：它**按定义**就是跨租户的，所以它在任何 Host 上都被接受。
//     这不是网开一面，这是那个身份的语义。关键在于「是不是平台级」是一个
//     被 HMAC 覆盖的显式字段（plt）：一串商家级令牌改不成平台级，
//     改一个字节签名就不成立。
//
// **plt 不能用「mid == 0」来推。** mid 为 0 在别的每一处都表示「没设」，
// 而两种含义压在同一个零值上时，任何一处漏判都会把「忘了设租户」当成
// 「这是平台管理员」。IssueStaff 因此拒绝签发「既没有租户、又不是平台级」
// 的令牌，与 Issue 拒绝签发没有租户的买家令牌是同一条规矩。
//
// ### 二、它是**有状态**的，每个请求都查一次库
//
// 买家的 access_token 无状态（每请求不查库），代价是不可吊销，于是寿命压到
// 2 小时，「可吊销」整个放到 refresh_token 上。后台反过来：§14 写死
// 「会话默认 7 天，可撤销」，而 7 天的不可吊销令牌对一串能改价、能退款的
// 凭据是不能接受的。所以后台没有 refresh 这一层，会话 token 本身进库
// （只存 sha256），每个请求校验一次 staff_tokens。
//
// 那为什么还要签名？签名回答的是「这串东西是不是我们签的、它声称属于谁」，
// 而那个回答不查库就能给出。于是跨店尝试、伪造、过期这三类在**进入数据库
// 之前**就被拒掉 —— 一个未认证的调用方打不出一次查库。
// 数据库那一次回答的是另一件事：「这条会话被撤销了没有、这个人还在岗没有」。

// StaffClaims 是后台令牌里的全部信息。
//
// 与 Claims 一样，这里只放定位所需的东西：令牌是 base64 的，任何人都能读，
// 放进去的每一个字段都是一次公开。**role 刻意不在里面** —— 权限必须每次从库里
// 读，否则一个被降级的操作员在 7 天之内都还是管理员。
type StaffClaims struct {
	// MerchantID 是这串令牌属于哪家店；Platform 为 true 时它是 0。
	MerchantID int64
	// Platform 为 true 表示平台级操作员（staff.merchant_id 为 NULL）。
	Platform bool
	// StaffID 是 staff.id。
	StaffID   int64
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// TenantMatches 回答「这串令牌能不能用在这个租户的请求上」。
//
// 判据写成一个方法而不是散在中间件里，是因为它有两条分支，而漏掉第二条
// （平台级）会让平台操作员在任何 Host 上都被拒，漏掉第一条就是跨店越权。
// 两条都要，而且只有这一份。
func (c StaffClaims) TenantMatches(merchantID int64) bool {
	if c.Platform {
		// 平台级操作员不属于任何一家店，也就不存在「用错了店」这回事。
		// 他看到的数据由 repository.WithPlatform 的作用域决定，而那个作用域
		// 里一家店的数据都没有（00017）—— 所以在这里放行不等于放行到别人的数据上。
		return true
	}
	return c.MerchantID == merchantID
}

// ErrStaffTokenShape 是「签名对，但载荷不是一串合法的后台令牌」。
//
// 与 ErrMalformedToken 分开，是为了让签发侧的 bug 在日志里认得出来：
// 前者说的是「有人发来一串垃圾」，这一条说的是「**我们自己**签出了一串
// 自相矛盾的令牌」（既没有租户又不是平台级，或者两者同时成立）。
var ErrStaffTokenShape = errors.New("后台令牌的租户字段自相矛盾")

// IssueStaff 签发一串后台会话令牌。
//
// merchantID 为 nil 表示平台级（staff.merchant_id 为 NULL）。**不用 0 表示**，
// 理由见文件头：0 在别处一律是「没设」。
//
// 载荷里**没有会话行的 id**，与买家的 refresh_token 同理（service/auth.go 的
// issue 那一段）：会话的身份就是这串令牌的 sha256，签的时候那一行还没建出来。
// 会话行的 id 由校验时那条 UPDATE ... RETURNING 带回来，所以它从来不需要
// 出现在客户端手里的那串东西里 —— 少一个字段就少一次「签名里的 sid 与库里
// 那一行对不上」的歧义分支。
func (s *Signer) IssueStaff(merchantID *int64, staffID int64, ttl time.Duration) (string, error) {
	if staffID <= 0 {
		return "", errors.New("拒绝签发没有操作员的后台令牌")
	}
	var mid int64
	platform := merchantID == nil
	if !platform {
		mid = *merchantID
		if mid <= 0 {
			// 一串 mid 为 0 却没有标成平台级的令牌，在 TenantMatches 里
			// 对任何租户都不成立，症状是「这个人怎么登录都是 401」。
			// 签不出来好过签出来。
			return "", errors.New("拒绝签发租户为 0 却又不是平台级的后台令牌")
		}
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("取令牌随机数失败: %w", err)
	}
	now := s.now().UTC()
	payload := claimsJSON{
		MerchantID: mid,
		UserID:     staffID,
		Kind:       string(KindStaff),
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(ttl).Unix(),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Platform:   platform,
	}
	h, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signing := jwtB64.EncodeToString(h) + "." + jwtB64.EncodeToString(p)
	return signing + "." + jwtB64.EncodeToString(s.mac(signing)), nil
}

// ParseStaff 校验一串后台会话令牌：签名、typ、载荷形状、有效期。
//
// 顺序与 Parse 一样是硬的：先验签，再读载荷。
func (s *Signer) ParseStaff(token string) (StaffClaims, error) {
	p, err := s.parseSigned(token)
	if err != nil {
		return StaffClaims{}, err
	}
	// typ 进签名，所以一串买家的 access_token 打到 /admin/ 上会停在这里 ——
	// 而不是带着一个 user_id 被当成 staff_id 拿去查 staff 表。两张表的 id
	// 来自同一种自增序列，撞上不是小概率。
	if Kind(p.Kind) != KindStaff {
		return StaffClaims{}, ErrWrongKind
	}
	if p.UserID <= 0 {
		return StaffClaims{}, ErrMalformedToken
	}
	// 「有租户」与「是平台级」必须**恰好成立一个**。
	//
	// 两者都不成立 = 一串谁也不属于的令牌；两者都成立 = 一串既属于某家店、
	// 又声称跨租户的令牌，而 TenantMatches 的平台分支会让它在**任何**
	// Host 上通过 —— 那正是「A 店的令牌在 B 店能用」换了个说法。
	if (p.MerchantID > 0) == p.Platform {
		return StaffClaims{}, ErrStaffTokenShape
	}

	claims := StaffClaims{
		MerchantID: p.MerchantID,
		Platform:   p.Platform,
		StaffID:    p.UserID,
		IssuedAt:   time.Unix(p.IssuedAt, 0).UTC(),
		ExpiresAt:  time.Unix(p.ExpiresAt, 0).UTC(),
	}
	// 过期无容差，理由同 Parse。
	if !claims.ExpiresAt.After(s.now().UTC()) {
		return claims, ErrTokenExpired
	}
	return claims, nil
}

// NewOpaqueToken 取一串一次性 token 的明文（引导 / 邮件链接）。
//
// 它**不是**签名令牌，是 32 字节随机数的 base64。理由：这两种 token 的
// 生命周期完全由服务端那一行决定（用掉即失效、15 分钟 / 24 小时过期），
// 载荷里没有任何东西需要客户端读，也没有任何东西需要在不查库的情况下判断
// —— 校验它本来就必须查库（要看 used_at）。签名在这里只会让串变长，
// 并多出一条「签名对但库里那行已经用过了」的歧义路径。
//
// 32 字节：它和会话令牌一样能换出后台全权，所以按不可猜的凭据来取，
// 而不是按「够用就行」。
func NewOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("取一次性 token 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashStaffToken 是后台 token 进库前的那一步：sha256 的十六进制。
//
// 与买家那边的 HashToken 只差一个编码（那边是 BYTEA，这边 §14 写的是 TEXT）。
// 不加盐、不用 argon2，理由与 HashToken 一字不差：它和口令不是一回事 ——
// 口令是人选的、低熵的，这一串是我们取的 256 位随机数，猜不出来。
//
// 为什么只存 hash：数据模型 §14 说得最清楚 —— 后台 token 能读全部订单与
// 客户手机号、能改价、能发起退款，它和支付密钥是同一个量级的东西。
// 数据库被拖走时，存明文意味着攻击者直接拿到全部后台权限。
func HashStaffToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
