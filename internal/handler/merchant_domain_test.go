package handler_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 商家自有域名的登记（PATCH /admin/merchants/{id} 的 domain，迁移 00340）。
//
// ===========================================================================
// 这一组测试的重心：那条接口**改的是这家店的入口**
// ===========================================================================
//
// 一个字段写错了会写坏整条租户解析：登记成功的域名必须是解析器采纳的那个文本，
// 而登记失败的必须是**一行都没写**。两边都要在库里看，只看响应码两头都看不住：
//
//   - 响应里 domain 正确、库里存的是原始大小写 → 接口绿，而这家店对新域名的每一个
//     请求都是 404（解析器比的是 normalizeHost(Host)）。
//   - 回 409 却留下半笔修改 → 接口绿，而这家店的店名被一次没成功的改动改掉了。
//
// 所以每条拒绝都配一条阳性对照（同一个域名换个合法写法必须成），
// 每条写都补一次库里回读。这与 webhook 那一组的写法是同一个理由。
//
// 解析缓存：这一组用一套单独装的路由，TTL 取 1 纳秒。包级 testEngine 的缓存是
// 30 秒（生产默认值），而「换绑之后旧域名 404」正是这条要断言的事 ——
// 30 秒的生效延迟是生产上接受的行为，不是这条测试要验的东西。
// 理由与写法同 TestDisabledMerchantIs404ForBuyersButSwitchable。

// domainEngine 装一套解析缓存只有 1 纳秒的路由。
func domainEngine(t *testing.T) *gin.Engine {
	t.Helper()
	return app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain, CacheTTL: time.Nanosecond}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})
}

// openFreshShop 用平台管理员开一家新店，返回它和它第一个管理员的 staff.id。
//
// 为什么每次都开新店、不用种子里那几家：登记域名改的是入口，动 shop-a / shop-b 的
// 域名会把同一个包里别的测试的 Host 换掉（它们正拿着那两个 Host 做租户隔离的断言）。
// 新店由 dropShop 连人带行收掉。
func openFreshShop(t *testing.T, token string) (api.Merchant, int64) {
	t.Helper()
	code := fmt.Sprintf("dom%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)

	email := code + "-boss@keel.test"
	var m api.Merchant
	decodeInto(t, postWithKey(t, hostA, "/api/v1/admin/merchants",
		fmt.Sprintf(`{"code":%q,"name":"域名测试店","admin_email":%q}`, code, email),
		token, "dom-"+code), http.StatusCreated, "开一家域名测试店", &m)

	id := adminQueryInt64(t, `SELECT id FROM staff WHERE email = $1`, email)
	if id <= 0 {
		t.Fatalf("新店没有第一个管理员（按邮箱数到 id=%d）", id)
	}
	return m, id
}

// patchDomain 打一次 PATCH 改这家店的域名，body 是 domain 那一段的原始 JSON
// （字符串、null、或者一个不该接受的东西 —— 三种都要能构造）。
func patchDomain(t *testing.T, engine *gin.Engine, id int64, token, domainJSON string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{}`
	if domainJSON != "" {
		body = `{"domain":` + domainJSON + `}`
	}
	return switchReq(t, engine, http.MethodPatch, hostA,
		fmt.Sprintf("/api/v1/admin/merchants/%d", id), body, token, "")
}

// registeredDomain 读库里这条登记，并顺带核对这家店只有一行（一家店一行）。
func registeredDomain(t *testing.T, merchantID int64) string {
	t.Helper()
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_domains WHERE merchant_id = $1`,
		merchantID); n != 1 {
		t.Fatalf("merchant_domains 里这家店有 %d 行，期望正好 1 行（换绑是先删后插，不是追加）", n)
	}
	return adminQueryText(t, `SELECT domain FROM merchant_domains WHERE merchant_id = $1`, merchantID)
}

