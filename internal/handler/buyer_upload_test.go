package handler_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/problem"
)

// 买家上传（契约 POST /uploads）与退款凭证的可见性（GET /uploads/{upload_id}、
// GET /admin/uploads/{upload_id}），以及退款申请对 evidence_urls 的归属核对。
//
// 这一组要钉住的是契约那张可读者表的第三行：「3 退款凭证 —— 仅上传者本人与后台客服」。
// 三个方向都要打：本人能读（阳性对照，否则「谁都读不到」也全绿）；匿名与别的买家读不到；
// 后台按引用它的退款单判权。

// uploadShop 是一家新店（couponShop），外加一条排在它前面执行的清理：
// uploads.user_id 指向买家，而 couponShop 的清理会删买家。
func uploadShop(t *testing.T) couponShop {
	t.Helper()
	cs := newCouponShop(t)
	t.Cleanup(func() { adminExec(t, `DELETE FROM uploads WHERE merchant_id = $1`, cs.MerchantID) })
	return cs
}

// buyerUpload 走真实的 POST /uploads 传一个 multipart 文件。purpose 为空串时不带这个字段；
// token 为空时不带 Authorization。
func buyerUpload(t *testing.T, host, token, purpose, contentType string, content []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if purpose != "" {
		if err := mw.WriteField("purpose", purpose); err != nil {
			t.Fatal(err)
		}
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="x.bin"`)
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", &buf)
	r.Host = host
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

// mustBuyerUpload 传一张并断言 201。
func mustBuyerUpload(t *testing.T, cs couponShop, b couponBuyer, purpose int, content []byte) api.Upload {
	t.Helper()
	var up api.Upload
	decodeInto(t, buyerUpload(t, cs.Host, b.Token, strconv.Itoa(purpose), "image/png", content, freshIdemKey()),
		http.StatusCreated, "买家上传", &up)
	return up
}

// getWithToken 发一个 GET，token 为空时不带 Authorization。
func getWithToken(t *testing.T, host, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	if token == "" {
		return getNoAuth(t, host, path)
	}
	return getAs(t, host, path, token)
}

// followBlob 跟着 302 的 Location 打第二跳（不带任何令牌 —— 第二跳只认签名），返回字节。
func followBlob(t *testing.T, host string, first *httptest.ResponseRecorder, what string) []byte {
	t.Helper()
	wantStatus(t, first, http.StatusFound, what)
	loc := first.Header().Get("Location")
	blob := getNoAuth(t, host, loc)
	wantStatus(t, blob, http.StatusOK, what+"（第二跳）")
	return blob.Body.Bytes()
}

// 主路径：买家传一张退款凭证 —— 登记在他名下、字节真的落盘、幂等；
// 只有他自己带着令牌读得到，匿名与别的买家 403，令牌坏了是 401 而不是 403。
func TestBuyerUploadsEvidenceThatOnlyTheOwnerCanRead(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "up-owner")
	other := cs.newBuyer(t, "up-other")
	content := []byte("PNG 退货照片 " + cs.Suffix)

	key := freshIdemKey()
	var up api.Upload
	decodeInto(t, buyerUpload(t, cs.Host, b.Token, "3", "image/png", content, key), http.StatusCreated, "传凭证", &up)
	if up.Url != "/api/v1/uploads/"+strconv.FormatInt(up.Id, 10) || up.ContentType != "image/png" ||
		up.SizeBytes != int64(len(content)) {
		t.Fatalf("响应里的 Upload 是 %+v", up)
	}
	var (
		userID, purpose int64
		staffNull, ref  bool
		storageKey      string
	)
	if err := admin(t).QueryRow(context.Background(), `
		SELECT COALESCE(user_id, 0), staff_id IS NULL, purpose, referenced, storage_key
		  FROM uploads WHERE id = $1`, up.Id).Scan(&userID, &staffNull, &purpose, &ref, &storageKey); err != nil {
		t.Fatal(err)
	}
	if userID != b.UserID || !staffNull || purpose != 3 || ref {
		t.Fatalf("库里这一行是 user_id=%d staff_id 为空=%v purpose=%d referenced=%v，"+
			"期望记在买家 %d 名下、purpose 3、还没被引用", userID, staffNull, purpose, ref, b.UserID)
	}
	onDisk, err := os.ReadFile(filepath.Join(testUploadRoot, filepath.FromSlash(storageKey)))
	if err != nil || !bytes.Equal(onDisk, content) {
		t.Fatalf("磁盘上 %s 的内容对不上（err=%v）", storageKey, err)
	}

	// 幂等：同一把钥匙同一个文件 → 同一个 id、带重放头；换一个文件 → 422。
	w := buyerUpload(t, cs.Host, b.Token, "3", "image/png", content, key)
	var again api.Upload
	decodeInto(t, w, http.StatusCreated, "重传同一个文件", &again)
	if again.Id != up.Id || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("同一把钥匙重传拿到 id %d（首次 %d）、重放头 %q", again.Id, up.Id, w.Header().Get("Idempotency-Replayed"))
	}
	if got := problemType(t, buyerUpload(t, cs.Host, b.Token, "3", "image/png", []byte("另一张图"), key),
		http.StatusUnprocessableEntity, "同一把钥匙换了文件"); got != problem.TypeIdempotencyKeyReused {
		t.Fatalf("同一把钥匙换了文件应 422 idempotency-key-reused，实得 %q", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE user_id = $1`, b.UserID); n != 1 {
		t.Fatalf("重放之后买家名下有 %d 条上传，期望 1", n)
	}

	// 可见性。
	if got := problemType(t, getNoAuth(t, cs.Host, up.Url), http.StatusForbidden, "匿名读凭证"); got != problem.TypeUploadForbidden {
		t.Fatalf("匿名读凭证应 403 upload-forbidden，实得 %q", got)
	}
	if got := problemType(t, getAs(t, cs.Host, up.Url, other.Token), http.StatusForbidden, "别的买家读凭证"); got != problem.TypeUploadForbidden {
		t.Fatalf("别的买家读凭证应 403 upload-forbidden，实得 %q", got)
	}
	wantStatus(t, getAs(t, cs.Host, up.Url, "not-a-token"), http.StatusUnauthorized, "带着坏令牌读凭证")
	if got := followBlob(t, cs.Host, getAs(t, cs.Host, up.Url, b.Token), "本人读凭证"); !bytes.Equal(got, content) {
		t.Fatalf("本人读回来 %d 字节，传上去 %d 字节", len(got), len(content))
	}

	// 第二跳：私有文件只认私有域的签名。拿**公开域**给它签一个地址（签名密钥是对的，
	// 只是域不对）—— 那正是「第一跳没判归属就签了一个地址」会长成的样子。
	exp := time.Now().Add(time.Minute).Unix()
	msg := fmt.Sprintf("%d:%d:%d", cs.MerchantID, up.Id, exp)
	forged := fmt.Sprintf("/api/v1/uploads/%d/blob?exp=%d&sig=%s", up.Id, exp,
		url.QueryEscape(testSigner.SignDetached("upload-blob", msg)))
	if w := getNoAuth(t, cs.Host, forged); w.Code == http.StatusOK {
		t.Fatalf("用公开域签名的地址读到了退款凭证 —— 第二跳没有区分私有文件：%d", w.Code)
	}
}

