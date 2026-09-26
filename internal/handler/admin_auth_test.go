package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/auth"
	"github.com/keel/keel/internal/db"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/service"
)

// ---------------------------------------------------------------------------
// 夹具
// ---------------------------------------------------------------------------
//
// staff 那两张表在种子里是空的，这是刻意的：引导流程的前提就是「没有平台级
// 管理员」，往种子里塞一个平台管理员会让那条路径在本包里永远测不到。
// 所以每条测试自己造自己要的那几行，用带测试名的邮箱互不相扰。

// adminExec 用管理员连接（绕过 RLS）跑一条语句。
//
// 夹具必须绕过 RLS：要往两家店 + 平台三个作用域里各插一行 staff，
// 而 keel_app 的任何一条连接一次只看得见其中一个作用域。
func adminExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func adminQueryInt64(t *testing.T, sql string, args ...any) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var out int64
	if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// mkStaff 造一行 staff。shopCode 为空表示**平台级**（merchant_id 为 NULL）。
func mkStaff(t *testing.T, shopCode, email string, role, status int16) int64 {
	t.Helper()
	if shopCode == "" {
		return adminQueryInt64(t,
			`INSERT INTO staff (merchant_id, email, role, status)
			 VALUES (NULL, $1, $2, $3) RETURNING id`, email, role, status)
	}
	return adminQueryInt64(t,
		`INSERT INTO staff (merchant_id, email, role, status)
		 VALUES ((SELECT id FROM merchants WHERE code = $1), $2, $3, $4) RETURNING id`,
		shopCode, email, role, status)
}

// mkEmailLink 给某个 staff 造一串 kind=2 的一次性登录链接 token，返回明文。
//
// 直接插库而不是走接口，是因为这些测试要的是「某个已经存在的人手里有一串
// 有效链接」，而接口那条路每次都会**多建一个人**。
//
// （「商家级的第一个管理员从哪来」这件事本轮有接口了：POST /admin/merchants
// 会在新店的租户作用域里建他并签一串同样 kind=2 的 token —— 它的端到端路径
// 由 admin_merchant_test.go 守着。这里仍然直接插库，理由见上一段。）
// 夹具造的是**库里那一行**，而换会话这一步走的是真实的
// POST /admin/auth/session —— 被测的那一段没有被替换掉。
func mkEmailLink(t *testing.T, staffID int64, ttl time.Duration) string {
	t.Helper()
	token, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	adminExec(t,
		`INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
		 VALUES ($1, $2, 2, now() + $3::interval)`,
		staffID, auth.HashStaffToken(token), fmt.Sprintf("%d seconds", int(ttl.Seconds())))
	return token
}

// staffSession 走真实的 POST /admin/auth/session 换一串会话 token。
//
// 与买家那边的 login 同一个原则：测试里的令牌一律从真实路径来，不自己用
// testSigner 签一串。自己签的话，「换会话真的会签发令牌吗」就没有任何测试
// 覆盖到，而每一条鉴权断言都在验一个测试自己造出来的东西。
func staffSession(t *testing.T, host string, staffID int64) api.StaffSession {
	t.Helper()
	link := mkEmailLink(t, staffID, time.Minute)
	w := post(t, host, "/api/v1/admin/auth/session", `{"token":"`+link+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("换会话失败（staff=%d host=%s）：%d %s", staffID, host, w.Code, w.Body.String())
	}
	var out api.StaffSession
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("StaffSession 解析失败: %v\n%s", err, w.Body.String())
	}
	if out.Token == "" {
		t.Fatal("换到会话却没有 token")
	}
	return out
}

// getAs 发一个带后台会话的 GET。
func getAs(t *testing.T, host, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = host
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// patchAs 发一个带后台会话的 PATCH。
func patchAs(t *testing.T, host, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, req)
	return w
}

// wipeStaff 清空 staff（连带 staff_tokens 的 ON DELETE CASCADE）。
// 只给引导那几条测试用 —— 它们的前提是「库里没有平台级管理员」。
func wipeStaff(t *testing.T) {
	t.Helper()
	adminExec(t, `DELETE FROM staff`)
}

// wipeShopStaff 只清掉一家店的 staff。
//
// 「最后一个在岗管理员」那条测试非用它不可：同一个包里的测试共用一个库，
// 前面几条各自往 shop-a 里放过管理员，于是「这是最后一个」这个前提不成立，
// 而测试会以 200 的形式失败 —— 真因是夹具，不是被测代码。
// 这正是这个仓库反复踩到的那类空转的镜像：前提没建起来时，
// 断言测的是另一件事。
func wipeShopStaff(t *testing.T, code string) {
	t.Helper()
	adminExec(t,
		`DELETE FROM staff WHERE merchant_id = (SELECT id FROM merchants WHERE code = $1)`,
		code)
}

// staffService 是测试直接调 service 的那个入口，只用在引导上：
// 「进程启动时创建引导账号」按设计就没有 HTTP 入口（那正是它的准入条件），
// 所以这一步只能从这一侧驱动。它接的是同一个池、同一个签名器。
func staffService() *service.StaffService {
	return service.NewStaffService(repository.New(testPool), testSigner, nil)
}

