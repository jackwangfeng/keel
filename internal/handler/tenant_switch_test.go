package handler_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 平台级会话的租户切换（X-Keel-Merchant）与商家管理。规则写在
// internal/auth/staff_tenant.go 的文件头，这里一条规则一条测试。
//
// 每一条都做过变异（改掉被守护的那一行、看它红不红），结果记在
// web/admin/README.md 的「商家管理与租户切换」一节。

// switchReq 发一个请求；switchTo 非空时带上 X-Keel-Merchant。
// POST 一律带一把新的幂等键，理由同 reqAs。
func switchReq(t *testing.T, engine *gin.Engine, method, host, path, body, bearer, switchTo string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = host
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if switchTo != "" {
		r.Header.Set(auth.MerchantSwitchHeader, switchTo)
	}
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", freshIdemKey())
	}
	w := httptest.NewRecorder()
	if engine == nil {
		engine = testEngine
	}
	engine.ServeHTTP(w, r)
	return w
}

// categoryNames 读 GET /admin/categories 的名字集合。
func categoryNames(t *testing.T, w *httptest.ResponseRecorder, what string) map[string]bool {
	t.Helper()
	var cats []api.AdminCategory
	decodeInto(t, w, http.StatusOK, what, &cats)
	out := map[string]bool{}
	for _, c := range cats {
		out[c.Name] = true
	}
	return out
}

// mkCategory 在一家店里建一个类目（走那家店自己的商家级管理员），返回名字。
func mkCategory(t *testing.T, sh adminShop, name string) {
	t.Helper()
	wantStatus(t, reqAs(t, http.MethodPost, sh.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q}`, name), sh.Token), http.StatusCreated, "建类目 "+name)
}

func shopCode(sh adminShop) string { return strings.TrimSuffix(sh.Host, "."+baseDomain) }

// 规则 2：平台管理员带头 → 落在头指定的那家店，读写都是。
//
// 读和写分开断言：只验读的话，一个「读切过去了、写还落在 Host 那家」的实现
// 是绿的（比如只在某个读路径上换了租户），而写错店是这条规则真正要防的事。
func TestPlatformSwitchLandsOnTheNamedMerchant(t *testing.T) {
	a, b := newAdminShop(t), newAdminShop(t)
	mkCategory(t, a, "只在A店-"+a.Suffix)
	mkCategory(t, b, "只在B店-"+b.Suffix)
	token := newPlatformAdmin(t)

	// 读：Host 是 A 店，头是 B 店 → 读到的是 B 店。
	got := categoryNames(t, switchReq(t, nil, http.MethodGet, a.Host, "/api/v1/admin/categories", "", token, shopCode(b)),
		"平台管理员带头读类目")
	if !got["只在B店-"+b.Suffix] {
		t.Fatalf("带 %s=%s 读到的类目是 %v，里面没有 B 店的类目 —— 切换没生效",
			auth.MerchantSwitchHeader, shopCode(b), got)
	}
	if got["只在A店-"+a.Suffix] {
		t.Fatalf("带头切到 B 店，却读到了 A 店（Host 那家）的类目：%v", got)
	}

	// 对照：不带头时就是 Host 那家。没有这一条，上面那条也可能是「平台管理员
	// 总能看见全部店」这种更糟的实现给出的绿。
	got = categoryNames(t, switchReq(t, nil, http.MethodGet, a.Host, "/api/v1/admin/categories", "", token, ""),
		"平台管理员不带头读类目")
	if !got["只在A店-"+a.Suffix] || got["只在B店-"+b.Suffix] {
		t.Fatalf("不带头时读到的类目是 %v，期望只有 A 店的", got)
	}

	// 写：带头建一个类目，它必须落在 B 店。
	name := "平台切过去建的-" + b.Suffix
	wantStatus(t, switchReq(t, nil, http.MethodPost, a.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q}`, name), token, shopCode(b)), http.StatusCreated, "平台管理员带头建类目")
	owner := adminQueryInt64(t, `SELECT merchant_id FROM categories WHERE name = $1`, name)
	if owner != b.MerchantID {
		t.Fatalf("带头建的类目落在 merchant_id=%d，期望 B 店 %d（A 店是 %d）", owner, b.MerchantID, a.MerchantID)
	}
}

