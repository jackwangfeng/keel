package handler_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/problem"
)

// 开店（POST /admin/merchants）。M4 收尾。
//
// ===========================================================================
// 这一组测试真正要钉住的是**作用域切换**，不是「接口返回 201」
// ===========================================================================
//
// 开店那条链路上有一个不报错的失败方向：建完 merchants 那一行之后忘了把
// app.platform_scope 关掉，于是 staff_scope_merchant() 仍然返回 NULL，
// 新店的第一个管理员被建成一个 **merchant_id 为 NULL 的平台级管理员** ——
// 一个拥有跨租户运维权、能继续开店的账号。
//
// 那条路上没有任何约束会拦：staff.merchant_id 本来就可空，RLS 的写谓词
// （merchant_id IS NOT DISTINCT FROM staff_scope_merchant()）在平台作用域下
// 对 NULL 行恰好为真。接口照样回 201，响应体一个字都不会变 ——
// 契约里 201 的 schema 是 Merchant，里面根本没有 staff。
//
// 所以下面第一条测试**必须去库里看那一列**，光看响应体的断言在这件事上
// 是全盲的。

// newPlatformAdmin 造一个平台级管理员并换一串会话，返回 token。
//
// 不走 EnsureBootstrapAdmin：那条路要求「库里一个在岗平台管理员都没有」，
// 于是它必须先 wipeStaff 把整张表清掉 —— 而这个包里别的测试正拿着自己那家
// 店的 staff 行。引导那条路径有它自己的测试守着（admin_auth_test.go），
// 这里要的只是「一个平台级身份」。
func newPlatformAdmin(t *testing.T) string {
	t.Helper()
	email := fmt.Sprintf("opener-%d@keel.test", time.Now().UnixNano())
	id := mkStaff(t, "", email, 1, 1)
	t.Cleanup(func() { adminExec(t, `DELETE FROM staff WHERE id = $1`, id) })
	// Host 随便给一家已有的店：平台级令牌对任何一家店放行
	// （auth.StaffClaims.TenantMatches），而那家店与开出来的新店无关。
	return staffSession(t, hostA, id).Token
}

// dropShop 把一家刚开出来的店连人带行删掉。
//
// 应用侧在 merchants 上**没有** DELETE（00021 只还回了 INSERT），
// 所以清理只能走管理员连接 —— 这件事本身就是那条 GRANT 面的一次确认。
//
// 子表按外键一条条删，不能指望 CASCADE：merchant_revisions 与 merchant_domains
// 都 REFERENCES merchants(id) 而没写级联删除（一张追加式日志、一份当前态，
// 谁都不该跟着店消失而被静默清掉）。删的顺序是先子后父。
func dropShop(t *testing.T, code string) {
	t.Helper()
	t.Cleanup(func() {
		adminExec(t, `DELETE FROM staff_tokens WHERE staff_id IN
		              (SELECT id FROM staff WHERE merchant_id =
		                 (SELECT id FROM merchants WHERE code = $1))`, code)
		adminExec(t, `DELETE FROM staff WHERE merchant_id =
		              (SELECT id FROM merchants WHERE code = $1)`, code)
		adminExec(t, `DELETE FROM merchant_revisions WHERE merchant_id =
		              (SELECT id FROM merchants WHERE code = $1)`, code)
		adminExec(t, `DELETE FROM merchant_domains WHERE merchant_id =
		              (SELECT id FROM merchants WHERE code = $1)`, code)
		adminExec(t, `DELETE FROM merchants WHERE code = $1`, code)
	})
}