// staffOf 把响应体读成 api.Staff。
func staffOf(t *testing.T, w *httptest.ResponseRecorder, wantStatus int) api.Staff {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("状态码是 %d，期望 %d。响应体：%s", w.Code, wantStatus, w.Body.String())
	}
	var out api.Staff
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("Staff 解析失败: %v\n%s", err, w.Body.String())
	}
	return out
}

// ---------------------------------------------------------------------------
// 引导
// ---------------------------------------------------------------------------

// 阳性对照 + 主路径：库里没有平台管理员时，启动那一步会建一个并签出引导
// token；拿它能换到一个能用的会话，而且那个身份是**平台级**的。
//
// 它排在所有「必须被拒」的断言前面：没有它，把 Bootstrap 写成恒 401
// 也能让下面一大半测试全绿。
func TestBootstrapExchangesTheStdoutTokenForAPlatformSession(t *testing.T) {
	wipeStaff(t)
	ctx := context.Background()

	token, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("库里一个平台管理员都没有，引导却什么都没做 —— " +
			"这个部署从此谁也进不去后台")
	}
	// 明文不该留在库里，只该有 sha256。这一条单独验：它是 §14 里
	// 「后台 token 和支付密钥是同一个量级」那句话在库这一层的落点。
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM staff_tokens WHERE token_hash = $1`, token); n != 0 {
		t.Error("库里存着引导 token 的明文 —— 只该存 sha256")
	}
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM staff_tokens WHERE token_hash = $1 AND kind = 1`,
		auth.HashStaffToken(token)); n != 1 {
		t.Fatalf("按 sha256 在库里找到 %d 行 kind=1，期望 1 —— 夹具或签发路径不对", n)
	}

	// 换会话。打在 shop-a 的域名上：平台级身份与 Host 无关，这一点下面
	// TestPlatformSessionWorksOnAnyHost 还会再验一次。
	w := post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"`+token+`","email":"platform-admin@example.com"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("引导换会话失败：%d %s", w.Code, w.Body.String())
	}
	var sess api.StaffSession
	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.Staff.MerchantId != nil {
		t.Errorf("引导账号的 merchant_id 是 %d，期望 null（平台级）", *sess.Staff.MerchantId)
	}
	if string(sess.Staff.Email) != "platform-admin@example.com" {
		t.Errorf("邮箱是 %q，期望换成请求里那一个 —— 引导的第二件事就是补上找回通道",
			sess.Staff.Email)
	}
	if sess.Staff.Role != 1 {
		t.Errorf("引导账号的 role 是 %d，期望 1（管理员）", sess.Staff.Role)
	}

	// 会话真的能用。
	me := staffOf(t, getAs(t, hostA, "/api/v1/admin/me", sess.Token), http.StatusOK)
	if me.Id != sess.Staff.Id {
		t.Errorf("/admin/me 回的是 %d，期望 %d", me.Id, sess.Staff.Id)
	}
	if me.MerchantId != nil {
		t.Errorf("/admin/me 的 merchant_id 是 %d，期望 null", *me.MerchantId)
	}
}

// 引导 token 用掉即失效，而且引导通道随之关闭。
//
// 两件事分开断言，因为它们会一起坏也会各自坏：一个不写 used_at 的实现
// 让第一条红，一个「token 错了就一律 401」的实现让第二条红。
func TestBootstrapTokenIsSingleUseAndThenTheChannelCloses(t *testing.T) {
	wipeStaff(t)
	ctx := context.Background()

	token, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil || token == "" {
		t.Fatalf("引导没签出 token：%v", err)
	}
	body := `{"token":"` + token + `","email":"once@example.com"}`

	// 阳性对照：第一次必须成功。没有它，下面那个 409 在「这条接口根本不工作」
	// 的时候同样是绿的。
	if w := post(t, hostA, "/api/v1/admin/auth/bootstrap", body, ""); w.Code != http.StatusOK {
		t.Fatalf("第一次引导就失败了：%d %s", w.Code, w.Body.String())
	}

	// 第二次：token 已经被用掉，而且现在库里有一个在岗平台管理员、
	// 一串活着的引导 token 都没有 —— 契约里那个 409「引导通道已关闭」。
	p := problemOf(t, post(t, hostA, "/api/v1/admin/auth/bootstrap", body, ""),
		http.StatusConflict)
	if p.Type != "https://keel.dev/problems/bootstrap-closed" {
		t.Errorf("type 是 %q，期望 bootstrap-closed", p.Type)
	}

	// 启动再跑一次也不该再签一串新的 —— 否则每次重启都往日志里丢一把
	// 后台全权的钥匙。
	again, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != "" {
		t.Error("已经有平台管理员了，引导却又签了一串 token")
	}
}