// 规则 3：商家级员工带头 → 403，而且**没有**读到或写到别家的东西。
//
// 两种失败都要挡：生效（读到 B 店）和静默忽略（200 且落在自己店）。
// 后者一样危险：一个以为自己切过去了的客户端，会把给 B 店的数据写进 A 店。
func TestMerchantStaffWithSwitchHeaderIsRejected(t *testing.T) {
	a, b := newAdminShop(t), newAdminShop(t)
	mkCategory(t, b, "B店的秘密-"+b.Suffix)

	w := switchReq(t, nil, http.MethodGet, a.Host, "/api/v1/admin/categories", "", a.Token, shopCode(b))
	if got := problemType(t, w, http.StatusForbidden, "商家级员工带头读"); got != problem.TypeTenantSwitchForbidden {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeTenantSwitchForbidden)
	}
	if strings.Contains(w.Body.String(), "B店的秘密") {
		t.Fatalf("403 的响应体里出现了 B 店的数据：%s", w.Body.String())
	}

	name := "不该写进任何一家-" + a.Suffix
	w = switchReq(t, nil, http.MethodPost, a.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q}`, name), a.Token, shopCode(b))
	if got := problemType(t, w, http.StatusForbidden, "商家级员工带头写"); got != problem.TypeTenantSwitchForbidden {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeTenantSwitchForbidden)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM categories WHERE name = $1`, name); n != 0 {
		t.Fatalf("接口回了 403，库里却有 %d 行叫 %q 的类目 —— 拒绝发生在写之后，"+
			"或者头被静默忽略了", n, name)
	}
}

// 规则 3 对**每一条**挂后台会话的路由都成立，不只是上面那两条。
//
// 这是「头的处理放在 StaffBearer 里而不是单独的中间件」这个决定的执行者：
// 契约里每一条声明了 KeelMerchant 的后台操作，只要注册了路由，就拿商家级会话
// + 头打一次，全部必须 403 tenant-switch-forbidden。哪条路由绕开了 StaffBearer
// （或者将来有人把切换挪成一道漏挂的中间件），它在这里就会是 200 / 404 / 422。
func TestSwitchHeaderIsRefusedForMerchantStaffOnEveryStaffRoute(t *testing.T) {
	a := newAdminShop(t)
	doc := loadContract(t)

	registered := map[string]bool{}
	for _, ri := range testEngine.Routes() {
		registered[ri.Method+" "+ri.Path] = true
	}

	checked := 0
	for path, item := range doc.Paths {
		for method, raw := range item {
			op, ok := raw.(map[string]any)
			if !ok || !contractHTTPMethods[method] || !declaresKeelMerchant(op) {
				continue
			}
			key := strings.ToUpper(method) + " " + ginPathOf(path)
			if !registered[key] {
				continue // 契约里有、服务端还没落地的（notYetRouted），没东西可打
			}
			concrete := strings.NewReplacer(
				"{product_id}", "1", "{sku_id}", "1", "{category_id}", "1", "{staff_id}", "1",
				"{store_id}", "1", "{region_id}", "1", "{merchant_id}", "1", "{order_no}", "x",
				"{refund_no}", "x").Replace(path)
			w := switchReq(t, nil, strings.ToUpper(method), a.Host, "/api/v1"+concrete, "", a.Token, "shop-b")
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), problem.TypeTenantSwitchForbidden) {
				t.Errorf("%s：商家级会话带 %s 得到 %d %s，期望 403 %s",
					key, auth.MerchantSwitchHeader, w.Code, w.Body.String(), problem.TypeTenantSwitchForbidden)
			}
			checked++
		}
	}
	if checked < 40 {
		t.Fatalf("只打了 %d 条路由 —— 后台挂会话的路由有四十多条，这条测试没在检查它该检查的东西", checked)
	}
}