// 主路径 + 那条不报错的失败方向：开出来的店真的存在，它的第一个管理员
// **属于这家新店**（不是平台级、也不是调用者那家店），而且他真的能登进去。
func TestOpeningAShopPutsItsFirstAdminInTheNewTenant(t *testing.T) {
	token := newPlatformAdmin(t)
	code := fmt.Sprintf("newshop%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)

	adminEmail := code + "-boss@keel.test"
	var m api.Merchant
	decodeInto(t, postWithKey(t, hostA, "/api/v1/admin/merchants",
		fmt.Sprintf(`{"code":%q,"name":"新开的店","admin_email":%q}`, code, adminEmail),
		token, "open-"+code), http.StatusCreated, "开店", &m)

	if m.Code != code || m.Id <= 0 {
		t.Fatalf("响应里的 merchant 是 %+v，期望 code=%q 且 id > 0", m, code)
	}
	if m.Status != 1 {
		t.Errorf("新店的 status 是 %d，期望 1 正常 —— status 不是一个入参（db/queries/merchants.sql）", m.Status)
	}
	if m.Domain != nil {
		t.Errorf("新店带上了 domain %q —— 开店这条路刻意不写 shop_settings（00021 文件头）", *m.Domain)
	}

	// —— 这一段是整条测试的重点：那个管理员落在哪个租户里。
	//
	// merchant_id 用 coalesce(..., 0) 读出来：0 在这张表上不是一个可能的
	// 租户 id（merchants.id 是 IDENTITY，从 1 开始），所以它能无歧义地表示
	// 「这一列是 NULL」，也就是「这个人是平台级的」。
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM staff WHERE email = $1`, adminEmail); n != 1 {
		t.Fatalf("按邮箱数到 %d 行 staff，期望正好 1 —— 新店的第一个管理员没建出来", n)
	}
	ownerID := adminQueryInt64(t, `SELECT id FROM staff WHERE email = $1`, adminEmail)
	ownerMerchant := adminQueryInt64(t,
		`SELECT coalesce(merchant_id, 0) FROM staff WHERE email = $1`, adminEmail)
	ownerRole := adminQueryInt64(t, `SELECT role FROM staff WHERE email = $1`, adminEmail)
	if ownerMerchant == 0 {
		t.Fatalf("新店的第一个管理员 merchant_id 是 NULL —— 他是个**平台级管理员**，"+
			"能跨租户运维、能继续开店。这说明建 merchants 之后没有把作用域切到新店"+
			"（app.platform_scope 没关，staff_scope_merchant() 还是 NULL）。"+
			"落点在 repository.enterTenantScope。staff_id=%d", ownerID)
	}
	if ownerMerchant != m.Id {
		t.Fatalf("新店的第一个管理员挂在 merchant_id=%d 上，而新店是 %d —— "+
			"作用域切到了别人家", ownerMerchant, m.Id)
	}
	if ownerRole != 1 {
		t.Errorf("新店的第一个管理员 role 是 %d，期望 1 管理员 —— "+
			"一家一个管理员都没有的店，员工列表谁也打不开", ownerRole)
	}

	// 开出来的店真的能用：这个管理员能在新店的 Host 上换到会话，
	// 而且那串会话认得出自己属于这家店。
	//
	// 这里用我们自己造的一次性链接，而不是 201 里那一条：后者由
	// TestOpeningAShopHandsTheEntranceKeyToTheOpener 专门验（它验的是「那一串是真的、
	// 且只认这家新店」）。这条验的是「这家店 + 这个人」这套东西成立。
	host := code + "." + baseDomain
	sess := staffSession(t, host, ownerID)
	w := getAs(t, host, "/api/v1/admin/me", sess.Token)
	me := staffOf(t, w, http.StatusOK)
	if me.MerchantId == nil || *me.MerchantId != m.Id {
		t.Fatalf("新店管理员的 /admin/me 回的 merchant_id 是 %v，期望 %d", me.MerchantId, m.Id)
	}
}

// ===========================================================================
// 开店把「进这家店的钥匙」交给开店的人（C：一次性 token 的交付）
// ===========================================================================
//
// 这条接口给的凭据是**唯一一个能进这家新店的账号**。平台会话看不见商家的员工
// （POST /admin/staff/{id}/login-token 对一个商家级 staff 回 404，那条边界是刻意做的，
// 见 permission_test.go 与 TestReissueLoginTokenScopes），所以如果 201 不带这一串，
// 开店的人除了翻容器日志就没有第二条路 —— 而日志会被采集、会进索引。
//
// 所以这里断言的不是「响应里多了一个字段」，而是**那一串当场能用、且只对这家新店用**：
// 一个字段名对了但内容错了（别人家的 token、一串没入库的随机串、或者过期时间算错）
// 在这条接口的形状上看不出来，只在第一次登录时炸。

func TestOpeningAShopHandsTheEntranceKeyToTheOpener(t *testing.T) {
	token := newPlatformAdmin(t)
	code := fmt.Sprintf("keyshop%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)

	adminEmail := code + "-boss@keel.test"
	var out api.MerchantOpened
	w := postWithKey(t, hostA, "/api/v1/admin/merchants",
		fmt.Sprintf(`{"code":%q,"name":"交钥匙的店","admin_email":%q}`, code, adminEmail),
		token, "open-"+code)
	decodeInto(t, w, http.StatusCreated, "开店并拿到凭据", &out)

	if out.AdminStaffId == nil {
		t.Fatalf("201 里没有 admin_staff_id：%s", w.Body.String())
	}
	if out.AdminLoginToken == nil || *out.AdminLoginToken == "" {
		t.Fatalf("201 里没有 admin_login_token：%s", w.Body.String())
	}
	if out.AdminLoginTokenExpireAt == nil {
		t.Fatalf("201 里没有 admin_login_token_expire_at：%s", w.Body.String())
	}

	// 响应里那个 id 必须就是库里那一个人的 id。这是这条接口唯一能把「凭据」和
	// 「人」对上的地方 —— 给错人的话，店主拿到的是别人家的门。
	if db := adminQueryInt64(t, `SELECT id FROM staff WHERE email = $1`, adminEmail); db != *out.AdminStaffId {
		t.Fatalf("admin_staff_id=%d，而库里这个邮箱对应的人 id=%d —— 凭据指向的不是新建的那管理员",
			*out.AdminStaffId, db)
	}

	// 失效时间必须服务端说了算，且与那一串入库的时间一致。自己加 15 分钟的话，
	// 响应说还有效而服务端已经拒了，客户端只会重试一个死掉的凭据。
	want := auth.StaffEmailLinkTTL
	if left := time.Until(*out.AdminLoginTokenExpireAt); left < want-2*time.Minute || left > want+time.Minute {
		t.Errorf("expire_at 距今 %v，期望约 %v", left, want)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM staff_tokens
		WHERE staff_id = $1 AND kind = 2 AND expire_at > now() AND revoked_at IS NULL`,
		*out.AdminStaffId); n != 1 {
		t.Fatalf("这个人名下活的一次性登录链接有 %d 行，期望正好 1 行", n)
	}

	// 重点：拿响应里这一串**真的去换会话**，而且只在新店的 Host 上换得出来。
	// 走真实的 POST /admin/auth/session，不自己签（同 staffSession 的理由）。
	body := `{"token":"` + *out.AdminLoginToken + `"}`
	host := code + "." + baseDomain
	var sess api.StaffSession
	decodeInto(t, post(t, host, "/api/v1/admin/auth/session", body, ""),
		http.StatusOK, "用开店拿到的凭据换会话", &sess)
	if sess.Staff.Id != *out.AdminStaffId {
		t.Fatalf("换到的会话是 staff=%d，期望 %d", sess.Staff.Id, *out.AdminStaffId)
	}
	if sess.Staff.MerchantId == nil || *sess.Staff.MerchantId != out.Id {
		t.Fatalf("换到的会话 merchant_id=%v，期望新开的店 %d", sess.Staff.MerchantId, out.Id)
	}

	// 换到会话之后 /admin/me 也认：这一串确实把人放进了他自己那家店。
	me := staffOf(t, getAs(t, host, "/api/v1/admin/me", sess.Token), http.StatusOK)
	if me.Id != *out.AdminStaffId {
		t.Fatalf("新店的 /admin/me 回的是 staff=%d，期望 %d", me.Id, *out.AdminStaffId)
	}

	// 用掉即失效（与 mkEmailLink 那条同一个纪律）：这条凭据走的是同一种 token。
	problemOf(t, post(t, host, "/api/v1/admin/auth/session", body, ""), http.StatusUnauthorized)
}

