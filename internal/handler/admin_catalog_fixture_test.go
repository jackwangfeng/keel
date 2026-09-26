package handler_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/keel/keel/internal/db"
)

// 商家自助发布那一组测试的夹具。
//
// ===========================================================================
// 为什么自己造一家店，而不是用种子里的 shop-a
// ===========================================================================
//
// 因为这一组测试**会写**：建商品、建 SKU、改库存、上下架。写进 shop-a 的话，
// 同一个包里那几条数着 shop-a 有几件商品的测试（product_test.go 的
// TestDraftAndDeletedProductsAreInvisible 拿 rawProductCount 做阳性对照）
// 就要跟着我们的写入一起动 —— 而它们红起来的时候，真因是这个文件。
//
// 这正是 main_test.go 里 TestMain 上那段「可变状态会跨轮次累积」记下的
// 同一类问题的另一面：那边是跨轮次，这边是跨测试。

// adminShop 是一家临时的店，带一个在岗的商家级管理员会话。
type adminShop struct {
	Host       string
	MerchantID int64
	StaffID    int64
	// StoreID 是这家店唯一的那家默认门店。00020 之后它不是可选的夹具细节：
	// CreateSKU 在同事务里建库存行那一步（CreateInventoryRow）挑的就是默认
	// 门店，**没有默认店时它一行都不建，而且那不算失败** —— 于是漏掉这一行
	// 夹具的症状是「新建的 SKU 永远缺货」，而不是任何一句报错。
	StoreID int64
	Token   string
	Suffix  string
}

// newAdminShop 造一家**真的能被 Host 解析出来**的店（status = 1 且有域名），
// 一个商家级管理员，并走真实的 POST /admin/auth/session 换一串会话 token。
//
// 会话从真实路径来，不自己用 testSigner 签一串：自己签的话，「这 16 条接口
// 认不认后台会话」这件事验的就只是「一串我们自己造的东西能过」。
func newAdminShop(t *testing.T) adminShop {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	suffix := fmt.Sprintf("cat%d", time.Now().UnixNano()%1_000_000_000)
	code := suffix

	var merchantID int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO merchants (code, name, status) VALUES ($1, '商家写路径店', 1)
		 RETURNING id`, code).Scan(&merchantID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		// 顺序是外键决定的，不是随手排的。product_images 指向 products 与
		// uploads；uploads 指向 staff —— 所以 staff 必须排在 uploads 后面，
		// 否则 `DELETE FROM staff` 会以 23503 失败，而那条错误会出现在
		// **别的**测试里（清理是 t.Cleanup，失败的却是下一条用到 staff 的测试）。
		//
		// 00020 又加了三条边：inventories → stores → regions → merchants，
		// 而 store_product_overrides / store_sku_prices 也挂在 stores 上
		// （这一组测试会调上下架与定价那几条接口）。它们全排在 stores 之前。
		// inventories 自带 merchant_id 了，不必再绕 skus 的子查询。
		for _, stmt := range []string{
			// jobs 指向 merchants，而且它是**跨租户共享**的（00022：这张表没有
			// RLS）。留下的待执行任务会被别的测试里那个 Drain 捞走，
			// 而那时它指向的商品已经不在了。
			`DELETE FROM jobs WHERE merchant_id = $1`,
			`DELETE FROM product_images WHERE merchant_id = $1`,
			`DELETE FROM product_text_vectors WHERE merchant_id = $1`,
			`DELETE FROM product_understanding WHERE merchant_id = $1`,
			`DELETE FROM inventories WHERE merchant_id = $1`,
			`DELETE FROM store_sku_prices WHERE merchant_id = $1`,
			`DELETE FROM region_sku_prices WHERE merchant_id = $1`,
			`DELETE FROM store_product_overrides WHERE merchant_id = $1`,
			`DELETE FROM region_product_overrides WHERE merchant_id = $1`,
			`DELETE FROM skus WHERE merchant_id = $1`,
			`DELETE FROM products WHERE merchant_id = $1`,
			// 运费模板（00055）：products 挂着它、它挂着 stores，排在两者之间。
			`DELETE FROM freight_template_rules WHERE merchant_id = $1`,
			`DELETE FROM freight_templates WHERE merchant_id = $1`,
			`DELETE FROM categories WHERE merchant_id = $1`,
			// staff_scopes（00025）指向 stores / regions / staff。三条外键都是
			// ON DELETE CASCADE，这一句不写也删得掉；写出来是为了让这张清单
			// 仍然是「这家店名下有哪些表」的完整答案。
			`DELETE FROM staff_scopes WHERE merchant_id = $1`,
			`DELETE FROM stores WHERE merchant_id = $1`,
			`DELETE FROM regions WHERE merchant_id = $1`,
			`DELETE FROM uploads WHERE merchant_id = $1`,
			// product_import_batches（00054）指向 staff 与 merchants：批量导入那一组
			// 与权限矩阵每确认一次就留一行，要排在 staff 之前删。
			`DELETE FROM product_import_batches WHERE merchant_id = $1`,
			`DELETE FROM staff WHERE merchant_id = $1`,
			// 幂等键那张表也指向 merchants（merchant_id 上有外键）。
			// M4 收尾给后台写接口接上幂等之后，这一组测试每跑一次就会在这里
			// 留下几行 —— 不删的话下面那句 DELETE FROM merchants 会以 23503
			// 失败，而那条错误会出现在**别的**测试里（清理是 t.Cleanup）。
			`DELETE FROM idempotency_keys WHERE merchant_id = $1`,
			// search_logs（00027）同理：买家侧那条「发布之后搜得到」每搜一次
			// 就写一行检索日志，而它对 merchants 有外键。
			`DELETE FROM search_logs WHERE merchant_id = $1`,
			// 店铺设置（00059）：改过设置的店在这里有一行，它对 merchants 有外键。
			`DELETE FROM shop_preferences WHERE merchant_id = $1`,
			`DELETE FROM merchants WHERE id = $1`,
		} {
			if _, err := admin.Exec(c, stmt, merchantID); err != nil {
				t.Errorf("清理失败 (%s): %v", stmt, err)
			}
		}
	})

	// 一个大区 + 一家默认门店。00020 的回填只覆盖迁移那一刻库里已有的商家，
	// 这一家是测试现建的，所以门店得自己播（理由见 StoreID 那段注释）。
	// is_default = TRUE：默认店靠「全国兜底」接单，不画围栏是正常形态；
	// 这一组测试一条都不按坐标或围栏选店。
	var regionID, storeID int64
	if err := admin.QueryRow(ctx,
		`INSERT INTO regions (merchant_id, code, name) VALUES ($1,'default','默认大区')
		 RETURNING id`, merchantID).Scan(&regionID); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO stores (merchant_id, region_id, code, name, is_default)
		 VALUES ($1,$2,'default','默认门店',TRUE) RETURNING id`,
		merchantID, regionID).Scan(&storeID); err != nil {
		t.Fatal(err)
	}

	staffID := mkStaff(t, code, "boss-"+suffix+"@keel.test", 1, 1)
	host := code + "." + baseDomain
	sess := staffSession(t, host, staffID)
	return adminShop{Host: host, MerchantID: merchantID, StaffID: staffID,
		StoreID: storeID, Token: sess.Token, Suffix: suffix}
}