// 边界：purpose 只收 2 / 3（商品图走后台）；要登录；类型与大小同后台；头像公开可读。
func TestBuyerUploadBoundaries(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "up-bound")
	png := []byte("PNG " + cs.Suffix)

	for _, p := range []string{"1", "4", "", "x"} {
		if got := problemType(t, buyerUpload(t, cs.Host, b.Token, p, "image/png", png, freshIdemKey()),
			http.StatusUnprocessableEntity, "purpose="+p); got != problem.TypeInvalidRequest {
			t.Errorf("purpose=%q 应 422 invalid-request，实得 %q", p, got)
		}
	}
	wantStatus(t, buyerUpload(t, cs.Host, "", "3", "image/png", png, freshIdemKey()), http.StatusUnauthorized, "不登录上传")
	// 后台会话令牌在买家接口上不算数（两个不同的中间件、两个不同的 context key）。
	wantStatus(t, buyerUpload(t, cs.Host, cs.Token, "3", "image/png", png, freshIdemKey()), http.StatusUnauthorized, "拿后台令牌上传")
	if got := problemType(t, buyerUpload(t, cs.Host, b.Token, "3", "image/gif", png, freshIdemKey()),
		http.StatusUnsupportedMediaType, "传 gif"); got != problem.TypeUploadUnsupportedMedia {
		t.Errorf("image/gif 应 415，实得 %q", got)
	}
	if got := problemType(t, buyerUpload(t, cs.Host, b.Token, "3", "image/png", png, ""),
		http.StatusUnprocessableEntity, "不带幂等键"); got != problem.TypeInvalidRequest {
		t.Errorf("不带 Idempotency-Key 应 422，实得 %q", got)
	}
	big := bytes.Repeat([]byte{0x89}, 10<<20+1)
	wantStatus(t, buyerUpload(t, cs.Host, b.Token, "3", "image/png", big, freshIdemKey()),
		http.StatusRequestEntityTooLarge, "传 10 MB + 1 字节")

	// 头像：所有人可读（契约那张表的第二行）。
	avatar := mustBuyerUpload(t, cs, b, 2, png)
	if got := followBlob(t, cs.Host, getNoAuth(t, cs.Host, avatar.Url), "匿名读头像"); !bytes.Equal(got, png) {
		t.Fatal("匿名读回来的头像字节对不上")
	}
}

