package handler_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 读文件（GET /uploads/{upload_id}）。M4 收尾。
//
// ===========================================================================
// 这一组真正要钉住的两件事
// ===========================================================================
//
// ① **后台传完图拿到的那个 url 真的能打开。** 在这条路由落地之前它是 404，
//    而「字节在磁盘上、缺一条读它的路由」这件事没有任何测试说得出来 ——
//    POST /admin/uploads 返回 201 的那条测试对它是全盲的。
// ② **一个租户的人读不到另一个租户的文件。** 而这一条有两跳，两跳都要验：
//    第一跳靠 uploads 上那条 RLS 策略，第二跳靠签名里带着 merchant_id。
//    只验第一跳的话，把 merchant_id 从签名里拿掉不会有任何东西变红 ——
//    而 upload id 是全局自增的，隔壁店只要猜编号就能读走。

// getNoAuth 发一个**不带任何令牌**的 GET。
//
// 这条接口是公开的（契约里没有 security，商品图本就公开），所以测试也必须
// 不带令牌打 —— 带上的话，「它到底需不需要登录」这件事就没被验过。
func getNoAuth(t *testing.T, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

// seedUpload 走真实的 POST /admin/uploads 传一张图，返回契约的 Upload 与内容。
func seedUpload(t *testing.T, sh adminShop, content []byte) api.Upload {
	t.Helper()
	var up api.Upload
	decodeInto(t, uploadImage(t, sh, "image/webp", content), http.StatusCreated, "传图", &up)
	if up.Url != "/api/v1/uploads/"+strconv.FormatInt(up.Id, 10) {
		t.Fatalf("Upload.url 是 %q，契约要求形如 /api/v1/uploads/{upload_id}", up.Url)
	}
	return up
}

// 主路径：POST /admin/uploads 回的那个 url 打过去是 302，跟过去拿到的是
// **一模一样的字节**。
func TestUploadURLRedirectsToATimeLimitedAddressServingTheBytes(t *testing.T) {
	sh := newAdminShop(t)
	content := []byte("RIFF....WEBPVP8 读回来要逐字节相同 " + sh.Suffix)
	up := seedUpload(t, sh, content)

	w := getNoAuth(t, sh.Host, up.Url)
	if w.Code != http.StatusFound {
		t.Fatalf("GET %s 回了 %d，契约要求 302。响应体：%s", up.Url, w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Fatal("302 没带 Location")
	}
	if !strings.HasPrefix(loc, "/api/v1/uploads/") {
		t.Fatalf("Location 是 %q，期望一个站内相对地址 —— 绝对 URL 要这个进程知道"+
			"自己对外是什么 origin，而它不知道", loc)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location %q 解不开: %v", loc, err)
	}
	if u.Query().Get("exp") == "" || u.Query().Get("sig") == "" {
		t.Fatalf("Location %q 上没有 exp 或 sig —— 契约要求的是一个**限时**地址，"+
			"一个裸路径意味着它永久有效", loc)
	}

	blob := getNoAuth(t, sh.Host, loc)
	if blob.Code != http.StatusOK {
		t.Fatalf("跟着 Location 打过去回了 %d：%s", blob.Code, blob.Body.String())
	}
	if !bytes.Equal(blob.Body.Bytes(), content) {
		t.Fatalf("读回来 %d 字节，传上去的是 %d 字节 —— 内容对不上",
			blob.Body.Len(), len(content))
	}
	if ct := blob.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/webp") {
		t.Errorf("Content-Type 是 %q，期望 image/webp（库里登记的那个）", ct)
	}
	// nosniff 有单独的断言，因为它挡的是一个具体的洞：content_type 是上传时
	// 客户端自己声明的（服务端刻意不嗅探内容），所以「声明 image/png、
	// 内容是一段 HTML」是做得到的 —— 没有这个头，那就是一个挂在本站域名下的
	// 存储型 XSS。
	if got := blob.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options 是 %q，期望 nosniff —— "+
			"这里吐的是用户上传的字节，而 content_type 是客户端自己声明的", got)
	}
}

// 跨租户：两跳都读不到。
func TestAnotherTenantCannotReadThisShopsUpload(t *testing.T) {
	a := newAdminShop(t)
	b := newAdminShop(t)
	up := seedUpload(t, a, []byte("只属于 A 店的字节 "+a.Suffix))

	// 第一跳：B 店的 Host 上，A 店那个 id 是 404 —— 与「这个 id 不存在」
	// 同一个响应。不是 403：upload id 是全局自增的，分开报就是一个能数出
	// 别家店传了多少文件的探测器。
	got := problemType(t, getNoAuth(t, b.Host, up.Url), http.StatusNotFound, "B 店读 A 店的文件")
	if got != problem.TypeUploadNotFound {
		t.Errorf("Problem type 是 %q，期望 %q", got, problem.TypeUploadNotFound)
	}

	// 第二跳：**拿 A 店签出来的那个限时地址，原样打到 B 店的 Host 上。**
	//
	// 这一条是 merchant_id 进签名的唯一执行者。把它从 blobMessage 里拿掉，
	// 上面那条第一跳的断言一个字都不会变 —— 而这条会当场变成 200。
	first := getNoAuth(t, a.Host, up.Url)
	loc := first.Header().Get("Location")
	if loc == "" {
		t.Fatalf("A 店自己读都没拿到 Location：%d %s", first.Code, first.Body.String())
	}
	if w := getNoAuth(t, b.Host, loc); w.Code != http.StatusNotFound {
		t.Fatalf("A 店签出来的限时地址在 B 店的 Host 上回了 %d，期望 404 —— "+
			"签名里没有带上 merchant_id，而 upload id 是全局自增的：%s",
			w.Code, w.Body.String())
	}
}

