package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 令牌必须锁死在租户上
// ---------------------------------------------------------------------------
//
// 这是本包存在的主要理由，也是多租户系统里最容易漏、后果最重的一处。
//
// 租户来自 Host（或单商家部署的默认商家），用户来自令牌。**两者是独立的输入**，
// 于是存在一个天然的攻击面：拿 shop-a 签发的令牌去访问 shop-b。
//
// 数据模型 §8/§9 已经定死了正确答案：买家属于商家，同一个人在 A 店和 B 店是
// 两行 users（uk_users_phone 的首列就是 merchant_id）。所以 A 店的令牌在 B 店
// 必须**不被接受**——而不是「解析出一个 user_id 然后在 B 店的租户上下文里去查」。
// 后者有两种结局，都比拒绝糟：
//
//   - 查不到 → 症状长得像「登录失效」，真因（跨店用令牌）完全不出现在任何
//     一条日志里，排查方向从第一步就是错的；
//   - user_id 恰好撞上 → **读到另一个人的数据**。两家店的 users.id 来自同一个
//     库、同一种自增序列，撞上不是小概率，是日常。
//
// ### 选的是哪条路：签名覆盖租户
//
// 题面给的两条路是「签名覆盖它」或「服务端存储」。access_token 走前者，
// 每一次请求校验它**不查库**。
//
// 为什么不是服务端存储：access_token 出现在每一个请求上，把它做成服务端状态
// 等于给每个请求加一次查库。而这条路要防的东西，签名已经防住了——
// mid 是被 HMAC 覆盖的字段，改一个字节签名就不成立。
//
// 两个方向的代价，诚实写出来：
//
//   - **令牌泄露方向**：无状态令牌**吊销不了**。一串 access_token 泄露之后，
//     它在有效期内一直好用，服务端没有任何开关能关掉它。本项目的应对是把
//     有效期压到 2 小时（AccessTTL），并把「可吊销」这件事整个放到
//     refresh_token 上——那一条走服务端存储（user_tokens 表，只存 sha256），
//     退出登录吊销的是它。所以两条路本项目都用了，分工是按「出现频率」
//     和「寿命」分的，不是二选一。
//   - **服务端状态方向**：无状态令牌在这个方向上的代价是**签名密钥**。
//     密钥换掉（或没配、进程重启后随机重取）时，全体用户被登出一次；
//     多实例部署时每个实例必须拿到同一个密钥，否则 A 实例签的令牌在 B 实例
//     上验不过——症状是「刷新页面有时候要重新登录」。这条写在
//     internal/app 里 KEEL_AUTH_SECRET 那段。
//
// ### 为什么不是「令牌决定租户」
//
// internal/tenant/resolver.go 写着「刻意不支持用请求头指定租户：公开接口没有
// 鉴权，那等于让调用方自己声明它是哪家店」。现在有鉴权了，那句话仍然成立，
// 而且理由比原来更强：
//
//   - 租户决定的是**这个请求属于哪家店的店面**，它对匿名请求也必须有答案
//     （商品列表、商品详情、下单前的试算都没有令牌）。让令牌来定租户，
//     等于让同一条路径在有没有令牌时走两套解析，而它们分叉的那天没人会发现。
//   - 令牌是客户端持有的东西。它能决定租户，就等于「调用方自己声明它是哪家店」
//     换了个签名的说法——签名只证明这串东西是我们签的，不证明它该出现在这个
//     Host 上。一个从 shop-a 拿到令牌的人，会拿它去 shop-b 的域名下试，
//     而那正是这里要拒绝的动作。
//
// 所以方向保持不变：**租户仍由 Host 决定，令牌只被校验是否属于这个租户。**

// 令牌有效期。
//
// access 2 小时：它不可吊销（见上），所以这个数字就是「一串泄露的 access_token
// 最久能用多久」。再短会让移动端在弱网下频繁刷新，再长就把不可吊销的窗口
// 拉到半天以上。
//
// refresh 30 天：它可吊销、可轮换，寿命长的代价由那两条性质兜住。
// 三十天是「一个月不打开 App 就要重新登录」，是国内电商 App 的常规体感。
const (
	AccessTTL  = 2 * time.Hour
	RefreshTTL = 30 * 24 * time.Hour
)