// 幂等重放**不带凭据**，而且凭据的明文**不在数据库里**。
//
// 这两件事是同一件事的两面，所以放在一条测试里：重放不能签第二串（那会让
// 「同一把幂等键 = 同一件事」变成「同一把键开出两个入口」），也不能回放第一串
// —— 回放要求它被存下来，而一次性凭据的明文不进数据库是仓库法律
// （idempotency_keys.response_body 存的是 repository.Merchant 那一份）。
// 有人为了「让重放也能拿到 token」把它写进存档时，这条会当场指出那串明文在库里。
func TestReplayedOpenShopCarriesNoCredential(t *testing.T) {
	token := newPlatformAdmin(t)
	code := fmt.Sprintf("replayshop%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)

	adminEmail := code + "-boss@keel.test"
	req := fmt.Sprintf(`{"code":%q,"name":"重放的店","admin_email":%q}`, code, adminEmail)
	key := "open-" + code

	var first api.MerchantOpened
	decodeInto(t, postWithKey(t, hostA, "/api/v1/admin/merchants", req, token, key),
		http.StatusCreated, "第一次开店", &first)
	if first.AdminLoginToken == nil {
		t.Fatalf("第一次开店没给凭据，后面两条断言都无从谈起")
	}

	w := postWithKey(t, hostA, "/api/v1/admin/merchants", req, token, key)
	var second api.MerchantOpened
	decodeInto(t, w, http.StatusCreated, "同一把幂等键重放", &second)
	if w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重放没有带 Idempotency-Replayed: true（是 %q）—— 那么它到底重放了什么？",
			w.Header().Get("Idempotency-Replayed"))
	}
	if second.Id != first.Id || second.Code != code {
		t.Fatalf("重放回来的不是第一家店（id=%d code=%q）", second.Id, second.Code)
	}
	if second.AdminStaffId != nil || second.AdminLoginToken != nil || second.AdminLoginTokenExpireAt != nil {
		t.Errorf("重放把凭据又给了一遍（%+v）—— 重放不签第二串，也不回放第一串", second)
	}
	// 原始字节层面也要干净：*string 为 nil 只说明解码后是 nil，
	// 「显式回了一个 null」与「这个键不存在」对客户端是两件事。
	if strings.Contains(w.Body.String(), "admin_login_token") {
		t.Errorf("重放的响应体里出现了 admin_login_token 这个键：%s", w.Body.String())
	}

	// 那次重放什么都没签：这个人名下仍然只有第一次那一行 token。
	if n := adminQueryInt64(t, `SELECT count(*) FROM staff_tokens
		WHERE staff_id = $1 AND kind = 2`, first.AdminStaffId); n != 1 {
		t.Errorf("重放之后这个人名下有 %d 行一次性登录链接，期望还是 1 行 —— 重放又签了一串", n)
	}

	// 而存档里没有那串明文。这一条是仓库法律的直接检查，不是风格问题。
	archived := adminQueryText(t, `SELECT coalesce(response_body::text, '') FROM idempotency_keys
		WHERE scope = 'admin.merchants.create' AND idem_key = $1`, key)
	if archived == "" {
		t.Fatal("幂等存档里读不到这一笔（scope 或键不对，还是根本没存？）")
	}
	if strings.Contains(archived, *first.AdminLoginToken) {
		t.Errorf("一次性凭据的明文进了 idempotency_keys.response_body ——「明文凭据绝不进数据库」")
	}
}