// 限时地址改一个字节就失效，过期了也失效。
func TestUploadBlobAddressRejectsTamperingAndExpiry(t *testing.T) {
	sh := newAdminShop(t)
	up := seedUpload(t, sh, []byte("签名要真的被校验 "+sh.Suffix))
	loc := getNoAuth(t, sh.Host, up.Url).Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	// ① 签名被改过。
	tampered := *u
	bad := q
	sig := bad.Get("sig")
	bad.Set("sig", flipLastHexDigit(sig))
	tampered.RawQuery = bad.Encode()
	if w := getNoAuth(t, sh.Host, tampered.String()); w.Code != http.StatusNotFound {
		t.Errorf("改掉签名最后一位之后回了 %d，期望 404 —— 签名没有被真的校验：%s",
			w.Code, w.Body.String())
	}

	// ② exp 被往后改（签名没跟着改）。这是最像「能绕过去」的那一种尝试：
	// 攻击者手里有一个合法地址，只想让它活得久一点。
	longer := *u
	far := u.Query()
	far.Set("exp", "99999999999")
	longer.RawQuery = far.Encode()
	if w := getNoAuth(t, sh.Host, longer.String()); w.Code != http.StatusNotFound {
		t.Errorf("把 exp 改到很远的未来之后回了 %d，期望 404 —— exp 不在签名里：%s",
			w.Code, w.Body.String())
	}

	// ③ 一个**真的过期**的地址。不能靠等 5 分钟，所以这里换个办法：
	// 把 exp 改成过去的时间之后签名当然也对不上了，于是它验不出「过期」
	// 这一条与「签名错」的差别 —— 两者在这条路上本来就是同一个响应
	// （service.ErrUploadLinkInvalid），分辨它们只会造出一个预言机。
	// 「过期判断真的存在」由 service 层那条单元测试守着
	// （internal/service 里 BlobFor 的时钟是可替换的）。
	//
	// 这里只钉住一件这一层能钉的事：**原样的那个地址是能用的**。
	// 没有这条阳性对照，上面两条断言在「这条路由永远 404」的实现下也全绿。
	if w := getNoAuth(t, sh.Host, loc); w.Code != http.StatusOK {
		t.Fatalf("原样的限时地址回了 %d，期望 200 —— 上面两条拒绝因此没有区分力：%s",
			w.Code, w.Body.String())
	}
}

// 退款凭证（purpose = 3）不公开可读：不带令牌打是 403（契约明写「刻意返回 403 而非 404」）。
//
// 这一条原先是 contract_test.go 里一笔 NotYetImplementedStage 挂账的反向执行者（那时一律 403，
// 「认上传者本人」还没有实现）。买家上传落地之后本人与后台客服两半都实现了，挂账已删；
// 这条留下来钉住匿名那一格。本人能读、别人不能读、后台按退款单判权，在 buyer_upload_test.go。
//
// 这里插的是一行 staff 上传的 purpose = 3（user_id 为空）：没有「本人」可言，谁带令牌都不行。
func TestRefundProofIsNotPubliclyReadable(t *testing.T) {
	sh := newAdminShop(t)
	// 直接插库：purpose = 3 没有任何接口产得出来，而这正是那笔挂账的内容。
	// 走管理员连接（uploads 有 RLS，keel_app 的连接要先有租户上下文）。
	id := adminQueryInt64(t, `
		INSERT INTO uploads (merchant_id, staff_id, purpose, driver, storage_key,
		                     content_type, size_bytes, sha256)
		VALUES ($1, $2, 3, 1, $3, 'image/png', 3, 'deadbeef')
		RETURNING id`,
		sh.MerchantID, sh.StaffID, "evidence/"+sh.Suffix+".png")
	t.Cleanup(func() { adminExec(t, `DELETE FROM uploads WHERE id = $1`, id) })

	got := problemType(t, getNoAuth(t, sh.Host, fmt.Sprintf("/api/v1/uploads/%d", id)),
		http.StatusForbidden, "公开读一张退款凭证")
	if got != problem.TypeUploadForbidden {
		t.Errorf("Problem type 是 %q，期望 %q —— 契约在这条接口上明写"+
			"「刻意返回 403 而非 404」", got, problem.TypeUploadForbidden)
	}
}

// flipLastHexDigit 把一串十六进制的最后一位换成另一个字符。
func flipLastHexDigit(s string) string {
	if s == "" {
		return "0"
	}
	last := s[len(s)-1]
	if last == '0' {
		return s[:len(s)-1] + "1"
	}
	return s[:len(s)-1] + "0"
}