// 主路径：登记一个自有域名，它当场成为这家店的入口；换绑与摘掉都干净。
func TestRegisteringADomainOpensThatDoor(t *testing.T) {
	token := newPlatformAdmin(t)
	engine := domainEngine(t)
	m, owner := openFreshShop(t, token)

	// 故意写成解析器要比的那个形状之外的一切：大写、结尾的点。
	// 存进库的必须是归一化之后的文本，否则这条登记不报错、不生效。
	first := fmt.Sprintf("My-Shop%d.Example.NET.", m.Id)
	firstNorm := fmt.Sprintf("my-shop%d.example.net", m.Id)

	var got api.Merchant
	decodeInto(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", first)),
		http.StatusOK, "登记自有域名", &got)
	if got.Domain == nil || *got.Domain != firstNorm {
		t.Fatalf("响应里的 domain 是 %v，期望归一化成 %q", got.Domain, firstNorm)
	}
	if s := registeredDomain(t, m.Id); s != firstNorm {
		t.Fatalf("库里存的是 %q，期望 %q —— 写进去的文本与解析器要比的文本不是同一个，"+
			"这条登记永远不会被采纳，而接口回的是 200", s, firstNorm)
	}
	// changed_by 是那条登记的审计（谁登记的）。NULL 意味着这条接口没把调用者带下去。
	if by := adminQueryInt64(t,
		`SELECT coalesce(changed_by, 0) FROM merchant_domains WHERE merchant_id = $1`, m.Id); by == 0 {
		t.Fatal("登记的 changed_by 是 NULL —— 平台级操作员是谁，这条记录里应当有")
	}

	// 域名真的通到这家店：拿这家店自己那个管理员的身份，从这个 Host 进后台。
	// 光看 404/200 不够 —— 它可能通到另一家店上（那正是这条接口最坏的失败）。
	sess := staffSession(t, m.Code+"."+baseDomain, owner)
	me := staffOf(t, switchReq(t, engine, http.MethodGet, firstNorm,
		"/api/v1/admin/me", "", sess.Token, ""), http.StatusOK)
	if me.MerchantId == nil || *me.MerchantId != m.Id {
		t.Fatalf("从 %q 进去看到的是 merchant_id=%v，期望这家店 %d —— 域名通到了别人家",
			firstNorm, me.MerchantId, m.Id)
	}

	// 换绑：旧的入口当场关掉，新的打开，而库里仍然只有一行。
	second := fmt.Sprintf("shop%d.example.org", m.Id)
	decodeInto(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", second)),
		http.StatusOK, "换绑", &got)
	if got.Domain == nil || *got.Domain != second {
		t.Fatalf("换绑之后响应里的 domain 是 %v，期望 %q", got.Domain, second)
	}
	if s := registeredDomain(t, m.Id); s != second {
		t.Errorf("换绑之后库里是 %q", s)
	}
	wantStatus(t, switchReq(t, engine, http.MethodGet, second, "/api/v1/categories", "", "", ""),
		http.StatusOK, "换绑之后的新域名")
	// 旧域名不再是任何一家店的入口。
	wantStatus(t, switchReq(t, engine, http.MethodGet, firstNorm, "/api/v1/categories", "", "", ""),
		http.StatusNotFound, "换绑之后被摘掉的旧域名")

	// 摘掉：显式 null 是一个功能（域名转让给别人之前得能先摘下来）。
	// 解码进一个新结构体：Domain 是 `*string,omitempty`，复用 got 的话
	// 「响应里根本没有 domain 这个键」会留下上一轮那个旧值，看着像没摘掉。
	var cleared api.Merchant
	decodeInto(t, patchDomain(t, engine, m.Id, token, `null`), http.StatusOK, "摘掉域名", &cleared)
	if cleared.Domain != nil {
		t.Errorf("摘掉之后响应里还有 domain %q", *cleared.Domain)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_domains WHERE merchant_id = $1`,
		m.Id); n != 0 {
		t.Fatalf("摘掉之后库里还剩 %d 行 —— 「没有那一行」才是没登记", n)
	}
	wantStatus(t, switchReq(t, engine, http.MethodGet, second, "/api/v1/categories", "", "", ""),
		http.StatusNotFound, "摘掉之后的域名")

	// 摘掉之后子域名那条路还在（这家店从没登记域名之前就是靠它进场的）。
	wantStatus(t, switchReq(t, engine, http.MethodGet, m.Code+"."+baseDomain,
		"/api/v1/categories", "", "", ""), http.StatusOK, "摘掉之后的子域名入口")
}

// 三个态里最贵的那一个：**不传 domain 不等于清空它**。
//
// 这条接口的 domain 与 name 混在同一个 PATCH 里，而「只改店名」是后台最常用的
// 一次调用。如果生成类型上那个 *string 的 nil 被当成「清空」，那么每一次改名
// 都会顺手把这家店的入口摘掉 —— 摘掉的后果是它立刻对全部买家 404，
// 而响应回的是 200、店名也确实改了。
func TestPatchingWithoutDomainLeavesTheEntranceAlone(t *testing.T) {
	token := newPlatformAdmin(t)
	engine := domainEngine(t)
	m, _ := openFreshShop(t, token)

	domain := fmt.Sprintf("keep%d.example.net", m.Id)
	decodeInto(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", domain)),
		http.StatusOK, "先登记一个域名", &api.Merchant{})

	name := "只改名字"
	var got api.Merchant
	decodeInto(t, switchReq(t, engine, http.MethodPatch, hostA,
		fmt.Sprintf("/api/v1/admin/merchants/%d", m.Id),
		fmt.Sprintf(`{"name":%q}`, name), token, ""), http.StatusOK, "只改店名", &got)
	if got.Name != name {
		t.Fatalf("店名是 %q，期望 %q", got.Name, name)
	}
	if got.Domain == nil || *got.Domain != domain {
		t.Fatalf("只改店名把 domain 变成了 %v —— 期望原样 %q。"+
			"「不传」与「显式 null」必须是两件事（判据是请求体里这个键出现过没有，"+
			"见 bindPatchBody）",
			got.Domain, domain)
	}
	if s := registeredDomain(t, m.Id); s != domain {
		t.Fatalf("库里变成了 %q，期望 %q", s, domain)
	}
	// 入口还在。
	wantStatus(t, switchReq(t, engine, http.MethodGet, domain, "/api/v1/categories", "", "", ""),
		http.StatusOK, "改名之后的域名")

	// 一个字段都没传：422，而不是「静默追加一行修订」。
	revs := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, m.Id)
	wantStatus(t, patchDomain(t, engine, m.Id, token, ""),
		http.StatusUnprocessableEntity, "一个字段都没传")
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`,
		m.Id); n != revs {
		t.Fatalf("空 PATCH 追加了 %d 行修订 —— 什么都没改却留下审计", n-revs)
	}
}