// 只有平台级管理员能开店（契约那个 403）。
//
// 商家级管理员是**最值得验的那一个**：他是管理员（role = 1），所以一个只判
// IsAdmin 的实现会放他过去 —— 而那意味着任何一个租户都能给自己再开一家店。
func TestOpeningAShopIsPlatformOnly(t *testing.T) {
	sh := newAdminShop(t) // 商家级管理员
	code := fmt.Sprintf("denied%d", time.Now().UnixNano()%1_000_000_000)

	got := problemType(t, postWithKey(t, sh.Host, "/api/v1/admin/merchants",
		fmt.Sprintf(`{"code":%q,"name":"不该开成","admin_email":"x@keel.test"}`, code),
		sh.Token, "denied-"+code), http.StatusForbidden, "商家级管理员开店")
	if got != problem.TypePlatformOnly {
		t.Errorf("Problem type 是 %q，期望 %q —— 与 staff-forbidden 分开报，"+
			"理由写在 problem.TypePlatformOnly 上", got, problem.TypePlatformOnly)
	}

	// 阳性对照：那家店真的没被开出来。只看状态码的话，一个「先建店再回 403」
	// 的实现也是绿的。
	if n := adminQueryInt64(t, `SELECT count(*) FROM merchants WHERE code = $1`, code); n != 0 {
		t.Fatalf("接口回了 403，库里却有 %d 行 code=%q 的店 —— 鉴权在写之后", n, code)
	}
}