func declaresKeelMerchant(op map[string]any) bool {
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		m, _ := p.(map[string]any)
		if ref, _ := m["$ref"].(string); strings.HasSuffix(ref, "/KeelMerchant") {
			return true
		}
	}
	return false
}

// 契约那一侧：KeelMerchant **恰好**声明在每一条挂后台会话的操作上
// （/admin/ 下 security 不是 [] 的），别处一条都没有；参数名与代码里的常量一致。
//
// 这个头是可选的，contract_test.go 的请求头对账只核「必填」那一类，
// 所以没有这一条的话，契约里漏声明、多声明都不会红。
func TestKeelMerchantHeaderDeclaredExactlyOnStaffOperations(t *testing.T) {
	doc := loadContract(t)
	p, ok := doc.Components.Parameters["KeelMerchant"]
	if !ok {
		t.Fatal("契约 components/parameters 里没有 KeelMerchant")
	}
	if p.Name != auth.MerchantSwitchHeader || p.In != "header" || p.Required {
		t.Fatalf("KeelMerchant 是 name=%q in=%q required=%v，期望 name=%q in=header required=false",
			p.Name, p.In, p.Required, auth.MerchantSwitchHeader)
	}
	declared := 0
	for path, item := range doc.Paths {
		for method, raw := range item {
			op, ok := raw.(map[string]any)
			if !ok || !contractHTTPMethods[method] {
				continue
			}
			sec, hasSec := op["security"].([]any)
			staffOp := strings.HasPrefix(path, "/admin/") && !(hasSec && len(sec) == 0)
			has := declaresKeelMerchant(op)
			switch {
			case staffOp && !has:
				t.Errorf("%s %s 挂后台会话，却没声明 KeelMerchant —— 商家级员工带这个头会被 403，"+
					"契约得说出来", method, path)
			case !staffOp && has:
				t.Errorf("%s %s 不挂后台会话，却声明了 KeelMerchant —— 这个头只在平台级鉴权之后才读，"+
					"公开接口与买家接口一律不读", method, path)
			}
			if has {
				declared++
			}
		}
	}
	if declared < 40 {
		t.Fatalf("只有 %d 条操作声明了 KeelMerchant —— 这条测试没在检查它该检查的东西", declared)
	}
}

// 买家侧带头：完全无效，落在 Host 解析的那家。
//
// resolver.go 那句「刻意不支持用请求头指定租户」一个字没改，这条测试是它的
// 行为执行者：买家接口没有平台级鉴权可以作为前提。
func TestBuyerRequestIgnoresSwitchHeader(t *testing.T) {
	ids := func(w *httptest.ResponseRecorder) map[int64]bool {
		t.Helper()
		var body listResp
		decodeInto(t, w, http.StatusOK, "商品列表", &body)
		out := map[int64]bool{}
		for _, it := range body.Items {
			out[it.ID] = true
		}
		return out
	}
	plainA := ids(switchReq(t, nil, http.MethodGet, hostA, "/api/v1/products", "", "", ""))
	plainB := ids(switchReq(t, nil, http.MethodGet, hostB, "/api/v1/products", "", "", ""))
	withHeader := ids(switchReq(t, nil, http.MethodGet, hostA, "/api/v1/products", "", "", "shop-b"))

	if len(plainA) == 0 || len(plainB) == 0 {
		t.Fatalf("种子里 shop-a / shop-b 应该各有商品（A=%d B=%d），前提不成立", len(plainA), len(plainB))
	}
	for id := range plainB {
		if withHeader[id] {
			t.Fatalf("买家请求 Host=shop-a 带 %s=shop-b，结果里出现了 B 店的商品 %d —— "+
				"公开接口读了这个头", auth.MerchantSwitchHeader, id)
		}
	}
	if len(withHeader) != len(plainA) {
		t.Fatalf("带头的结果（%d 件）和不带头（%d 件）不一样 —— 头影响了买家侧", len(withHeader), len(plainA))
	}
}