// 落在平台基础域名之下的登记一律 422：那片地盘的名字归平台，解析器在那里只认 code。
//
// 这一条不是「难看一点也没事」：那种行存进去之后**永远不被采纳**，
// 而接口回 200。运营看到域名已经填上了，买家那边却是 404。
// 判据与解析器共用同一个表达式（tenant.DomainUnderBase），所以这里要把
// 那三种形状分开验：恰好一级、apex 本身、多级。
func TestRegisteringADomainUnderTheBaseDomainIsRejected(t *testing.T) {
	token := newPlatformAdmin(t)
	engine := domainEngine(t)
	m, _ := openFreshShop(t, token)

	for _, c := range []struct{ domain, why string }{
		{baseDomain, "apex 本身：放行等于商家能登记平台主站"},
		{"shop-a." + baseDomain, "恰好一级：那一支只认 code"},
		{"a.b." + baseDomain, "多级：也在基础域名这片地盘里"},
		{"admin.internal." + baseDomain, "平台将来要用的后台域名"},
		{fmt.Sprintf("shop%d.%s", m.Id, baseDomain), "与自己的 code 长得像也一样拒"},
		{"SHOP-A." + baseDomain, "大写不能绕过这道闸门（归一化之后仍然落在基础域名下）"},
	} {
		got := problemType(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", c.domain)),
			http.StatusUnprocessableEntity, "登记"+c.domain)
		if got != problem.TypeInvalidRequest {
			t.Errorf("[%s] type 是 %q，期望 %q", c.why, got, problem.TypeInvalidRequest)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_domains WHERE merchant_id = $1`,
			m.Id); n != 0 {
			t.Fatalf("[%s] 422 之后这家店却有 %d 行域名登记 —— 闸门在写之后", c.why, n)
		}
	}

	// 阳性对照：同一个商家、同一个域名形状，落在基础域名之外就必须成。
	// 没有它，上面那六条「被拒」也可能只是因为这条接口整个坏了。
	ok := fmt.Sprintf("out%d.example.net", m.Id)
	decodeInto(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", ok)),
		http.StatusOK, "登记基础域名之外的域名", &api.Merchant{})
	if s := registeredDomain(t, m.Id); s != ok {
		t.Fatalf("库里是 %q，期望 %q", s, ok)
	}
}

// 不是一个域名的东西一律 422，而**不是 500、也不是静默收下**。
//
// "" 与 "example"（单段）尤其要紧：过不了的话，登记一个 "com" 就占了整个顶级域。
// `{"domain": 123}` 走的是另一条路径（handler 的原始 JSON 判别），
// 它必须拒而不是被 json.Unmarshal 悄悄塞成一个字符串。
func TestRegisteringSomethingThatIsNotADomainIsRejected(t *testing.T) {
	token := newPlatformAdmin(t)
	engine := domainEngine(t)
	m, _ := openFreshShop(t, token)

	for _, c := range []struct{ json, why string }{
		{`""`, "空串：清空它是另一件事（要写 null）"},
		{`"example"`, "单段：过不了就等于能占住一个顶级域"},
		{`"com"`, "同上，而且这是最坏的那个"},
		{`"not a domain"`, "含空格"},
		{`"shop_example.net"`, "下划线不是 DNS 标签"},
		{`"-shop.example.net"`, "标签以连字符开头"},
		{`"shop-.example.net"`, "标签以连字符结尾"},
		{`"shop..example.net"`, "空标签"},
		{`123`, "不是字符串也不是 null"},
		{`["shop.example.net"]`, "不是字符串也不是 null"},
		{`{"a":1}`, "不是字符串也不是 null"},
	} {
		got := problemType(t, patchDomain(t, engine, m.Id, token, c.json),
			http.StatusUnprocessableEntity, "登记 "+c.json)
		if got != problem.TypeInvalidRequest {
			t.Errorf("[%s] type 是 %q，期望 %q", c.why, got, problem.TypeInvalidRequest)
		}
		if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_domains WHERE merchant_id = $1`,
			m.Id); n != 0 {
			t.Fatalf("[%s] 422 之后却有 %d 行登记", c.why, n)
		}
	}

	// 阳性对照。
	decodeInto(t, patchDomain(t, engine, m.Id, token,
		fmt.Sprintf(`"fine%d.example.net"`, m.Id)), http.StatusOK, "登记一个合法域名",
		&api.Merchant{})

	// 带端口不是「拒」而是「归一化掉」：normalizeHost 去端口是因为 Host 头上就带端口，
	// 登记的字符串与要比的文本必须走同一个函数。这里断言的是存进去的那个文本，
	// 不是状态码 —— 收下原始串同样是 200，而那条登记永远匹配不上。
	withPort := fmt.Sprintf("port%d.example.net.:8443", m.Id)
	want := fmt.Sprintf("port%d.example.net", m.Id)
	var got api.Merchant
	decodeInto(t, patchDomain(t, engine, m.Id, token, fmt.Sprintf("%q", withPort)),
		http.StatusOK, "登记一个带端口、带结尾点、带大写的域名", &got)
	if got.Domain == nil || *got.Domain != want {
		t.Fatalf("响应里的 domain 是 %v，期望 %q", got.Domain, want)
	}
	if s := registeredDomain(t, m.Id); s != want {
		t.Fatalf("库里存的是 %q，期望 %q", s, want)
	}
}