// 引导 token 过期前没人用 → 下一次启动**给同一个占位账号补签一串**。
//
// 这是第一版漏掉的那条死路：判据是「在岗平台管理员 > 0」，占位账号自己就算
// 一个，于是 token 一过期就再也不签了。邮件登录在没接 SMTP 时回 501，
// 这个部署的后台就永久进不去 —— 而系统认为引导已经做完了。演示栈就是这样
// 被锁住的：引导 token 印在了一个早就删掉的容器日志里。
func TestExpiredUnredeemedBootstrapIsReissuedOnTheNextStart(t *testing.T) {
	wipeStaff(t)
	ctx := context.Background()

	first, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil || first == "" {
		t.Fatalf("第一次启动没签出引导 token：%v", err)
	}
	// 让它在没人用的情况下过期。
	adminExec(t, `UPDATE staff_tokens SET expire_at = now() - interval '1 minute' WHERE kind = 1`)

	// 过期没兑换的时候，拿那串旧 token 来换，回的是 401 而不是 409：
	// 通道并没有「关闭」，下一次启动会补签。回 409 会让运维以为已经有管理员了，
	// 去找一个根本不存在的人。
	if w := post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"`+first+`","email":"late@example.com"}`, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("用过期的引导 token 换会话回了 %d，期望 401 —— 通道没关，只是这串过期了：%s",
			w.Code, w.Body.String())
	}

	second, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second == "" {
		t.Fatal("引导 token 过期前没人用，再启动一次却什么都没签 —— 这个部署的后台从此" +
			"谁也进不去（占位账号被当成了「已经有人能登录」）")
	}
	if second == first {
		t.Fatal("补签出来的还是过期的那一串")
	}
	// 补签挂在**原来那个**占位账号上，不是再建一个：平台级邮箱有唯一索引，
	// 再建一个会撞；就算不撞，两个占位账号也说不清谁是谁。
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM staff WHERE merchant_id IS NULL AND role = 1`); n != 1 {
		t.Errorf("平台级管理员有 %d 个，期望 1 —— 补签应当复用原来的占位账号", n)
	}

	// 补签的那串真的能用。
	w := post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"`+second+`","email":"finally@example.com"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("用补签的 token 换会话失败：%d %s", w.Code, w.Body.String())
	}

	// 兑换之后就有人能登录了，再启动一次不签 —— 原来那条规矩照旧。
	if again, err := staffService().EnsureBootstrapAdmin(ctx); err != nil || again != "" {
		t.Errorf("已经有人登录过后台了，启动又签了一串（err=%v）", err)
	}
}

// 引导 token **还在有效期内**时重启，不签第二串。
//
// 这条是上一条的反面，两条合起来才钉住补签的边界：一个「只要没人兑换过就签」
// 的实现能让上一条全绿，而它每重启一次就往日志里多丢一把后台全权的钥匙。
func TestRestartInsideTheBootstrapWindowIssuesNoSecondToken(t *testing.T) {
	wipeStaff(t)
	ctx := context.Background()

	first, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil || first == "" {
		t.Fatalf("第一次启动没签出引导 token：%v", err)
	}
	again, err := staffService().EnsureBootstrapAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != "" {
		t.Error("上一串引导 token 还没过期，重启又签了一串 —— 每次重启都多一把钥匙")
	}
	if n := adminQueryInt64(t,
		`SELECT count(*) FROM staff_tokens WHERE kind = 1 AND used_at IS NULL AND expire_at > now()`); n != 1 {
		t.Errorf("活着的引导 token 有 %d 串，期望 1", n)
	}
}

// 引导窗口**还开着**的时候，一串错 token 是 401，不是 409。
//
// 这条和上面那条是同一个判据的两半。合成一条的话，一个「永远回 409」的
// 实现能让上面那条全绿，而它把一次「你 token 输错了」报成「通道已关闭」——
// 运维会去找一个根本不存在的已存在管理员。
func TestWrongBootstrapTokenIsUnauthorizedWhileTheWindowIsOpen(t *testing.T) {
	wipeStaff(t)
	if _, err := staffService().EnsureBootstrapAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := problemOf(t, post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"not-the-real-one","email":"x@example.com"}`, ""),
		http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/unauthorized" {
		t.Errorf("type 是 %q，期望 unauthorized", p.Type)
	}
}

// ---------------------------------------------------------------------------
// 令牌锁死在作用域上 —— 这一组是本轮最要紧的断言
// ---------------------------------------------------------------------------

// A 店的后台会话在 B 店必须被拒，而且**拒的是鉴权语义，不是「查不到这个人」**。
//
// 与买家那条（TestTokenFromAnotherShopIsRejected）同构，理由更硬：后台 token
// 能读全部订单与客户手机号、能改价、能发起退款。
//
// 断言 type 而不只是状态码：把这条校验删掉之后，请求会带着 A 店的 staff_id
// 进入 B 店的作用域，而 staff 的 RLS 会让它查不到 —— 那也是 401，
// 只是 type 变成 unauthorized。两者在状态码上一模一样。
func TestStaffSessionFromAnotherShopIsRejected(t *testing.T) {
	idA := mkStaff(t, "shop-a", "cross-a@example.com", 1, 1)
	sess := staffSession(t, hostA, idA)

	// 阳性对照：这串 token 在自己店里是好用的。没有它，一个恒 401 的
	// 中间件会让下面那条断言全绿。
	if w := getAs(t, hostA, "/api/v1/admin/me", sess.Token); w.Code != http.StatusOK {
		t.Fatalf("A 店的 token 在 A 店就不好用：%d %s", w.Code, w.Body.String())
	}

	p := problemOf(t, getAs(t, hostB, "/api/v1/admin/me", sess.Token), http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/token-tenant-mismatch" {
		t.Fatalf("type 是 %q，期望 token-tenant-mismatch —— "+
			"靠「在 B 店查不到这个人」来拒绝的话，真因（跨店用令牌）不会出现在"+
			"任何一条日志里，而两家店的 staff.id 来自同一个序列，撞上就是"+
			"读到另一家店的管理员身份", p.Type)
	}
}

// 平台级会话在**任何** Host 上都被接受 —— 那是那个身份的语义，不是网开一面。
//
// 它与上面那条是一对：只有上面那条的话，一个「所有后台令牌都比对租户」的
// 实现会全绿，而平台操作员从此哪个域名都进不去。
func TestPlatformSessionWorksOnAnyHost(t *testing.T) {
	wipeStaff(t)
	token, err := staffService().EnsureBootstrapAdmin(context.Background())
	if err != nil || token == "" {
		t.Fatalf("引导没签出 token：%v", err)
	}
	w := post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"`+token+`","email":"anyhost@example.com"}`, "")
	var sess api.StaffSession
	if w.Code != http.StatusOK {
		t.Fatalf("引导换会话失败：%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{hostA, hostB} {
		me := staffOf(t, getAs(t, host, "/api/v1/admin/me", sess.Token), http.StatusOK)
		if me.MerchantId != nil {
			t.Errorf("在 %s 上 /admin/me 的 merchant_id 是 %d，期望 null", host, *me.MerchantId)
		}
	}
}

