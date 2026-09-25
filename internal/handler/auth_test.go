package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
)

// 两家店的入口。种子（db/seed/dev.sql）里 shop-a 与 shop-b 都有一个
// **手机号相同**的买家，那是下面跨店断言的全部前提。
const (
	hostA = "shop-a." + baseDomain
	hostB = "shop-b." + baseDomain

	seedPhone    = "13800000001"
	seedPassword = "keel-dev-2026"
)

// post 往真实路由上发一个 JSON 请求。bearer 非空时带上 Authorization 头。
func post(t *testing.T, host, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// login 走真实的 /auth/login 拿一组令牌。
//
// 测试里的令牌**一律从这条路径来**，不自己用 testSigner 签一串。自己签的话，
// 「登录真的会签发令牌吗」就没有任何测试覆盖到，而每一条鉴权断言都在验一个
// 测试自己造出来的东西。只有过期令牌那一条是例外（没法等两小时），
// 它在自己的测试里写明了为什么。
func login(t *testing.T, host, phone, password string) api.LoginResponse {
	t.Helper()
	w := post(t, host, "/api/v1/auth/login",
		`{"phone":"`+phone+`","password":"`+password+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("登录 %s@%s 失败：%d %s", phone, host, w.Code, w.Body.String())
	}
	var out api.LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("登录响应解析失败: %v\n%s", err, w.Body.String())
	}
	return out
}

// problemOf 把响应体读成 Problem，顺带断言 Content-Type 与状态码。
func problemOf(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) api.Problem {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("状态码是 %d，期望 %d。响应体：%s", w.Code, wantStatus, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type 是 %q，期望 application/problem+json —— "+
			"契约里每个接口的错误响应都是 Problem，别的类型客户端读不懂", ct)
	}
	var p api.Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应体不是 Problem: %v\n%s", err, w.Body.String())
	}
	return p
}

// ---------------------------------------------------------------------------
// 密码登录
// ---------------------------------------------------------------------------

// 阳性对照：种子里那个买家能登进来，拿到的令牌真的能用。
//
// 它排在所有「必须被拒」的断言前面：没有它，把 Login 写成恒 401 也能让
// 下面一大半测试全绿。
func TestPasswordLoginIssuesAUsableToken(t *testing.T) {
	got := login(t, hostA, seedPhone, seedPassword)

	if got.AccessToken == "" || got.RefreshToken == nil || *got.RefreshToken == "" {
		t.Fatal("登录成功却没有令牌")
	}
	if got.TokenType != "Bearer" {
		t.Errorf("token_type 是 %q，契约里只有 Bearer", got.TokenType)
	}
	if want := int(auth.AccessTTL / time.Second); got.ExpiresIn != want {
		t.Errorf("expires_in 是 %d，期望 %d", got.ExpiresIn, want)
	}
	if got.User.Nickname != "A 店的买家" {
		t.Errorf("登进来的是 %q，期望 A 店那一行", got.User.Nickname)
	}
	// 手机号必须脱敏：这个对象会被前端缓存、被日志采集。
	if got.User.Phone == nil || *got.User.Phone != "138****0001" {
		t.Errorf("phone 是 %v，期望脱敏成 138****0001", got.User.Phone)
	}
	if got.IsNewUser != nil && *got.IsNewUser {
		t.Error("密码登录把 is_new_user 置成了 true —— 首登即注册只属于验证码那条路")
	}

	// 令牌真的能用：拿它打一个需要鉴权的接口。
	if w := post(t, hostA, "/api/v1/auth/logout", "", got.AccessToken); w.Code != http.StatusNoContent {
		t.Fatalf("拿刚签发的令牌调 logout 得到 %d %s", w.Code, w.Body.String())
	}
}

// 同一个手机号在两家店是两个人（数据模型 §8/§9）。
func TestSamePhoneIsADifferentBuyerInEachShop(t *testing.T) {
	a := login(t, hostA, seedPhone, seedPassword)
	b := login(t, hostB, seedPhone, seedPassword)

	if a.User.Id == b.User.Id {
		t.Fatalf("两家店登出来是同一个 user_id（%d）—— "+
			"要么 RLS 失效了，要么种子没给 B 店建这一行", a.User.Id)
	}
	if a.User.Nickname == b.User.Nickname {
		t.Fatalf("两家店登出来的昵称都是 %q —— 这条断言分不清两行", a.User.Nickname)
	}
}