// 一个域名只能是一家店的入口：撞车回 409，而且整笔回滚。
//
// 查重由 merchant_domains.domain 上那条**全表** UNIQUE 报的 23505 来，不是先查后写
// （两家店同时登记同一个域名时两边都会通过先查，而数据库只让一边提交）。
// 所以这条测试要看的是：撞了之后**一家店的域名都没被改到**，
// 而那笔失败的 PATCH 里连一行修订都不该留下 —— 域名与店名在同一个事务里。
func TestOneDomainIsOneShopsEntrance(t *testing.T) {
	token := newPlatformAdmin(t)
	engine := domainEngine(t)
	a, _ := openFreshShop(t, token)
	b, _ := openFreshShop(t, token)

	shared := fmt.Sprintf("prize%d.example.net", time.Now().UnixNano()%1_000_000_000)
	decodeInto(t, patchDomain(t, engine, a.Id, token, fmt.Sprintf("%q", shared)),
		http.StatusOK, "A 先登记", &api.Merchant{})

	// B 同时想登记它，而且顺手改个名 —— 名字也不能被改走。
	bRevs := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, b.Id)
	w := switchReq(t, engine, http.MethodPatch, hostA,
		fmt.Sprintf("/api/v1/admin/merchants/%d", b.Id),
		fmt.Sprintf(`{"name":"抢来的名字","domain":%q}`, shared), token, "")
	if got := problemType(t, w, http.StatusConflict, "B 登记 A 已有的域名"); got != problem.TypeMerchantDomainTaken {
		t.Fatalf("type 是 %q，期望 %q", got, problem.TypeMerchantDomainTaken)
	}

	if s := registeredDomain(t, a.Id); s != shared {
		t.Errorf("A 的域名被改成了 %q —— 两家店抢一个域名，赢家应当是先登记的那个", s)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_domains WHERE merchant_id = $1`, b.Id); n != 0 {
		t.Fatalf("B 撞车之后却有 %d 行登记", n)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchant_revisions WHERE merchant_id = $1`, b.Id); n != bRevs {
		t.Fatalf("B 的 PATCH 失败了，却追加了 %d 行修订（名字被改走了）—— "+
			"域名与修订不在同一个事务里", n-bRevs)
	}

	// 阳性对照一：B 换一个域名就成（不是这条接口整个坏了）。
	decodeInto(t, patchDomain(t, engine, b.Id, token,
		fmt.Sprintf(`"own%d.example.net"`, b.Id)), http.StatusOK, "B 登记自己的域名",
		&api.Merchant{})

	// 阳性对照二：**同一家店**重登记自己那个域名不是撞车。
	// 换绑是先删后插，删掉了自己那一行，所以全表 UNIQUE 不会把自己撞成 409。
	// 这条尤其要紧：把它改成「先查一遍有没有人用」的实现，两条都会红，
	// 而先删后插在这里必须是绿的。
	decodeInto(t, patchDomain(t, engine, a.Id, token, fmt.Sprintf("%q", shared)),
		http.StatusOK, "A 重登记自己的域名", &api.Merchant{})
	if s := registeredDomain(t, a.Id); s != shared {
		t.Errorf("重登记之后库里是 %q", s)
	}
}