// 未登录带头：无效。后台接口上它先是 401（鉴权在前，头在后），
// 公开接口上它落在 Host 那家（上一条测试）。
//
// 401 而不是 422 / 403 是要紧的：那证明头在鉴权之前**根本没被读**。
// 反过来的实现（先读头再鉴权）会把一个匿名请求当成零值身份——而 StaffIdentity
// 的零值 MerchantID 是 nil，也就是**平台级**。
func TestAnonymousRequestIgnoresSwitchHeader(t *testing.T) {
	for _, code := range []string{"shop-b", "no-such-shop"} {
		w := switchReq(t, nil, http.MethodGet, hostA, "/api/v1/admin/categories", "", "", code)
		if got := problemType(t, w, http.StatusUnauthorized, "匿名带头"); got != problem.TypeUnauthorized {
			t.Fatalf("匿名请求带 %s=%s，type 是 %q，期望 %q —— 头在鉴权之前被读了",
				auth.MerchantSwitchHeader, code, got, problem.TypeUnauthorized)
		}
	}
	// 未认证的 /admin/auth/* 也不读：一个不存在的 code 不该让登录失败。
	w := switchReq(t, nil, http.MethodPost, hostA, "/api/v1/admin/auth/session", `{"token":"x"}`, "", "no-such-shop")
	if got := problemType(t, w, http.StatusUnauthorized, "未认证的换会话接口带头"); got != problem.TypeUnauthorized {
		t.Fatalf("换会话接口带了一个不存在的 code，type 是 %q，期望 %q（头不该被读）", got, problem.TypeUnauthorized)
	}
}