// ---------------------------------------------------------------------------
// 跨店用令牌：本任务最要紧的一条
// ---------------------------------------------------------------------------

// A 店签发的 access_token 拿到 B 店去，必须被当作**鉴权失败**拒绝。
//
// 断言的是 problem type 而不只是 401：题面要求的是「拒绝的是鉴权语义，
// 而不是查不到这个人」，而这两者在状态码上可以完全一样。type 是客户端与
// 运维唯一能把它们分开的东西。
//
// 把 auth.Bearer 里那句 `claims.MerchantID != merchantID` 删掉会怎样：
// 请求带着 A 店的 user_id 进入 B 店的租户上下文，logout 去吊销一个在 B 店
// 不可见的会话，RLS 挡住 → 服务层按「幂等」处理 → **204**。
// 于是这条测试红在状态码上，而线上的症状是「退出登录成功了，但什么也没退」。
// 更糟的一支在注释里：两家店的 users.id 来自同一个序列，撞上就是读到另一个人。
func TestCrossTenantAccessTokenIsRejectedAsAuthFailure(t *testing.T) {
	tok := login(t, hostA, seedPhone, seedPassword).AccessToken

	// 阳性对照必须在前：这串令牌在自己店里是好用的。
	// 没有它的话，「在 B 店被拒」既可能是租户校验起作用，
	// 也可能只是这串令牌本身就是坏的。
	if w := post(t, hostA, "/api/v1/auth/logout", "", tok); w.Code != http.StatusNoContent {
		t.Fatalf("阳性对照失败：A 店的令牌在 A 店也不好用（%d %s）", w.Code, w.Body.String())
	}

	// 再登一次拿一串新的（上一串的会话刚被吊销）。
	tok = login(t, hostA, seedPhone, seedPassword).AccessToken
	p := problemOf(t, post(t, hostB, "/api/v1/auth/logout", "", tok), http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/token-tenant-mismatch" {
		t.Fatalf("跨店用令牌被拒了，但 type 是 %q —— "+
			"期望 token-tenant-mismatch。拒绝必须是鉴权语义的："+
			"一次跨店尝试和一次普通的登录失效在运维那里必须分得开", p.Type)
	}
}