// 退款申请的 evidence_urls：只收自己传的 purpose=3；申请成功时标成已引用；
// 被驳回 / 撤回后带着同一批凭证重新申请也能成（同一个文件第二次被引用）。
func TestRefundEvidenceMustBeTheBuyersOwnProof(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "ev-owner")
	other := cs.newBuyer(t, "ev-other")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	item := lines[cs.DressSKU].Id

	mine := mustBuyerUpload(t, cs, b, 3, []byte("我的凭证 "+cs.Suffix))
	theirs := mustBuyerUpload(t, cs, other, 3, []byte("别人的凭证 "+cs.Suffix))
	myAvatar := mustBuyerUpload(t, cs, b, 2, []byte("我的头像 "+cs.Suffix))
	productImage := seedUpload(t, cs.adminShop, []byte("商品图 "+cs.Suffix))

	withEvidence := func(urls ...string) string {
		quoted := make([]string, len(urls))
		for i, u := range urls {
			quoted[i] = strconv.Quote(u)
		}
		return fmt.Sprintf(`{"items":[{"order_item_id":%d,"quantity":1}],"refund_type":1,"reason_code":3,"evidence_urls":[%s]}`,
			item, strings.Join(quoted, ","))
	}
	for name, urls := range map[string][]string{
		"别人的凭证":   {theirs.Url},
		"自己的头像":   {myAvatar.Url},
		"商品图":     {productImage.Url},
		"外链":      {"https://example.com/proof.png"},
		"带参数的变体":  {mine.Url + "?x=1"},
		"不存在的 id": {"/api/v1/uploads/999999999"},
		"同一张填两次":  {mine.Url, mine.Url},
	} {
		if p := problemOf(t, applyRefund(t, cs.Host, o.OrderNo, b.Token, withEvidence(urls...), "ev-"+uniqueKey()),
			http.StatusUnprocessableEntity); p.Type != problem.TypeInvalidRequest {
			t.Errorf("%s 当凭证应 422 invalid-request，实得 %+v", name, p)
		}
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM refunds r JOIN orders o ON o.id = r.order_id
	                             WHERE o.order_no = $1`, o.OrderNo); n != 0 {
		t.Fatalf("被拒的申请落了 %d 张退款单", n)
	}

	r := cs.mustApply(t, b, o.OrderNo, withEvidence(mine.Url))
	if r.EvidenceUrls == nil || len(*r.EvidenceUrls) != 1 || (*r.EvidenceUrls)[0] != mine.Url {
		t.Fatalf("退款单上的 evidence_urls 是 %v", r.EvidenceUrls)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM uploads WHERE id = $1 AND referenced`, mine.Id); n != 1 {
		t.Fatal("申请成功之后凭证没有被标成已引用 —— 24 小时后孤儿回收会把它删掉")
	}

	// 撤回之后带着同一张凭证重新申请：同一个文件第二次被引用。
	wantStatus(t, postWithKey(t, cs.Host, "/api/v1/refunds/"+r.RefundNo+"/cancel", "", b.Token, "c-"+uniqueKey()),
		http.StatusOK, "撤回")
	cs.mustApply(t, b, o.OrderNo, withEvidence(mine.Url))
}

// 后台客服读凭证：按引用它的退款单判权；没被引用的凭证谁都读不到；商品图照读；
// 买家令牌在后台路由上不算数。（门店范围内外那几格在权限矩阵里。）
func TestAdminReadsEvidenceThroughTheRefund(t *testing.T) {
	cs := uploadShop(t)
	b := cs.newBuyer(t, "ev-admin")
	o := cs.placePaid(t, b, cs.NorthStore, cs.DressSKU, 1, nil)
	_, lines := cs.lines(t, b, o.OrderNo)
	content := []byte("给客服看的凭证 " + cs.Suffix)
	used := mustBuyerUpload(t, cs, b, 3, content)
	unused := mustBuyerUpload(t, cs, b, 3, []byte("传了没提交 "+cs.Suffix))
	cs.mustApply(t, b, o.OrderNo, fmt.Sprintf(
		`{"items":[{"order_item_id":%d,"quantity":1}],"refund_type":1,"reason_code":3,"evidence_urls":[%q]}`,
		lines[cs.DressSKU].Id, used.Url))

	adminPath := func(id int64) string { return fmt.Sprintf("/api/v1/admin/uploads/%d", id) }
	if got := followBlob(t, cs.Host, getAs(t, cs.Host, adminPath(used.Id), cs.Token), "客服读凭证"); !bytes.Equal(got, content) {
		t.Fatal("客服读回来的凭证字节对不上")
	}
	if got := problemType(t, getAs(t, cs.Host, adminPath(unused.Id), cs.Token), http.StatusForbidden,
		"客服读没被引用的凭证"); got != problem.TypeUploadForbidden {
		t.Fatalf("没被引用的凭证应 403 upload-forbidden，实得 %q", got)
	}
	img := seedUpload(t, cs.adminShop, []byte("商品图 "+cs.Suffix))
	wantStatus(t, getAs(t, cs.Host, adminPath(img.Id), cs.Token), http.StatusFound, "客服读商品图")
	wantStatus(t, getAs(t, cs.Host, adminPath(used.Id), b.Token), http.StatusUnauthorized, "买家令牌打后台读文件")
	wantStatus(t, getAs(t, cs.Host, adminPath(999999999), cs.Token), http.StatusNotFound, "后台读不存在的文件")
}