// 规则 4：code 不存在 → 422，**不回落**到 Host 那家。读写都是。
func TestUnknownMerchantCodeIsRejectedNotFallenBack(t *testing.T) {
	a := newAdminShop(t)
	mkCategory(t, a, "Host那家的-"+a.Suffix)
	token := newPlatformAdmin(t)

	for _, code := range []string{"no-such-shop", " "} {
		w := switchReq(t, nil, http.MethodGet, a.Host, "/api/v1/admin/categories", "", token, code)
		if got := problemType(t, w, http.StatusUnprocessableEntity, "不存在的 code 读"); got != problem.TypeUnknownMerchant {
			t.Fatalf("code=%q：type 是 %q，期望 %q", code, got, problem.TypeUnknownMerchant)
		}
		if strings.Contains(w.Body.String(), "Host那家的") {
			t.Fatalf("code=%q 查不到，响应里却是 Host 那家的数据 —— 回落了", code)
		}
	}

	name := "不该落在Host那家-" + a.Suffix
	w := switchReq(t, nil, http.MethodPost, a.Host, "/api/v1/admin/categories",
		fmt.Sprintf(`{"name":%q}`, name), token, "no-such-shop")
	if got := problemType(t, w, http.StatusUnprocessableEntity, "不存在的 code 写"); got != problem.TypeUnknownMerchant {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeUnknownMerchant)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM categories WHERE name = $1`, name); n != 0 {
		t.Fatalf("code 查不到，类目却建出来了 %d 行 —— 回落到了 Host 那家", n)
	}
}

// 单商家部署里开店：409 single-merchant-mode，而且库里没有多出一家店。
//
// 用一套单独装的路由（DefaultCode = shop-a），不动包级实例——理由同
// TestPaymentIntentIsRefusedWhenSandboxIsOff。
func TestSingleMerchantModeRefusesToOpenAShop(t *testing.T) {
	single := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{DefaultCode: "shop-a"}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})
	token := newPlatformAdmin(t)
	code := fmt.Sprintf("single%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)
	before := adminQueryInt64(t, `SELECT count(*) FROM merchants`)

	w := switchReq(t, single, http.MethodPost, "anything.local", "/api/v1/admin/merchants",
		fmt.Sprintf(`{"code":%q,"name":"单商家里开的","admin_email":%q}`, code, code+"@keel.test"), token, "")
	if got := problemType(t, w, http.StatusConflict, "单商家部署开店"); got != problem.TypeSingleMerchantMode {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeSingleMerchantMode)
	}
	if after := adminQueryInt64(t, `SELECT count(*) FROM merchants`); after != before {
		t.Fatalf("接口回了 409，merchants 却从 %d 行变成了 %d 行 —— 闸门在写之后", before, after)
	}

	// 列表里如实说出这件事，后台据此把开店按钮置灰。
	var list api.MerchantList
	decodeInto(t, switchReq(t, single, http.MethodGet, "anything.local", "/api/v1/admin/merchants", "", token, ""),
		http.StatusOK, "单商家部署的商家列表", &list)
	if !list.SingleMerchantMode {
		t.Fatal("单商家部署的商家列表里 single_merchant_mode 是 false")
	}
	decodeInto(t, switchReq(t, nil, http.MethodGet, hostA, "/api/v1/admin/merchants", "", token, ""),
		http.StatusOK, "多商家部署的商家列表", &list)
	if list.SingleMerchantMode {
		t.Fatal("多商家部署的商家列表里 single_merchant_mode 是 true")
	}
}

// 单商家部署里停用那唯一一家店：409。停掉之后启动自检过不了。
func TestSingleMerchantModeRefusesToDisableTheDefaultShop(t *testing.T) {
	single := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{DefaultCode: "shop-a"}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})
	token := newPlatformAdmin(t)
	shopA := adminQueryInt64(t, `SELECT id FROM merchants WHERE code = 'shop-a'`)
	revs := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, shopA)

	w := switchReq(t, single, http.MethodPatch, "anything.local",
		fmt.Sprintf("/api/v1/admin/merchants/%d", shopA), `{"status":2}`, token, "")
	if got := problemType(t, w, http.StatusConflict, "单商家部署停用默认商家"); got != problem.TypeSingleMerchantMode {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeSingleMerchantMode)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, shopA); n != revs {
		t.Fatalf("接口回了 409，修订却多了 %d 行", n-revs)
	}
}

// 停用之后：买家侧 404；平台管理员仍能切进去、能启用；启用之后买家侧恢复。
//
// 这一套路由用 1 纳秒的解析缓存：包级实例的缓存是 30 秒（生产默认值），
// 而这家店在夹具里刚被解析过一次（换会话那一步），用它的话「停用后 404」
// 要等半分钟——那是生产上可接受的生效延迟，不是这条测试要验的东西。
func TestDisabledMerchantIs404ForBuyersButSwitchable(t *testing.T) {
	engine := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain, CacheTTL: time.Nanosecond}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})
	b := newAdminShop(t)
	mkCategory(t, b, "停用后还在的-"+b.Suffix)
	token := newPlatformAdmin(t)
	path := fmt.Sprintf("/api/v1/admin/merchants/%d", b.MerchantID)

	wantStatus(t, switchReq(t, engine, http.MethodGet, b.Host, "/api/v1/categories", "", "", ""),
		http.StatusOK, "停用前买家侧")

	var m api.Merchant
	decodeInto(t, switchReq(t, engine, http.MethodPatch, hostA, path, `{"status":2}`, token, ""),
		http.StatusOK, "停用", &m)
	if m.Status != 2 {
		t.Fatalf("停用之后响应里的 status 是 %d", m.Status)
	}

	if got := problemType(t, switchReq(t, engine, http.MethodGet, b.Host, "/api/v1/categories", "", "", ""),
		http.StatusNotFound, "停用后买家侧"); got != problem.TypeNotFound {
		t.Fatalf("停用后买家侧 type 是 %q，期望 %q", got, problem.TypeNotFound)
	}

	// 平台管理员从别家的 Host 切进去，读得到那家店的数据。
	got := categoryNames(t, switchReq(t, engine, http.MethodGet, hostA, "/api/v1/admin/categories", "", token, shopCode(b)),
		"切进一家停用的店")
	if !got["停用后还在的-"+b.Suffix] {
		t.Fatalf("切进停用的 B 店读到的类目是 %v —— 平台管理员得进得去才修得好", got)
	}

	// 列表含停用的店（契约：不含的话就没有入口把它启用回来）。
	var list api.MerchantList
	decodeInto(t, switchReq(t, engine, http.MethodGet, hostA, "/api/v1/admin/merchants?page_size=100", "", token, ""),
		http.StatusOK, "商家列表", &list)
	found := false
	for _, it := range list.Items {
		if it.Id == b.MerchantID {
			found = it.Status == 2
		}
	}
	if !found {
		t.Fatal("商家列表里没有这家停用的店（或者它的 status 不是 2）")
	}

	decodeInto(t, switchReq(t, engine, http.MethodPatch, hostA, path, `{"status":1,"name":"启用回来"}`, token, ""),
		http.StatusOK, "启用", &m)
	if m.Status != 1 || m.Name != "启用回来" || m.UpdatedAt == nil {
		t.Fatalf("启用之后响应是 %+v", m)
	}
	wantStatus(t, switchReq(t, engine, http.MethodGet, b.Host, "/api/v1/categories", "", "", ""),
		http.StatusOK, "启用后买家侧")

	// merchants 那一行一个字节都没被改：改名与停用走的是追加修订。
	if name := adminQueryText(t, `SELECT name FROM merchants WHERE id = $1`, b.MerchantID); name != "商家写路径店" {
		t.Fatalf("merchants.name 被改成了 %q —— 应用侧在 merchants 上本该没有 UPDATE", name)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, b.MerchantID); n != 2 {
		t.Fatalf("两次修改应该追加 2 行修订，实际 %d 行", n)
	}
	adminExec(t, `DELETE FROM merchant_revisions WHERE merchant_id = $1`, b.MerchantID)
}

// 商家目录只给平台级：商家级管理员看列表、看详情、改名都是 403。
func TestMerchantDirectoryIsPlatformOnly(t *testing.T) {
	a := newAdminShop(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/merchants", ""},
		{http.MethodGet, fmt.Sprintf("/api/v1/admin/merchants/%d", a.MerchantID), ""},
		{http.MethodPatch, fmt.Sprintf("/api/v1/admin/merchants/%d", a.MerchantID), `{"name":"自己改自己的店名"}`},
	} {
		w := switchReq(t, nil, c.method, a.Host, c.path, c.body, a.Token, "")
		if got := problemType(t, w, http.StatusForbidden, c.method+" "+c.path); got != problem.TypePlatformOnly {
			t.Errorf("%s %s：type 是 %q，期望 %q", c.method, c.path, got, problem.TypePlatformOnly)
		}
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, a.MerchantID); n != 0 {
		t.Fatalf("403 之后修订表里有 %d 行 —— 鉴权在写之后", n)
	}
}

// 写权限的第二道防线在数据库上：租户作用域里的 keel_app 插不进 merchant_revisions。
//
// 这是「不给 keel_app 加 UPDATE、改走追加修订」这个决定真正换来的东西：
// 即使应用代码里某处 bug 在租户作用域里发了这条 INSERT，库也会拒绝（42501）。
// 用 UPDATE 的话，这一层根本不存在。
func TestTenantScopeCannotReviseTheMerchantDirectory(t *testing.T) {
	ctx := context.Background()
	shopB := adminQueryInt64(t, `SELECT id FROM merchants WHERE code = 'shop-b'`)
	shopA := adminQueryInt64(t, `SELECT id FROM merchants WHERE code = 'shop-a'`)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, fmt.Sprint(shopA)); err != nil {
		t.Fatal(err)
	}
	// 「租户 1 的上下文里把租户 2 停用」——00005 实测过的那条越权。
	_, err = tx.Exec(ctx,
		`INSERT INTO merchant_revisions (merchant_id, name, status, changed_by) VALUES ($1, 'x', 2, 1)`, shopB)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("租户作用域里插修订的结果是 %v，期望 42501（RLS 拒绝）", err)
	}

	// 对照：UPDATE merchants 也一样拒绝——keel_app 在那张表上没有 UPDATE。
	tx2, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	_, err = tx2.Exec(ctx, `UPDATE merchants SET status = 2 WHERE id = $1`, shopB)
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("keel_app UPDATE merchants 的结果是 %v，期望 42501（没有 UPDATE 权限）", err)
	}
}