// 同一条规则在 /auth/refresh 上要再做一遍。
//
// 它是**公开接口**（契约里 security: []），auth.Bearer 那道中间件不在它前面。
// 漏掉的话，A 店的 refresh_token 能在 B 店换出一串 mid=B 的合法 access_token
// —— 跨店的入口从「拿旧令牌试试」升级成「拿旧令牌换一把新钥匙」。
func TestCrossTenantRefreshTokenIsRejectedAsAuthFailure(t *testing.T) {
	got := login(t, hostA, seedPhone, seedPassword)
	body := `{"refresh_token":"` + *got.RefreshToken + `"}`

	p := problemOf(t, post(t, hostB, "/api/v1/auth/refresh", body, ""), http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/token-tenant-mismatch" {
		t.Fatalf("跨店刷新被拒了，但 type 是 %q，期望 token-tenant-mismatch", p.Type)
	}

	// 阳性对照放在后面：它会轮换掉这串 refresh_token，必须在跨店那条之后跑。
	if w := post(t, hostA, "/api/v1/auth/refresh", body, ""); w.Code != http.StatusOK {
		t.Fatalf("阳性对照失败：同一串 refresh_token 在自己店里也刷不动（%d %s）",
			w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 令牌本身
// ---------------------------------------------------------------------------

// 过期的 access_token 必须被拒，且拒的理由要与「令牌无效」分得开。
//
// 这一条的令牌是用 testSigner 直接签的 —— 全文唯一一处这么做的地方。
// 理由：它要的是一串「两小时前签的」令牌，而登录接口只会签当下的。
// 用的密钥是路由里那一个，所以服务端**真的会验过签名**，
// 拒绝只可能来自有效期。
func TestExpiredAccessTokenIsRejected(t *testing.T) {
	got := login(t, hostA, seedPhone, seedPassword)
	claims, err := testSigner.Parse(got.AccessToken)
	if err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-auth.AccessTTL - time.Minute)
	expired, err := testSigner.WithClock(func() time.Time { return past }).
		Issue(claims.MerchantID, claims.UserID, claims.SessionID, auth.KindAccess, auth.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}

	p := problemOf(t, post(t, hostA, "/api/v1/auth/logout", "", expired), http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/token-expired" {
		t.Fatalf("过期令牌的 type 是 %q，期望 token-expired —— "+
			"客户端见到它该去刷新，见到 unauthorized 该重新登录，两者不能混", p.Type)
	}
}

// 没有 Authorization 头 / 头的形状不对 / 签名不对，都要 401。
func TestLogoutRequiresAValidBearerToken(t *testing.T) {
	for name, tc := range map[string]struct{ header, token string }{
		"没有头":     {},
		"签名不对":    {token: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJtaWQiOjEsInVpZCI6MX0.bm9wZQ"},
		"不是三段":    {token: "just-a-string"},
		"refresh": {},
	} {
		tok := tc.token
		if name == "refresh" {
			// refresh_token 不能当 access_token 用。
			tok = *login(t, hostA, seedPhone, seedPassword).RefreshToken
		}
		w := post(t, hostA, "/api/v1/auth/logout", "", tok)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s：状态码是 %d，期望 401（%s）", name, w.Code, w.Body.String())
		}
	}
}

// ---------------------------------------------------------------------------
// 登录的各条失败分支
// ---------------------------------------------------------------------------

// 口令不对 → 401。**这条是口令校验那处变异验证的靶子。**
// 把 VerifyPassword 的比对改成恒真，它会红在这里。
func TestWrongPasswordIsRejected(t *testing.T) {
	w := post(t, hostA, "/api/v1/auth/login",
		`{"phone":"`+seedPhone+`","password":"wrong-password"}`, "")
	p := problemOf(t, w, http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/unauthorized" {
		t.Errorf("type 是 %q，期望 unauthorized", p.Type)
	}
	// 响应体里不能出现任何「这个号码存在 / 不存在」的线索。
	if strings.Contains(w.Body.String(), "不存在") {
		t.Errorf("401 的响应体透露了账号是否存在：%s", w.Body.String())
	}
}

// 本店没有这个号码、以及号码属于一个已注销（软删）的账号 —— 都是 401，
// 而且与「口令不对」长得一模一样。
//
// 13800000004 在种子里是 shop-a 的软删账号：它证明的是
// uk_users_phone 上那个 `deleted_at IS NULL` 不只是索引写法 ——
// 查询少了这个条件的话，注销用户还能登录，而这条会红。
func TestUnknownAndDeletedAccountsLookIdentical(t *testing.T) {
	for _, phone := range []string{"13899999999", "13800000004"} {
		w := post(t, hostA, "/api/v1/auth/login",
			`{"phone":"`+phone+`","password":"`+seedPassword+`"}`, "")
		p := problemOf(t, w, http.StatusUnauthorized)
		if p.Type != "https://keel.dev/problems/unauthorized" {
			t.Errorf("%s: type 是 %q，期望与「口令不对」同形", phone, p.Type)
		}
	}
}

// 封禁 / 注销的账号 → 403（契约明写）。
func TestDisabledAccountGets403(t *testing.T) {
	w := post(t, hostA, "/api/v1/auth/login",
		`{"phone":"13800000002","password":"`+seedPassword+`"}`, "")
	p := problemOf(t, w, http.StatusForbidden)
	if p.Type != "https://keel.dev/problems/account-disabled" {
		t.Errorf("type 是 %q，期望 account-disabled", p.Type)
	}
}

// 仅第三方登录的账号（password_hash 为空）传 password → 401，不是 500。
// 契约里这条写在 401 的描述里：「该账号未设置密码」。
func TestAccountWithoutPasswordGets401(t *testing.T) {
	w := post(t, hostA, "/api/v1/auth/login",
		`{"phone":"13800000003","password":"`+seedPassword+`"}`, "")
	problemOf(t, w, http.StatusUnauthorized)
}

// 两种凭据都给、或一个都不给 → 422。契约里那是 oneOf，不是 anyOf。
func TestLoginNeedsExactlyOneCredential(t *testing.T) {
	for _, body := range []string{
		`{"phone":"` + seedPhone + `"}`,
		`{"phone":"` + seedPhone + `","code":"1234","password":"` + seedPassword + `"}`,
		`{"password":"` + seedPassword + `"}`, // 没有手机号
	} {
		problemOf(t, post(t, hostA, "/api/v1/auth/login", body, ""),
			http.StatusUnprocessableEntity)
	}
}

// 验证码登录**没有实现**，它必须说出来，而不是静默失败、也不是伪装成
// 「验证码错误」的 401。
//
// 这条测试与 contract_test.go 的 NotYetImplementedBody 是一对：
//   - 有人实现了短信登录 → 这里红，逼他把那份清单里的一行划掉；
//   - 有人删了那行清单却没实现 → contract_test 红。
//
// 两个方向都会响，这笔账才烂不掉。
func TestSMSLoginSaysItIsNotImplemented(t *testing.T) {
	w := post(t, hostA, "/api/v1/auth/login",
		`{"phone":"`+seedPhone+`","code":"123456"}`, "")
	p := problemOf(t, w, http.StatusNotImplemented)
	if p.Type != "https://keel.dev/problems/not-implemented" {
		t.Fatalf("type 是 %q，期望 not-implemented —— 401 会让用户以为是自己"+
			"输错了验证码，而那串验证码根本没人发过", p.Type)
	}
	if _, listed := routeOf(t, http.MethodPost, "/auth/login").
		NotYetImplementedBody["code"]; !listed {
		t.Error("验证码登录还没实现，但 contract_test.go 的 NotYetImplementedBody 里" +
			"没有 code 这一笔账 —— 清单与实现分叉了")
	}
}

// ---------------------------------------------------------------------------
// 刷新与退出
// ---------------------------------------------------------------------------

// 刷新会轮换：新的一串能用，旧的那串当场失效。
func TestRefreshRotatesTheToken(t *testing.T) {
	first := login(t, hostA, seedPhone, seedPassword)
	body := `{"refresh_token":"` + *first.RefreshToken + `"}`

	w := post(t, hostA, "/api/v1/auth/refresh", body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("刷新失败：%d %s", w.Code, w.Body.String())
	}
	var second api.LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == nil || *second.RefreshToken == *first.RefreshToken {
		t.Fatal("刷新之后 refresh_token 没换 —— 轮换没有发生")
	}
	// 新令牌可用。
	if w := post(t, hostA, "/api/v1/auth/logout", "", second.AccessToken); w.Code != http.StatusNoContent {
		t.Fatalf("刷新拿到的 access_token 不可用：%d %s", w.Code, w.Body.String())
	}
	// 旧的那串不能再用。
	problemOf(t, post(t, hostA, "/api/v1/auth/refresh", body, ""), http.StatusUnauthorized)
}

// 退出登录**真的吊销**了服务端那一行，而不是回一个 204 就完事。
//
// 这条测试是 user_tokens 那张表存在的全部理由（见 00010 的文件头）：
// 无状态令牌吊销不了，所以 refresh_token 走服务端存储。
// 把 RevokeSession 那一句删掉（或者让 Logout 直接返回 nil），这里会红。
func TestLogoutRevokesTheSession(t *testing.T) {
	got := login(t, hostA, seedPhone, seedPassword)
	body := `{"refresh_token":"` + *got.RefreshToken + `"}`

	// 退出之前，这串 refresh_token 是好用的 —— 阳性对照，但它会轮换，
	// 所以拿轮换后的那串继续。
	w := post(t, hostA, "/api/v1/auth/refresh", body, "")
	if w.Code != http.StatusOK {
		t.Fatalf("阳性对照失败：退出之前就刷不动了（%d %s）", w.Code, w.Body.String())
	}
	var refreshed api.LoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}

	if w := post(t, hostA, "/api/v1/auth/logout", "", refreshed.AccessToken); w.Code != http.StatusNoContent {
		t.Fatalf("退出登录失败：%d %s", w.Code, w.Body.String())
	}

	// 退出之后，那串 refresh_token 再也换不出东西。
	after := `{"refresh_token":"` + *refreshed.RefreshToken + `"}`
	problemOf(t, post(t, hostA, "/api/v1/auth/refresh", after, ""), http.StatusUnauthorized)

	// 重复退出是幂等的：客户端在网络抖动时会重发。
	if w := post(t, hostA, "/api/v1/auth/logout", "", refreshed.AccessToken); w.Code != http.StatusNoContent {
		t.Fatalf("第二次退出登录返回 %d，退出必须幂等", w.Code)
	}
}