// 单商家部署里不接受域名登记：那种行不报错、不生效。
//
// 单商家模式完全不解析 Host（Resolve 第一行就返回默认商家），所以登记进来的域名
// 永远不会被采纳 —— 而运营看到域名填上了。清空它同理：这套部署里没有「域名」
// 这个概念，两种写法一起拒，而不是一个 409 一个 200。
// 改名不受影响，所以阳性对照用改名。
func TestSingleMerchantModeRefusesDomainRegistration(t *testing.T) {
	token := newPlatformAdmin(t)
	multi := domainEngine(t)
	single := app.Router(testPool,
		tenant.NewResolver(testPool, tenant.Config{DefaultCode: "shop-a"}),
		testSigner, testOrders, service.PaymentConfig{Sandbox: true}, conceptEmbedder{})

	m, _ := openFreshShop(t, token)
	domain := fmt.Sprintf("single%d.example.net", m.Id)
	decodeInto(t, patchDomain(t, multi, m.Id, token, fmt.Sprintf("%q", domain)),
		http.StatusOK, "多商家形态下先登记上", &api.Merchant{})

	for _, c := range []struct{ json, why string }{
		{fmt.Sprintf("%q", domain), "登记"},
		{fmt.Sprintf("%q", fmt.Sprintf("other%d.example.net", m.Id)), "换一个域名也是同一件事"},
		// 形状判在部署形态判之后：这套部署里没有域名这个概念，填什么都一样不行。
		{`"com"`, "形状不合法的值"},
	} {
		got := problemType(t, patchDomain(t, single, m.Id, token, c.json),
			http.StatusConflict, "单商家里"+c.why)
		if got != problem.TypeSingleMerchantMode {
			t.Errorf("[%s] type 是 %q，期望 %q", c.why, got, problem.TypeSingleMerchantMode)
		}
	}
	// 摘掉同样拒：这条接口在这套部署里没有意义。
	if got := problemType(t, patchDomain(t, single, m.Id, token, `null`),
		http.StatusConflict, "单商家里摘掉域名"); got != problem.TypeSingleMerchantMode {
		t.Errorf("type 是 %q，期望 %q", got, problem.TypeSingleMerchantMode)
	}

	// 库里那条登记一个字节都没动（前面那次是多商家形态写的）。
	if s := registeredDomain(t, m.Id); s != domain {
		t.Fatalf("单商家形态的几次失败调用把域名改成了 %q，期望 %q", s, domain)
	}

	// 阳性对照：改名不受影响。
	decodeInto(t, switchReq(t, single, http.MethodPatch, "anything.local",
		fmt.Sprintf("/api/v1/admin/merchants/%d", m.Id), `{"name":"单商家里改个名"}`, token, ""),
		http.StatusOK, "单商家里改名", &api.Merchant{})
}