// 平台级操作员对商家不可见，商家的人对平台级也不可见。
//
// 这条验的是 00017 那条 `merchant_id IS NOT DISTINCT FROM staff_scope_merchant()`
// 在**接口层**的效果。把它换成 `= current_merchant() OR merchant_id IS NULL`
// 之后，下面第一条断言会红 —— 商家管理员会在自己的员工列表里看到平台操作员的邮箱。
func TestPlatformAndMerchantStaffAreInvisibleToEachOther(t *testing.T) {
	wipeStaff(t)
	// 平台级一个（走引导，因为平台会话只能这么拿到）。
	token, err := staffService().EnsureBootstrapAdmin(context.Background())
	if err != nil || token == "" {
		t.Fatalf("引导没签出 token：%v", err)
	}
	w := post(t, hostA, "/api/v1/admin/auth/bootstrap",
		`{"token":"`+token+`","email":"invis-platform@example.com"}`, "")
	var platformSess api.StaffSession
	if w.Code != http.StatusOK {
		t.Fatalf("引导换会话失败：%d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &platformSess); err != nil {
		t.Fatal(err)
	}

	// 商家级两个：A 店与 B 店各一个管理员。
	idA := mkStaff(t, "shop-a", "invis-a@example.com", 1, 1)
	mkStaff(t, "shop-b", "invis-b@example.com", 1, 1)
	merchantSess := staffSession(t, hostA, idA)

	emails := func(t *testing.T, host, bearer string) []string {
		t.Helper()
		w := getAs(t, host, "/api/v1/admin/staff", bearer)
		if w.Code != http.StatusOK {
			t.Fatalf("列员工失败：%d %s", w.Code, w.Body.String())
		}
		var page struct {
			Items []api.Staff `json:"items"`
			Total int         `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range page.Items {
			out = append(out, string(s.Email))
		}
		// 阳性对照：一行都没有的话，下面的「不包含」断言什么也证明不了。
		if len(out) == 0 {
			t.Fatalf("%s 的员工列表是空的 —— 这条断言在空转", host)
		}
		return out
	}

	merchantSees := emails(t, hostA, merchantSess.Token)
	if contains(merchantSees, "invis-platform@example.com") {
		t.Errorf("A 店的管理员看到了平台操作员：%v —— "+
			"策略被放宽成了 `OR merchant_id IS NULL`", merchantSees)
	}
	if contains(merchantSees, "invis-b@example.com") {
		t.Errorf("A 店的管理员看到了 B 店的人：%v —— 租户比较整个没生效", merchantSees)
	}
	if !contains(merchantSees, "invis-a@example.com") {
		t.Errorf("A 店的管理员看不到自己店的人：%v", merchantSees)
	}

	platformSees := emails(t, hostA, platformSess.Token)
	if contains(platformSees, "invis-a@example.com") || contains(platformSees, "invis-b@example.com") {
		t.Errorf("平台作用域看到了商家的员工：%v —— "+
			"平台作用域退化成了 BYPASSRLS，那比它该有的权限宽得多", platformSees)
	}
	if !contains(platformSees, "invis-platform@example.com") {
		t.Errorf("平台作用域看不到自己：%v —— 这正是 §14 那条可空 merchant_id "+
			"与标准策略相撞时的症状", platformSees)
	}
}

// 新员工的租户从会话继承，**不接受请求体传入**（契约与 §14 认证流程 ④）。
//
// 请求体里根本没有 merchant_id 这个字段（api.StaffCreateRequest 没有它），
// 所以这条测试验的是另一半：建出来的那一行落在**调用者**的租户上，
// 而不是落成 NULL（平台级）或别人家。
func TestNewStaffInheritsTheCallersTenant(t *testing.T) {
	idA := mkStaff(t, "shop-a", "creator-a@example.com", 1, 1)
	sess := staffSession(t, hostA, idA)

	w := post(t, hostA, "/api/v1/admin/staff",
		`{"email":"hired-a@example.com","name":"新人","role":2}`, sess.Token)
	got := staffOf(t, w, http.StatusCreated)

	wantMerchant := merchantIDOf(t, "shop-a")
	if got.MerchantId == nil {
		t.Fatal("商家管理员建出来的员工 merchant_id 是 null —— " +
			"那是一个平台级操作员，也就是一次提权")
	}
	if *got.MerchantId != wantMerchant {
		t.Errorf("新员工落在 merchant_id=%d，期望 %d（调用者那家店）",
			*got.MerchantId, wantMerchant)
	}
	// 库里那一行也要对：响应体是我们自己拼的，它对不上库的时候看不出来。
	gotInDB := adminQueryInt64(t,
		`SELECT coalesce(merchant_id, -1) FROM staff WHERE email = $1`, "hired-a@example.com")
	if gotInDB != wantMerchant {
		t.Errorf("库里那一行的 merchant_id 是 %d，期望 %d", gotInDB, wantMerchant)
	}
}

// ---------------------------------------------------------------------------
// 会话的生命周期
// ---------------------------------------------------------------------------

// 过期的后台会话必须被拒，而且拒的是 token-expired。
//
// 令牌用 testSigner 的一个**过去的时钟**签出来（没法等 7 天），这是本包里
// 唯一一处自己签令牌的地方，与买家那边过期测试的处理一致。
//
// 关键在于库里那一行是**真实存在且没被撤销**的：不这么做的话，这条断言
// 在「校验根本没查到这一行」时同样是绿的，而那是另一个原因。
func TestExpiredStaffSessionIsRejected(t *testing.T) {
	idA := mkStaff(t, "shop-a", "expired-a@example.com", 1, 1)
	merchant := merchantIDOf(t, "shop-a")

	past := time.Now().Add(-8 * 24 * time.Hour)
	expired, err := testSigner.WithClock(func() time.Time { return past }).
		IssueStaff(&merchant, idA, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	// 库里那一行仍然活着（到期时间在未来）—— 于是唯一能拒绝这个请求的
	// 就是令牌自己的 exp。
	adminExec(t,
		`INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
		 VALUES ($1, $2, 3, now() + interval '7 days')`,
		idA, auth.HashStaffToken(expired))

	// 阳性对照：同一个人、同一张表，一串没过期的令牌是好用的。
	live, err := testSigner.IssueStaff(&merchant, idA, auth.StaffSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	adminExec(t,
		`INSERT INTO staff_tokens (staff_id, token_hash, kind, expire_at)
		 VALUES ($1, $2, 3, now() + interval '7 days')`,
		idA, auth.HashStaffToken(live))
	if w := getAs(t, hostA, "/api/v1/admin/me", live); w.Code != http.StatusOK {
		t.Fatalf("阳性对照失败：没过期的令牌也不好用（%d %s）", w.Code, w.Body.String())
	}

	p := problemOf(t, getAs(t, hostA, "/api/v1/admin/me", expired), http.StatusUnauthorized)
	if p.Type != "https://keel.dev/problems/token-expired" {
		t.Errorf("type 是 %q，期望 token-expired —— "+
			"一片过期是会话到期，一片无效是有人在试，运维看到的是两件事", p.Type)
	}
}

// 撤销一条会话之后它当场失效。
//
// 这是 staff_tokens 里 revoked_at 那一列存在的全部理由，也是「后台会话为什么
// 敢做成 7 天」的那一半（§14：可撤销）。把 TouchLiveStaffSession 里的
// `revoked_at IS NULL` 删掉，这里会红。
func TestRevokedStaffSessionStopsWorkingImmediately(t *testing.T) {
	idA := mkStaff(t, "shop-a", "revoked-a@example.com", 1, 1)
	sess := staffSession(t, hostA, idA)

	// 阳性对照：撤销之前它是好用的。
	if w := getAs(t, hostA, "/api/v1/admin/me", sess.Token); w.Code != http.StatusOK {
		t.Fatalf("撤销之前就不好用了：%d %s", w.Code, w.Body.String())
	}

	adminExec(t, `UPDATE staff_tokens SET revoked_at = now() WHERE token_hash = $1`,
		auth.HashStaffToken(sess.Token))

	if w := getAs(t, hostA, "/api/v1/admin/me", sess.Token); w.Code != http.StatusUnauthorized {
		t.Fatalf("撤销之后还能用：%d %s —— 那么 7 天的会话就是 7 天不可吊销的全权",
			w.Code, w.Body.String())
	}
}

// 一次性登录链接用掉即失效。
func TestEmailLinkTokenIsSingleUse(t *testing.T) {
	idA := mkStaff(t, "shop-a", "once-a@example.com", 1, 1)
	link := mkEmailLink(t, idA, time.Minute)
	body := `{"token":"` + link + `"}`

	if w := post(t, hostA, "/api/v1/admin/auth/session", body, ""); w.Code != http.StatusOK {
		t.Fatalf("第一次换会话就失败了：%d %s", w.Code, w.Body.String())
	}
	problemOf(t, post(t, hostA, "/api/v1/admin/auth/session", body, ""),
		http.StatusUnauthorized)
}

// A 店的一次性链接在 B 店换不出会话。
//
// 这条与会话令牌那条是两种不同的防线：一次性 token 是**不透明**的，
// 里面没有可比对的租户声明，所以守它的是 RLS —— 那一行的 staff 不属于
// B 店的作用域，也不属于平台作用域，两次查找都落空。
func TestEmailLinkFromAnotherShopCannotBeExchanged(t *testing.T) {
	idA := mkStaff(t, "shop-a", "link-a@example.com", 1, 1)
	link := mkEmailLink(t, idA, time.Minute)

	problemOf(t, post(t, hostB, "/api/v1/admin/auth/session",
		`{"token":"`+link+`"}`, ""), http.StatusUnauthorized)

	// 阳性对照：同一串 token 在 A 店换得出来。少了这一句，上面那条断言在
	// 「这条接口对谁都回 401」时同样是绿的。
	if w := post(t, hostA, "/api/v1/admin/auth/session",
		`{"token":"`+link+`"}`, ""); w.Code != http.StatusOK {
		t.Fatalf("阳性对照失败：这串 token 在 A 店也换不出会话（%d %s）",
			w.Code, w.Body.String())
	}
}

// 停用的操作员拿着一串还没过期的会话也进不来，而且回的是 403 不是 401。
//
// 403 与 401 的差别不是措辞：401 会让被停用的人一遍遍重新登录，
// 而他登得进来（邮箱链接照样发得出去），只是每次都在同一个地方被挡。
func TestDisabledStaffIsForbiddenNotUnauthorized(t *testing.T) {
	idA := mkStaff(t, "shop-a", "disabled-a@example.com", 1, 1)
	sess := staffSession(t, hostA, idA)

	if w := getAs(t, hostA, "/api/v1/admin/me", sess.Token); w.Code != http.StatusOK {
		t.Fatalf("停用之前就不好用了：%d %s", w.Code, w.Body.String())
	}
	adminExec(t, `UPDATE staff SET status = 2 WHERE id = $1`, idA)

	p := problemOf(t, getAs(t, hostA, "/api/v1/admin/me", sess.Token), http.StatusForbidden)
	if p.Type != "https://keel.dev/problems/account-disabled" {
		t.Errorf("type 是 %q，期望 account-disabled", p.Type)
	}
}

// 买家的 access_token 打不动 /admin/，反过来后台会话也打不动买家接口。
//
// 两个方向都要：只有一个方向的话，一个「typ 根本没进签名」的实现能让
// 其中一条全绿。两张表的 id 来自同一种自增序列，所以这不是理论问题 ——
// 一串买家令牌被当成后台令牌用时，它的 uid 会被拿去查 staff。
func TestBuyerAndStaffTokensCannotImpersonateEachOther(t *testing.T) {
	buyer := login(t, hostA, seedPhone, seedPassword)
	idA := mkStaff(t, "shop-a", "swap-a@example.com", 1, 1)
	staffSess := staffSession(t, hostA, idA)

	if w := getAs(t, hostA, "/api/v1/admin/me", buyer.AccessToken); w.Code != http.StatusUnauthorized {
		t.Errorf("买家令牌打 /admin/me 得到 %d，期望 401", w.Code)
	}
	if w := post(t, hostA, "/api/v1/auth/logout", "", staffSess.Token); w.Code != http.StatusUnauthorized {
		t.Errorf("后台会话打 /auth/logout 得到 %d，期望 401", w.Code)
	}
}

// 没有令牌时 /admin/ 一律 401。
func TestAdminEndpointsRequireAStaffToken(t *testing.T) {
	for _, path := range []string{"/api/v1/admin/me", "/api/v1/admin/staff"} {
		if w := getAs(t, hostA, path, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s 不带令牌得到 %d，期望 401", path, w.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// 员工管理
// ---------------------------------------------------------------------------

// 操作员（role=2）加不了人。
func TestOperatorCannotAddStaff(t *testing.T) {
	// 同店先有一个管理员：不然下面那条阳性对照造不出来。
	adminID := mkStaff(t, "shop-a", "op-admin-a@example.com", 1, 1)
	opID := mkStaff(t, "shop-a", "op-a@example.com", 2, 1)

	opSess := staffSession(t, hostA, opID)
	p := problemOf(t, post(t, hostA, "/api/v1/admin/staff",
		`{"email":"nope-a@example.com","role":2}`, opSess.Token), http.StatusForbidden)
	if p.Type != "https://keel.dev/problems/staff-forbidden" {
		t.Errorf("type 是 %q，期望 staff-forbidden", p.Type)
	}

	// 阳性对照：同一个请求，管理员发就成。没有它，一个「POST /admin/staff
	// 恒 403」的实现会让上面那条全绿。
	adminSess := staffSession(t, hostA, adminID)
	if w := post(t, hostA, "/api/v1/admin/staff",
		`{"email":"yes-a@example.com","role":2}`, adminSess.Token); w.Code != http.StatusCreated {
		t.Fatalf("管理员加人也失败了：%d %s", w.Code, w.Body.String())
	}
}

// 不能把最后一个在岗管理员降级或停用（数据模型 §14 那条进不了数据库的约束）。
func TestLastAdminCannotBeDemotedOrDisabled(t *testing.T) {
	wipeShopStaff(t, "shop-a")
	onlyAdmin := mkStaff(t, "shop-a", "last-admin-a@example.com", 1, 1)

	// 阳性对照：这家店现在真的只有这一个在岗管理员。少了这一句，
	// 前面某条测试留下的另一个管理员会让下面那个 409 变成 200，
	// 而失败信息指向的是「业务规则没实现」这个错方向。
	if n := adminQueryInt64(t, `
		SELECT count(*) FROM staff s JOIN merchants m ON m.id = s.merchant_id
		 WHERE m.code = 'shop-a' AND s.role = 1 AND s.status = 1 AND s.deleted_at IS NULL`); n != 1 {
		t.Fatalf("shop-a 现在有 %d 个在岗管理员，期望 1 —— 这条测试的前提没建起来", n)
	}
	sess := staffSession(t, hostA, onlyAdmin)
	path := fmt.Sprintf("/api/v1/admin/staff/%d", onlyAdmin)

	// 停用自己：唯一还能踩到这条 409 的路径。
	//
	// 分级权限（v0.1.0）之后「任何人都不能改自己的角色」（403 role-forbidden，
	// 见 permission_test.go 的 TestNobodyCanChangeTheirOwnRole），于是降级
	// 最后一个管理员这件事只剩「别人来降」—— 而来降的那个人自己就是在岗管理员，
	// 被降的那个按定义不是最后一个。所以这条规则今天只挡「最后一个管理员停用自己」。
	p := problemOf(t, patchAs(t, hostA, path, `{"status":2}`, sess.Token), http.StatusConflict)
	if p.Type != "https://keel.dev/problems/last-admin" {
		t.Errorf("停用自己时 type 是 %q，期望 last-admin", p.Type)
	}

	// 阳性对照：再加一个管理员之后，同一个请求就该通过。
	// 没有它，一个「PATCH 恒 409」的实现会让上面那条全绿。
	second := mkStaff(t, "shop-a", "second-admin-a@example.com", 1, 1)
	secondSess := staffSession(t, hostA, second)
	w := patchAs(t, hostA, path, `{"role":2}`, secondSess.Token)
	got := staffOf(t, w, http.StatusOK)
	if got.Role != 2 {
		t.Errorf("降级之后 role 是 %d，期望 2", got.Role)
	}
}

// 别家店的 staff_id 一律 404，不是 403（契约 /admin/ 那一段的约定 3）。
//
// 403 会让自增 id 空间变成一个跨租户的存在性探针 —— 「这个 id 存在但不是
// 你的」本身就是一条不该泄露的信息。
func TestStaffFromAnotherTenantIsNotFound(t *testing.T) {
	adminA := mkStaff(t, "shop-a", "probe-a@example.com", 1, 1)
	// 同店再放一个人，好让阳性对照有东西可改。
	targetA := mkStaff(t, "shop-a", "probe-target-a@example.com", 2, 1)
	targetB := mkStaff(t, "shop-b", "probe-b@example.com", 2, 1)
	sess := staffSession(t, hostA, adminA)

	p := problemOf(t, patchAs(t, hostA,
		fmt.Sprintf("/api/v1/admin/staff/%d", targetB), `{"role":1}`, sess.Token),
		http.StatusNotFound)
	if p.Type != "https://keel.dev/problems/not-found" {
		t.Errorf("type 是 %q，期望 not-found", p.Type)
	}

	// 阳性对照：同一个请求打在自己店的人身上要成功。少了它，一个
	// 「PATCH 恒 404」的实现会让上面那条全绿。
	if w := patchAs(t, hostA,
		fmt.Sprintf("/api/v1/admin/staff/%d", targetA), `{"role":1}`, sess.Token); w.Code != http.StatusOK {
		t.Fatalf("改自己店的人也 404 了：%d %s", w.Code, w.Body.String())
	}
}

// 同一个租户里邮箱不能重复，回 409。
func TestDuplicateStaffEmailInTheSameTenantConflicts(t *testing.T) {
	adminA := mkStaff(t, "shop-a", "dup-admin-a@example.com", 1, 1)
	sess := staffSession(t, hostA, adminA)

	if w := post(t, hostA, "/api/v1/admin/staff",
		`{"email":"dup-a@example.com","role":2}`, sess.Token); w.Code != http.StatusCreated {
		t.Fatalf("第一次加人就失败了：%d %s", w.Code, w.Body.String())
	}
	p := problemOf(t, post(t, hostA, "/api/v1/admin/staff",
		`{"email":"dup-a@example.com","role":2}`, sess.Token), http.StatusConflict)
	if p.Type != "https://keel.dev/problems/staff-email-taken" {
		t.Errorf("type 是 %q，期望 staff-email-taken", p.Type)
	}

	// 同一个邮箱在另一家店是**另一个人**（uk_staff_email 的首列是 merchant_id）。
	// 这一条是上面那条的边界：只有上面那条的话，一条全局唯一的索引也能让它绿，
	// 而那会让后开的那家店建不了同名邮箱的员工。
	adminB := mkStaff(t, "shop-b", "dup-admin-b@example.com", 1, 1)
	sessB := staffSession(t, hostB, adminB)
	if w := post(t, hostB, "/api/v1/admin/staff",
		`{"email":"dup-a@example.com","role":2}`, sessB.Token); w.Code != http.StatusCreated {
		t.Fatalf("同一个邮箱在 B 店也被拒了：%d %s —— "+
			"唯一约束没有收进租户内", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 清账：邮箱登录链接
// ---------------------------------------------------------------------------

// 邮箱登录链接这条路今天发不出去，所以它回 501 而不是契约里那个 202。
//
// 形状与买家那边的 TestSMSLoginSaysItIsNotImplemented 完全一样，
// 两个方向都锁：实现了它、那条 501 消失，这里红；忘了从 contract_test.go
// 的 NotYetImplementedBody 里划掉，这里也红。
func TestAdminEmailLinkSaysItIsNotImplemented(t *testing.T) {
	w := post(t, hostA, "/api/v1/admin/auth/email-link",
		`{"email":"someone@example.com"}`, "")
	p := problemOf(t, w, http.StatusNotImplemented)
	if p.Type != "https://keel.dev/problems/not-implemented" {
		t.Fatalf("type 是 %q，期望 not-implemented —— 202「已受理」是一句在"+
			"邮件服务上线之前都不会被纠正的假话", p.Type)
	}
	if _, listed := routeOf(t, http.MethodPost, "/admin/auth/email-link").
		NotYetImplementedBody["email"]; !listed {
		t.Error("邮箱登录链接还没实现，但 contract_test.go 的 NotYetImplementedBody 里" +
			"没有 email 这一笔账 —— 清单与实现分叉了")
	}
}

// ---------------------------------------------------------------------------
// 挂着的账要有靶子
// ---------------------------------------------------------------------------

// POST /admin/staff 与 POST /admin/merchants **还没有实现幂等**，
// 而契约里 Idempotency-Key 在这两条上都是必填请求头。
//
// 这条测试是 contract_test.go 那两笔 NotYetImplementedHeader 的反向执行者：
// 挂账只能做「清单 → 契约」一个方向的机械对账（AST 跟不过一个包级常量），
// 所以「实现了却忘了划掉」由这里盯着 —— 真的做了幂等，它会红。
//
// 它同时是那两笔账的**暴露面说明书**，而这份暴露面比另外 5 条轻：
// 两条接口各自被一条唯一约束兜住，重发建不出第二个人、也建不出第二家店，
// 第二次拿到的是 409 而不是一次重放的 201。
//
// 它们缺的是同一样东西：idempotency_keys.merchant_id 的默认值是
// current_merchant()，而这两条都可能跑在平台作用域里 —— 那里
// current_merchant() 是 RAISE，不是 NULL。
func TestPlatformScopedWritesAreNotYetIdempotent(t *testing.T) {
	token := newPlatformAdmin(t)
	key := "cccccccc-dddd-eeee-ffff-" + fmt.Sprintf("%012d", time.Now().UnixNano()%1_000_000_000_000)

	// —— 加平台操作员。
	email := fmt.Sprintf("twice-%d@keel.test", time.Now().UnixNano())
	t.Cleanup(func() { adminExec(t, `DELETE FROM staff WHERE email = $1`, email) })
	body := fmt.Sprintf(`{"email":%q,"role":2}`, email)

	if w := postWithKey(t, hostA, "/api/v1/admin/staff", body, token, key); w.Code != http.StatusCreated {
		t.Fatalf("第一次加员工失败：%d %s", w.Code, w.Body.String())
	}
	w := postWithKey(t, hostA, "/api/v1/admin/staff", body, token, key)
	if w.Code == http.StatusCreated && w.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("同一把 Idempotency-Key 打两次 POST /admin/staff 拿到了重放 —— " +
			"幂等实现了。请把 contract_test.go 里那笔 NotYetImplementedHeader 挂账删掉，" +
			"并把这条测试改成断言真正的幂等")
	}
	if w.Code != http.StatusConflict {
		t.Fatalf("第二次加同一个邮箱回了 %d，期望 409（邮箱唯一约束挡住了）：%s",
			w.Code, w.Body.String())
	}

	// —— 开店。同一把钥匙，同一个 code。
	code := fmt.Sprintf("twice%d", time.Now().UnixNano()%1_000_000_000)
	dropShop(t, code)
	shop := fmt.Sprintf(`{"code":%q,"name":"重发不会变两家","admin_email":%q}`,
		code, code+"@keel.test")
	if w := postWithKey(t, hostA, "/api/v1/admin/merchants", shop, token, key); w.Code != http.StatusCreated {
		t.Fatalf("第一次开店失败：%d %s", w.Code, w.Body.String())
	}
	w2 := postWithKey(t, hostA, "/api/v1/admin/merchants", shop, token, key)
	if w2.Code == http.StatusCreated && w2.Header().Get("Idempotency-Replayed") == "true" {
		t.Fatalf("同一把 Idempotency-Key 打两次 POST /admin/merchants 拿到了重放 —— " +
			"幂等实现了，请把那笔挂账删掉")
	}
	if w2.Code != http.StatusConflict {
		t.Fatalf("第二次开同一个 code 回了 %d，期望 409（code 全局唯一挡住了）：%s",
			w2.Code, w2.Body.String())
	}
}