// ---------------------------------------------------------------------------
// 请求小工具
// ---------------------------------------------------------------------------

func reqAs(t *testing.T, method, host, path, body, bearer string) *httptest.ResponseRecorder {
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
	// POST 一律带一把**新鲜**的 Idempotency-Key。
	//
	// 后台那几条 POST 在 M4 收尾之后把它当成必填（不带是 422），而这个工具是
	// 表驱动测试的通用入口 —— 那张表里 POST 与 PATCH / DELETE 混在一起，
	// 它们要断言的是同一件事（跨租户一律 404）。不带的话，表里那两行 POST
	// 会在 422 上失败，而失败信息指向幂等键，与那条测试要验的东西毫无关系。
	// 每次一把新钥匙：这个工具的调用之间没有任何幂等关系。
	// 别的方法不带这个头 —— 契约只在 POST 上声明了它。
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", freshIdemKey())
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

func putAs(t *testing.T, host, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return reqAs(t, http.MethodPut, host, path, body, bearer)
}

func deleteAs(t *testing.T, host, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return reqAs(t, http.MethodDelete, host, path, "", bearer)
}

// uploadImage 走真实的 POST /admin/uploads 传一个 multipart 文件。
//
// contentType 是**写在 part 头上的**那个，与服务端判 415 的判据一致；
// 内容是不是真的一张 PNG 不重要，那正是服务端刻意不嗅探的东西
// （判据是声明值，理由写在 admin_upload.go 上）。
func uploadImage(t *testing.T, sh adminShop, contentType string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	// 每次一把新钥匙，理由同 postIdem：夹具造数据时两次上传之间没有任何
	// 幂等关系。要复用同一把的那条测试走 uploadImageWithKey。
	return uploadImageWithKey(t, sh, contentType, content, freshIdemKey())
}

// uploadImageWithKey 与上面一样，但钥匙由调用方给。
func uploadImageWithKey(t *testing.T, sh adminShop, contentType string,
	content []byte, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="x.bin"`)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
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
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/uploads", &buf)
	r.Host = sh.Host
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+sh.Token)
	if idemKey != "" {
		r.Header.Set("Idempotency-Key", idemKey)
	}
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

// ---------------------------------------------------------------------------
// 断言小工具
// ---------------------------------------------------------------------------

// wantStatus 在状态码不对时**把响应体一起打出来**。
//
// 不打响应体的话，一条 409 变成 404 时看到的只有两个数字，而真正的信息
// （Problem 的 type）在体里 —— 这一组测试的全部意义就是分辨那些 type。
func wantStatus(t *testing.T, w *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("%s：状态码 %d，期望 %d。响应体：%s", what, w.Code, want, w.Body.String())
	}
}

// problemType 取响应体里的 Problem type，顺带核一次状态码与 Content-Type。
//
// Content-Type 也核：契约里每条错误响应都是 application/problem+json，
// 而一个 application/json 的 409 会让按契约生成的客户端在它最需要读懂的
// 那类响应上走错分支。
func problemType(t *testing.T, w *httptest.ResponseRecorder, wantCode int, what string) string {
	t.Helper()
	wantStatus(t, w, wantCode, what)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("%s：Content-Type 是 %q，期望 application/problem+json", what, ct)
	}
	var p struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s：Problem 解析失败 %v\n%s", what, err, w.Body.String())
	}
	if p.Status != wantCode {
		t.Errorf("%s：Problem 体里的 status 是 %d，而 HTTP 状态码是 %d —— 两者必须一致",
			what, p.Status, wantCode)
	}
	return p.Type
}

// decodeInto 把成功响应解进 v，顺带核状态码。
func decodeInto(t *testing.T, w *httptest.ResponseRecorder, wantCode int, what string, v any) {
	t.Helper()
	wantStatus(t, w, wantCode, what)
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%s：解析失败 %v\n%s", what, err, w.Body.String())
	}
}

// postIdem 发一个带**新鲜** Idempotency-Key 的 POST。
//
// 后台那 5 条 POST 在 M4 收尾之后把这个头当成必填（不带是 422），所以这一组
// 测试里凡是打那 5 条的地方都走它。每次一把新钥匙 —— 夹具造数据时两次调用
// 之间没有任何幂等关系，共用一把钥匙会让第二次静默变成重放，
// 而那时红的会是后面某条断言，真因在这里。
//
// 想复用同一把钥匙（那正是幂等要测的东西）用 postWithKey；
// 想**不带**这个头（那是「必填」那条测试要的）用 post。
func postIdem(t *testing.T, host, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	return postWithKey(t, host, path, body, bearer, freshIdemKey())
}

// freshIdemKey 造一把没人用过的钥匙。形状随便，只要够长、够独特 ——
// 服务端不解析它（契约说它是客户端生成的 UUID，而服务端把它当不透明串）。
func freshIdemKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// postWithKey 发一个带**指定** Idempotency-Key 的 POST。
//
// 幂等那一组测试用它：它们要的正是「同一把钥匙打两次」。
func postWithKey(t *testing.T, host, path, body, bearer, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+bearer)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	testEngine.ServeHTTP(w, r)
	return w
}

// adminQueryText 用管理员连接读一个字符串（绕过 RLS）。
func adminQueryText(t *testing.T, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, db.AdminDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var out string
	if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertUploadedFileMatches 打开 storage_key 指向的那个真实文件，逐字节比对。
//
// **这是「没有假装文件存下来了」的判据。** 只登记元数据的实现会在库里留下
// 一条指向不存在文件的记录，而它与一条正常记录在表上长得一模一样 ——
// 没有任何一列说得出「这个 key 后面什么都没有」。所以判据只能在文件系统上。
func assertUploadedFileMatches(t *testing.T, storageKey string, want []byte) {
	t.Helper()
	full := filepath.Join(testUploadRoot, filepath.FromSlash(storageKey))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("storage_key %q 指向的文件读不出来: %v —— "+
			"库里登记了一条元数据，而磁盘上什么都没有", storageKey, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("storage_key %q 指向的文件有 %d 字节，期望 %d 字节且内容一致",
			storageKey, len(got), len(want))
	}
}

// countFilesUnder 数一棵目录树下有多少个普通文件。
//
// 上传那条幂等测试拿它证明「重放没有在磁盘上留下第二份字节」。数文件而不是
// 比某个具体路径：重放那一路写出来的 key 是随机的（storage_key 刻意不是内容
// 寻址，见 upload_store.go），测试事先不知道它叫什么 —— 而那正是问题所在：
// 一个谁也不认识的文件。
func countFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	if err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	}); err != nil {
		t.Fatalf("数 %s 下的文件失败: %v", root, err)
	}
	return n
}
