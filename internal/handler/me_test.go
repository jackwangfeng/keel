package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 个人信息（/me 与 /me/*）的端到端测试，外加「买家自己的三组接口一条不漏地要令牌」。

// 19 条买家自己的接口，不带令牌一律 401。
//
// 只核路由表（run_test.go）的话，把 app.go 里某一行的 auth.Bearer 删掉，路由表
// 一个字都不会变 —— 这条测试逐条打一次，才是「每一条都挂了」的证明。
// 清单从 contract_test.go 的 routes 表里按路径前缀筛出来，不另抄一份：
// 新加一条 /cart 或 /me 下的路由而忘了挂令牌，这里自动覆盖到。
func TestBuyerSelfRoutesRequireToken(t *testing.T) {
	n := 0
	for _, r := range routes {
		p := r.ContractPath
		if !(p == "/me" || strings.HasPrefix(p, "/me/") || strings.HasPrefix(p, "/addresses") ||
			p == "/cart" || strings.HasPrefix(p, "/cart/")) {
			continue
		}
		n++
		path := strings.NewReplacer("{address_id}", "1", "{item_id}", "1", "{provider}", "1",
			"{notification_id}", "1").
			Replace(apiPrefix + p)
		var body *strings.Reader
		if r.HTTPMethod == http.MethodGet || r.HTTPMethod == http.MethodDelete {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(`{}`)
		}
		req := httptest.NewRequest(r.HTTPMethod, path, body)
		req.Host = "shop-a." + baseDomain
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", freshIdemKey())
		w := httptest.NewRecorder()
		testEngine.ServeHTTP(w, req)
		if typ := problemType(t, w, http.StatusUnauthorized, r.HTTPMethod+" "+path); typ != problem.TypeUnauthorized {
			t.Errorf("%s %s 不带令牌：type 是 %s", r.HTTPMethod, path, typ)
		}
	}
	if n != 23 {
		t.Fatalf("从 routes 表里筛出 %d 条买家自己的接口，期望 23 条（个人信息 6 + 地址簿 6 + 购物车 7 + 消息中心 4）", n)
	}
}

func TestMeReadAndPatch(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "资料")

	var u api.User
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/me", "", b), http.StatusOK, "读资料", &u)
	if u.Id != b.UserID || u.Nickname != "买家 资料" {
		t.Fatalf("读到的不是自己：%+v", u)
	}
	if u.Phone == nil || *u.Phone == b.Phone || !strings.Contains(*u.Phone, "****") {
		t.Fatalf("手机号必须脱敏返回：%v", u.Phone)
	}
	if u.HasPassword == nil || *u.HasPassword {
		t.Fatalf("夹具买家没有密码，has_password 应为 false：%v", u.HasPassword)
	}

	avatar := mustBuyerUpload(t, bs.couponShop, b, 2, []byte("头像 "+bs.Suffix))
	decodeInto(t, bs.call(t, http.MethodPatch, "/api/v1/me",
		fmt.Sprintf(`{"nickname":"  新昵称  ","gender":2,"avatar_url":%q}`, avatar.Url), b),
		http.StatusOK, "改资料", &u)
	if u.Nickname != "新昵称" || u.Gender == nil || *u.Gender != 2 ||
		u.AvatarUrl == nil || *u.AvatarUrl != avatar.Url {
		t.Fatalf("改资料的结果不对：%+v", u)
	}
	// 只改一个字段，别的不动；avatar_url 给空串即清掉。
	// 每次解码前清零：json.Unmarshal 不会把响应里缺席的字段置回零值。
	u = api.User{}
	decodeInto(t, bs.call(t, http.MethodPatch, "/api/v1/me", `{"avatar_url":""}`, b), http.StatusOK, "清头像", &u)
	if u.AvatarUrl != nil || u.Nickname != "新昵称" || *u.Gender != 2 {
		t.Fatalf("清头像不该动别的字段：%+v", u)
	}
	// 再读一次：落库了，不只是回显。
	u = api.User{}
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/me", "", b), http.StatusOK, "再读资料", &u)
	if u.Nickname != "新昵称" || u.AvatarUrl != nil {
		t.Fatalf("改动没有落库：%+v", u)
	}

	for _, body := range []string{
		`{}`,
		`{"nickname":"   "}`,
		fmt.Sprintf(`{"nickname":%q}`, strings.Repeat("字", 33)),
		`{"gender":3}`,
		`{"avatar_url":"https://img.example.com/a.png"}`,
	} {
		if typ := problemType(t, bs.call(t, http.MethodPatch, "/api/v1/me", body, b),
			http.StatusUnprocessableEntity, "非法资料 "+body); typ != problem.TypeInvalidRequest {
			t.Fatalf("%s 的 type 是 %s", body, typ)
		}
	}
	// 另一个买家读到的是他自己，改动不串。
	other := bs.newBuyer(t, "另一个")
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/me", "", other), http.StatusOK, "别人读资料", &u)
	if u.Id != other.UserID || u.Nickname != "买家 另一个" {
		t.Fatalf("另一个买家读到的是：%+v", u)
	}

	// 注销（软删）之后，手里还没过期的令牌读 /me 是 401 而不是 500 或别人的资料。
	adminExec(t, `UPDATE users SET deleted_at = now() WHERE id = $1`, b.UserID)
	if typ := problemType(t, bs.call(t, http.MethodGet, "/api/v1/me", "", b),
		http.StatusUnauthorized, "注销后读资料"); typ != problem.TypeUnauthorized {
		t.Fatalf("注销后读资料的 type 是 %s", typ)
	}
}

func insertIdentity(t *testing.T, bs buyerShop, userID int64, provider int, withUnion bool) {
	t.Helper()
	union := any(nil)
	if withUnion {
		union = fmt.Sprintf("union-%d-%d", userID, provider)
	}
	adminExec(t, `INSERT INTO user_identities (merchant_id, user_id, provider, external_id, union_id)
	              VALUES ($1, $2, $3, $4, $5)`,
		bs.MerchantID, userID, provider, fmt.Sprintf("openid-%d-%d", userID, provider), union)
}

func identityCount(t *testing.T, userID int64) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM user_identities WHERE user_id = $1`, userID)
}

// 已绑定身份的列表不漏敏感标识；解绑守住「最后一个凭据」，且只能解自己的。
func TestIdentitiesListAndUnbind(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "身份")
	other := bs.newBuyer(t, "别人")

	var ids []api.UserIdentity
	decodeInto(t, bs.call(t, http.MethodGet, "/api/v1/me/identities", "", b), http.StatusOK, "空列表", &ids)
	if ids == nil || len(ids) != 0 {
		t.Fatalf("没有绑定时应是空数组：%v", ids)
	}

	insertIdentity(t, bs, b.UserID, 1, true)
	insertIdentity(t, bs, other.UserID, 1, false)
	w := bs.call(t, http.MethodGet, "/api/v1/me/identities", "", b)
	decodeInto(t, w, http.StatusOK, "列表", &ids)
	if len(ids) != 1 || ids[0].Provider != 1 || ids[0].HasUnionId == nil || !*ids[0].HasUnionId {
		t.Fatalf("列表不对（只该有自己那一条）：%+v", ids)
	}
	if body := w.Body.String(); strings.Contains(body, "openid-") || strings.Contains(body, "union-") {
		t.Fatalf("openid / unionid 是渠道敏感标识，一律不对外返回：%s", body)
	}

	// 没有密码、只有这一条身份：解绑之后就登不进来了 → 409，那一行还在。
	if typ := problemType(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/1", "", b),
		http.StatusConflict, "解绑最后一个凭据"); typ != problem.TypeLastCredential {
		t.Fatalf("type 是 %s", typ)
	}
	if identityCount(t, b.UserID) != 1 {
		t.Fatal("被拒的解绑删掉了那一行 —— 应当整体回滚")
	}

	// 再绑一个 Apple：现在可以解掉微信。
	insertIdentity(t, bs, b.UserID, 5, false)
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/1", "", b), http.StatusNoContent, "解绑微信")
	if identityCount(t, b.UserID) != 1 {
		t.Fatal("解绑之后应只剩 Apple 那一条")
	}
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/1", "", b), http.StatusNotFound, "重复解绑")
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/9", "", b), http.StatusNotFound, "不存在的 provider")
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/abc", "", b), http.StatusNotFound, "非数字 provider")

	// 有密码的人可以解掉最后一个身份。
	adminExec(t, `UPDATE users SET password_hash = 'x' WHERE id = $1`, b.UserID)
	wantStatus(t, bs.call(t, http.MethodDelete, "/api/v1/me/identities/5", "", b), http.StatusNoContent, "有密码时解绑最后一个身份")

	// 别人的那一条原封不动：我解绑 provider 1 只动我自己名下的。
	if identityCount(t, other.UserID) != 1 {
		t.Fatal("解绑动到了别人名下的身份")
	}
}

// 绑微信、绑手机号：本项目没接微信开放平台与短信服务，两条都诚实地回 501，
// 并且 contract_test.go 里挂着这两笔账（实现了而忘了划掉 → 这条会红）。
func TestIdentityAndPhoneBindingSayNotImplemented(t *testing.T) {
	bs := newBuyerShop(t)
	b := bs.newBuyer(t, "绑定")
	for _, tc := range []struct{ path, body string }{
		{"/api/v1/me/identities/wechat", `{"code":"wx-code"}`},
		{"/api/v1/me/phone", `{"phone":"13800138000","code":"123456"}`},
	} {
		if typ := problemType(t, bs.call(t, http.MethodPost, tc.path, tc.body, b),
			http.StatusNotImplemented, tc.path); typ != problem.TypeNotImplemented {
			t.Fatalf("%s 的 type 是 %s", tc.path, typ)
		}
		r := routeOf(t, http.MethodPost, strings.TrimPrefix(tc.path, apiPrefix))
		if _, ok := r.NotYetImplementedBody["code"]; !ok {
			t.Fatalf("%s 返回 501，但 contract_test.go 的 routes 表里没有挂 code 这一笔账", tc.path)
		}
	}
	if identityCount(t, b.UserID) != 0 {
		t.Fatal("501 的绑定请求落了一行身份")
	}
}