// code 撞车回 409，而且**不是** 500：merchants.code 是全局唯一的
// （tenancy.json 的 unique_global_ok：它就是租户标识本身）。
func TestOpeningAShopWithATakenCodeConflicts(t *testing.T) {
	token := newPlatformAdmin(t)
	code := fmt.Sprintf("dup%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)

	body := fmt.Sprintf(`{"code":%q,"name":"先开的","admin_email":%q}`, code, code+"-1@keel.test")
	if w := postWithKey(t, hostA, "/api/v1/admin/merchants", body, token, "dup1-"+code); w.Code != http.StatusCreated {
		t.Fatalf("第一次开店失败：%d %s", w.Code, w.Body.String())
	}

	second := fmt.Sprintf(`{"code":%q,"name":"后开的","admin_email":%q}`, code, code+"-2@keel.test")
	got := problemType(t, postWithKey(t, hostA, "/api/v1/admin/merchants", second, token, "dup2-"+code),
		http.StatusConflict, "同一个 code 再开一次")
	if got != problem.TypeMerchantCodeTaken {
		t.Errorf("Problem type 是 %q，期望 %q", got, problem.TypeMerchantCodeTaken)
	}

	// 第二次那个管理员一行都没留下 —— 建店与建管理员在同一个事务里。
	if n := adminQueryInt64(t, `SELECT count(*) FROM staff WHERE email = $1`, code+"-2@keel.test"); n != 0 {
		t.Fatalf("第二次开店失败了，却留下了 %d 行 staff —— 建店与建它的第一个管理员"+
			"不在同一个事务里", n)
	}
}

// code 不满足契约的 pattern 时回 422，而且**在碰数据库之前**。
//
// 它值得一条独立的测试，因为这一列会变成域名的一段与路径的一段：放一个带点
// 的 code 进库，租户解析那一层再也解不出它来，而应用侧在 merchants 上既没有
// UPDATE 也没有 DELETE —— 改不了也删不掉。
func TestOpeningAShopRejectsACodeThatCannotBeRouted(t *testing.T) {
	token := newPlatformAdmin(t)
	for _, bad := range []string{"has.dot", "Upper", "a", "has/slash", ""} {
		body := fmt.Sprintf(`{"code":%q,"name":"路由不出来","admin_email":"x@keel.test"}`, bad)
		w := postWithKey(t, hostA, "/api/v1/admin/merchants", body, token, "bad-"+bad)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("code=%q 回了 %d，期望 422（契约 pattern ^[a-z0-9][a-z0-9-]{1,30}$）：%s",
				bad, w.Code, w.Body.String())
		}
	}
}