// Kind 区分两种令牌。它进签名，所以一串 refresh_token 拿去当 access_token 用
// 会被当场拒绝——没有这个字段的话，两者只差一个过期时间，
// 而 refresh_token 的寿命是 access 的 360 倍。
type Kind string

const (
	KindAccess  Kind = "access"
	KindRefresh Kind = "refresh"
)

// Claims 是令牌里的全部信息。
//
// 没有昵称、没有手机号、没有权限位：令牌是 base64 的，**任何人都能读**。
// 放进去的每一个字段都是一次公开。这里只放定位所需的三个 id 加上生命周期。
type Claims struct {
	// MerchantID 是这串令牌属于哪家店。它是本文件存在的理由，见文件头。
	MerchantID int64
	UserID     int64
	// SessionID 指向 user_tokens 里那一行。access_token 带着它，
	// 于是 /auth/logout 只拿一个 access_token 就知道该吊销哪个会话。
	SessionID int64
	Kind      Kind
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// 令牌校验的三种失败。分开是因为它们的处置不同：格式与签名是「这不是我们签的
// 东西」，过期是「这是我们签的，但过期了」——客户端见到后者应该去刷新，
// 见到前者应该去重新登录。
var (
	ErrMalformedToken = errors.New("令牌格式不合法")
	ErrBadSignature   = errors.New("令牌签名不匹配")
	ErrTokenExpired   = errors.New("令牌已过期")
	ErrWrongKind      = errors.New("令牌类型不对")
)

// claimsJSON 是令牌载荷的线格式。
//
// 字段名短，且与 JWT 的惯例对齐：exp / iat 是 RFC 7519 的注册声明（秒级
// 数字时间），mid / uid / sid / typ 是本项目的私有声明。
type claimsJSON struct {
	MerchantID int64  `json:"mid"`
	UserID     int64  `json:"uid"`
	SessionID  int64  `json:"sid"`
	Kind       string `json:"typ"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`

	// Nonce 让每一串令牌都不同。
	//
	// 不加它的话，同一个用户在同一秒内登录两次会拿到**逐字节相同**的两串
	// refresh_token（载荷里的字段全都一样），而 user_tokens 上
	// uk_user_tokens_hash 是唯一索引——第二次登录会撞唯一约束失败。
	// 那是一个只在「同一秒、同一用户、两次登录」时出现的 500，
	// 本地几乎复现不出来。
	Nonce string `json:"jti"`
}

// header 是固定的 JWT 头。
//
// **不从令牌里读 alg 来决定怎么验**，只拿它来核对。那是 JWT 历史上最贵的一课：
// 收到 alg=none 就不验签、收到 alg=HS256 而服务端配的是 RSA 公钥就拿公钥当
// HMAC 密钥——两者都是「让攻击者选择校验算法」。这里算法是写死的，
// 令牌里的 alg 只是一个必须等于 HS256 的常量字段。
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

var jwtB64 = base64.RawURLEncoding

// Signer 签发与校验令牌。
//
// 自己实现 HS256 而不是引一个 JWT 库：需要的是三段 base64 加一次 HMAC，
// 大约六十行；而 JWT 库的历史 CVE 几乎全部出在「它支持而我们不需要」的那些
// 地方（alg 协商、kid 解引用、嵌套令牌、时间容差）。主模块此前的运行时依赖
// 是 0，本任务只为口令哈希加了一个（golang.org/x/crypto），这里不再加第二个。
type Signer struct {
	key []byte
	now func() time.Time
}

// NewSigner 建签名器。key 请给至少 32 字节的随机值。
//
// now 可注入是为了让「过期的令牌必须被拒」有一条不必真的等两小时的测试。
func NewSigner(key []byte) *Signer {
	return &Signer{key: key, now: time.Now}
}

// WithClock 换掉时钟，只给测试用。
func (s *Signer) WithClock(now func() time.Time) *Signer {
	return &Signer{key: s.key, now: now}
}

// NewRandomKey 取一个进程内随机密钥。没配 KEEL_AUTH_SECRET 时用它。
func NewRandomKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("取随机签名密钥失败: %w", err)
	}
	return k, nil
}

// Issue 按 kind 与 ttl 签发一串令牌。
func (s *Signer) Issue(merchantID, userID, sessionID int64, kind Kind, ttl time.Duration) (string, error) {
	if merchantID <= 0 {
		// 空租户的令牌是一串能被任何店接受的凭据——它必须签不出来，
		// 而不是签出来之后靠调用方记得检查。
		return "", errors.New("拒绝签发没有租户的令牌")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("取令牌随机数失败: %w", err)
	}
	now := s.now().UTC()
	payload := claimsJSON{
		MerchantID: merchantID,
		UserID:     userID,
		SessionID:  sessionID,
		Kind:       string(kind),
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(ttl).Unix(),
		Nonce:      jwtB64.EncodeToString(nonce),
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

// Parse 校验签名与有效期，返回声明。
//
// 顺序是硬的：**先验签，再读载荷**。反过来的话，一段攻击者构造的 JSON 会先
// 被解析、被用来查库，然后才发现签名不对——那条路径上任何一处的副作用
// （一次错误日志里的注入、一次昂贵的查询）都成了不需要凭据就能触发的东西。
func (s *Signer) Parse(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformedToken
	}
	sig, err := jwtB64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	// hmac.Equal 是常数时间的。用 bytes.Equal 的话，逐字节的返回时间差
	// 理论上可以被用来一个字节一个字节地凑出签名。
	if !hmac.Equal(sig, s.mac(parts[0]+"."+parts[1])) {
		return Claims{}, ErrBadSignature
	}

	rawHeader, err := jwtB64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	var hd header
	if err := json.Unmarshal(rawHeader, &hd); err != nil {
		return Claims{}, ErrMalformedToken
	}
	// 签名已经过了，所以这里的 alg 不可能是攻击者选的——但仍然要核对：
	// 哪天签名算法换代，一串用旧算法签的令牌不该因为「签名恰好也对」而通过。
	if hd.Alg != "HS256" {
		return Claims{}, ErrBadSignature
	}

	rawPayload, err := jwtB64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformedToken
	}
	var p claimsJSON
	if err := json.Unmarshal(rawPayload, &p); err != nil {
		return Claims{}, ErrMalformedToken
	}
	if p.MerchantID <= 0 || p.UserID <= 0 {
		// 签名对但字段缺失，说明签发侧有 bug。当成格式错拒掉，
		// 而不是带着一个 0 继续往下走——0 会在 RLS 那一层变成一次 42501，
		// 或者更糟，变成一次谁也没预期的匹配。
		return Claims{}, ErrMalformedToken
	}

	claims := Claims{
		MerchantID: p.MerchantID,
		UserID:     p.UserID,
		SessionID:  p.SessionID,
		Kind:       Kind(p.Kind),
		IssuedAt:   time.Unix(p.IssuedAt, 0).UTC(),
		ExpiresAt:  time.Unix(p.ExpiresAt, 0).UTC(),
	}
	// 过期判定**没有容差**。给一个「5 分钟宽限」看上去很体贴，实际是把
	// 有效期悄悄延长了 5 分钟，而所有关于「令牌最久能用多久」的推理
	// （见 AccessTTL 那段）都会因此不成立。时钟漂移是运维问题，
	// 该由 NTP 解决，不该由鉴权层替它兜底。
	if !claims.ExpiresAt.After(s.now().UTC()) {
		return claims, ErrTokenExpired
	}
	return claims, nil
}

// ParseKind 在 Parse 之上多校验一条：这串令牌是不是要的那一种。
func (s *Signer) ParseKind(token string, want Kind) (Claims, error) {
	claims, err := s.Parse(token)
	if err != nil {
		return claims, err
	}
	if claims.Kind != want {
		return claims, ErrWrongKind
	}
	return claims, nil
}

func (s *Signer) mac(signing string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(signing))
	return m.Sum(nil)
}

// HashToken 是 refresh_token 进库前的那一步：只存 sha256。
//
// 理由同数据模型 §14 的 staff_tokens：一份能直接用来登录的凭据在库里以明文
// 躺着，等价于把它写进每一次备份、每一条慢查询日志、每一个 pg_dump。
// 校验是按 hash 的点查，所以明文只需要在签发的那一瞬间存在。
//
// 这里不加盐、不用 argon2：它和口令不是一回事。口令是人选的、低熵的，
// 必须靠慢哈希抵御离线爆破；refresh_token 是我们签的、带 256 位 HMAC 的串，
// 猜不出来，慢哈希只会给每次刷新加上 24 ms。
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
