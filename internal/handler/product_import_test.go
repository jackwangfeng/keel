package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/app"
	"github.com/keel/keel/internal/catalogimport"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/service"
	"github.com/keel/keel/internal/tenant"
)

// 商品批量导入（契约 /admin/product-imports）的端到端测试：走真实路由、真实会话、真库。
//
// 类目推荐挂的是 conceptEmbedder（search_fixture_test.go）：标题里有「裙 / 女装 / 雪纺」
// 就落在同一根轴上，于是「雪纺碎花连衣裙 → 女装」是一条可预测的推荐，
// 而一个一个概念词都不含的标题只剩扰动，分数低到过不了门 —— 两种决定都有靶子。
// 真模型的推荐质量不在这里验（那是离线评测的事，make category-eval）。

const importHeader = "商品标题,副标题,类目,规格名,规格值,SKU 编码,基准价（元）,库存,重量（克）,图片 URL,描述\n"

// importReq 发一个 multipart 请求到导入接口。categories 为空串时不带那一项；
// key 为空串时不带 Idempotency-Key。
func importReq(t *testing.T, host, token, path string, file []byte, categories, key string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "商品.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(file); err != nil {
		t.Fatal(err)
	}
	switch {
	case strings.HasPrefix(categories, "json-part:"):
		// 照契约的 encoding（contentType: application/json）发成一个带文件名的 part ——
		// 浏览器里 FormData.append(name, Blob) 就是这个形状。
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="categories"; filename="blob"`)
		h.Set("Content-Type", "application/json")
		pw, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pw.Write([]byte(strings.TrimPrefix(categories, "json-part:"))); err != nil {
			t.Fatal(err)
		}
	case categories != "":
		if err := mw.WriteField("categories", categories); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, &buf)
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

func previewImport(t *testing.T, sh adminShop, file []byte) api.ProductImportPreview {
	t.Helper()
	var pv api.ProductImportPreview
	decodeInto(t, importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports/preview", file, "", ""),
		http.StatusOK, "预检", &pv)
	return pv
}

func choicesJSON(t *testing.T, m map[int]int64) string {
	t.Helper()
	var cs []api.ProductImportCategoryChoice
	for row, id := range m {
		cs = append(cs, api.ProductImportCategoryChoice{FirstRow: row, CategoryId: id})
	}
	b, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// namedCategory 建一个名字**不带后缀**的类目（推荐与名字匹配都要认得出原名）。
func namedCategory(t *testing.T, sh adminShop, name string, parent *int64) int64 {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	if parent != nil {
		body = fmt.Sprintf(`{"name":%q,"parent_id":%d}`, name, *parent)
	}
	var c api.AdminCategory
	decodeInto(t, postIdem(t, sh.Host, "/api/v1/admin/categories", body, sh.Token), http.StatusCreated, "建类目", &c)
	return c.Id
}

func productCount(t *testing.T, sh adminShop) int64 {
	t.Helper()
	return adminQueryInt64(t, `SELECT count(*) FROM products WHERE merchant_id = $1`, sh.MerchantID)
}

func findRow(t *testing.T, pv api.ProductImportPreview, line int) api.ProductImportRow {
	t.Helper()
	for _, r := range pv.Rows {
		if r.Row == line {
			return r
		}
	}
	t.Fatalf("预检结果里没有第 %d 行", line)
	return api.ProductImportRow{}
}

func findProduct(t *testing.T, pv api.ProductImportPreview, firstRow int) api.ProductImportProduct {
	t.Helper()
	for _, p := range pv.Products {
		if p.FirstRow == firstRow {
			return p
		}
	}
	t.Fatalf("预检结果里没有首行为 %d 的商品", firstRow)
	return api.ProductImportProduct{}
}

func issueCodes(is []api.ProductImportIssue) []string {
	var out []string
	for _, i := range is {
		out = append(out, i.Code)
	}
	return out
}

func hasImportIssue(is []api.ProductImportIssue, code string) bool {
	for _, i := range is {
		if i.Code == code {
			return true
		}
	}
	return false
}

// importFixture 是一家有四个类目的店与一份覆盖各种情况的 csv。
type importFixture struct {
	sh                  adminShop
	women, coffee, coat int64
	file                []byte
}

func newImportFixture(t *testing.T) importFixture {
	t.Helper()
	sh := newAdminShop(t)
	fx := importFixture{sh: sh}
	fx.women = namedCategory(t, sh, "女装", nil)
	fx.coffee = namedCategory(t, sh, "咖啡器具", nil)
	apparel := namedCategory(t, sh, "服装", nil)
	fx.coat = namedCategory(t, sh, "外套", &apparel)
	s := sh.Suffix
	fx.file = []byte(importHeader +
		// 第 2–3 行：一件两规格的连衣裙，类目留空 → 按标题推荐「女装」。
		"雪纺碎花连衣裙,夏季新款,,颜色;尺码,红;M,DR-" + s + "-RM,199.9,10,300,https://img.example.com/a.jpg,100% 雪纺\n" +
		"雪纺碎花连衣裙,,,颜色;尺码,红;L,DR-" + s + "-RL,199.90,5,,,\n" +
		// 第 4 行：类目名对上 → matched。副标题里有违禁词 → 提示但不阻断。
		"手冲咖啡壶,全网最佳手感,咖啡器具,,,POT-" + s + ",89,3,,,\n" +
		// 第 5 行：路径写法对上二级类目。
		"羊毛大衣,,服装 > 外套,,,COAT-" + s + ",599,2,,,\n" +
		// 第 6 行：一个概念词都没有 → 推荐分数过不了门 → needs_review。
		"神秘盲盒,,,,,BOX-" + s + ",9.9,100,,,\n" +
		// 第 7 行：价格写错 → 整件不导入。
		"坏价格商品,,咖啡器具,,,BAD-" + s + ",12.345,1,,,\n")
	return fx
}

func TestImportTemplateDownloads(t *testing.T) {
	sh := newAdminShop(t)
	w := reqAs(t, http.MethodGet, sh.Host, "/api/v1/admin/product-imports/template", "", sh.Token)
	wantStatus(t, w, http.StatusOK, "下载 xlsx 模板")
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "spreadsheetml") {
		t.Errorf("xlsx 模板的 Content-Type 不对：%q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition 不对：%q", cd)
	}
	f, err := excelize.OpenReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("模板不是合法的 xlsx：%v", err)
	}
	rows, err := f.GetRows(catalogimport.SheetName)
	if err != nil || len(rows) != 1 {
		t.Fatalf("「%s」表应当只有表头一行：%v %v", catalogimport.SheetName, rows, err)
	}
	for i, c := range catalogimport.Columns {
		if rows[0][i] != c.Header {
			t.Errorf("模板第 %d 列是 %q，期望 %q", i+1, rows[0][i], c.Header)
		}
	}

	w = reqAs(t, http.MethodGet, sh.Host, "/api/v1/admin/product-imports/template?format=csv", "", sh.Token)
	wantStatus(t, w, http.StatusOK, "下载 csv 模板")
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("\xEF\xBB\xBF商品标题,")) {
		t.Errorf("csv 模板要以 BOM + 表头开始：%q", w.Body.String())
	}
	if got := problemType(t, reqAs(t, http.MethodGet, sh.Host, "/api/v1/admin/product-imports/template?format=pdf", "", sh.Token),
		http.StatusUnprocessableEntity, "不认得的格式"); got != "https://keel.dev/problems/invalid-request" {
		t.Errorf("不认得的格式应当是 invalid-request，得到 %s", got)
	}
}

func TestImportPreviewReportsEverythingAndWritesNothing(t *testing.T) {
	fx := newImportFixture(t)
	sh := fx.sh
	// 店里已经有一个 SKU 占着第 6 行的编码 → sku_code_exists。
	existingCat := namedCategory(t, sh, "占位类目", nil)
	createPublishedSKU(t, sh, existingCat, "已有商品", 100)
	taken := adminQueryText(t, `SELECT sku_code FROM skus WHERE merchant_id = $1`, sh.MerchantID)
	file := append(append([]byte{}, fx.file...), []byte("撞编码商品,,咖啡器具,,,"+taken+",1,1,,,\n")...)

	before := productCount(t, sh)
	pv := previewImport(t, sh, file)
	if after := productCount(t, sh); after != before {
		t.Fatalf("预检不该写库：商品数 %d → %d", before, after)
	}
	if pv.TotalRows != 7 || len(pv.Products) != 6 || pv.Format != "csv" || len(pv.FileSha256) != 64 {
		t.Fatalf("预检总览不对：rows=%d products=%d format=%s", pv.TotalRows, len(pv.Products), pv.Format)
	}
	if pv.CategoryEngine != "ok" || pv.CategoryGate.MinMargin <= 0 {
		t.Errorf("引擎状态 / 判据没回：%s %+v", pv.CategoryEngine, pv.CategoryGate)
	}

	dress := findProduct(t, pv, 2)
	if dress.Category.Status != "recommended" || dress.Category.CategoryId == nil || *dress.Category.CategoryId != fx.women {
		t.Errorf("连衣裙应当被推荐到「女装」：%+v", dress.Category)
	}
	if len(dress.Category.Candidates) == 0 || dress.Category.Candidates[0].PathName != "女装" || len(dress.Rows) != 2 || !dress.Importable {
		t.Errorf("连衣裙的候选 / 行 / 可导入不对：%+v", dress)
	}
	if r := findRow(t, pv, 3); r.PriceCents == nil || *r.PriceCents != 19990 || r.SpecValues["尺码"] != "L" || r.FirstRow != 2 {
		t.Errorf("第 3 行解析不对：%+v", r)
	}

	pot := findProduct(t, pv, 4)
	if pot.Category.Status != "matched" || *pot.Category.CategoryId != fx.coffee {
		t.Errorf("咖啡壶的类目应当按名字对上：%+v", pot.Category)
	}
	potRow := findRow(t, pv, 4)
	if len(potRow.Violations) != 1 || *potRow.Violations[0].Field != "subtitle" || *potRow.Violations[0].Offset != 2 {
		t.Errorf("副标题里的「最佳」应当标出来（第 3 个字起）：%+v", potRow.Violations)
	}
	if !pot.Importable {
		t.Error("违禁词只是提示，不该让商品变成不可导入")
	}

	coat := findProduct(t, pv, 5)
	if coat.Category.Status != "matched" || *coat.Category.CategoryId != fx.coat || *coat.Category.PathName != "服装 > 外套" {
		t.Errorf("路径写法应当对上二级类目：%+v", coat.Category)
	}

	box := findProduct(t, pv, 6)
	if box.Category.Status != "needs_review" || box.Category.CategoryId != nil || len(box.Category.Candidates) == 0 {
		t.Errorf("没有概念词的标题应当是 needs_review（给候选但不替商家选）：%+v", box.Category)
	}

	bad := findProduct(t, pv, 7)
	if bad.Importable || !hasImportIssue(findRow(t, pv, 7).Errors, catalogimport.CodeInvalidPrice) {
		t.Errorf("价格写错的那件应当不可导入：%+v / %v", bad, issueCodes(findRow(t, pv, 7).Errors))
	}
	if r := findRow(t, pv, 8); !hasImportIssue(r.Errors, catalogimport.CodeSKUCodeExists) {
		t.Errorf("店里已有的编码应当报 sku_code_exists：%v", issueCodes(r.Errors))
	}
	if pv.ErrorRows != 2 {
		t.Errorf("error_rows 应当是 2，得到 %d", pv.ErrorRows)
	}
	if pv.PreviousImport != nil {
		t.Error("这份文件还没导入过")
	}
}

func TestImportCommitCreatesDraftsAndIsIdempotentTwice(t *testing.T) {
	fx := newImportFixture(t)
	sh := fx.sh
	pv := previewImport(t, sh, fx.file)
	// 界面的做法：推荐的原样带回来，needs_review 的那件人工选「女装」以外的一个。
	choices := choicesJSON(t, map[int]int64{2: *findProduct(t, pv, 2).Category.CategoryId, 6: fx.coffee})

	// categories 按契约的 encoding 发成 application/json 的 part（后台界面就是这么发的）；
	// 下面几次重放 / 再确认用普通字段，两种形状都要认。
	key := freshIdemKey()
	w := importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", fx.file, "json-part:"+choices, key)
	var res api.ProductImportResult
	decodeInto(t, w, http.StatusCreated, "确认导入", &res)
	if res.AlreadyImported || res.CreatedProducts != 4 || res.CreatedSkus != 5 || res.FailedRows != 1 || res.TotalRows != 6 {
		t.Fatalf("回执计数不对：%+v", res)
	}
	byRow := map[int]api.ProductImportOutcome{}
	for _, o := range res.Products {
		byRow[o.FirstRow] = o
	}
	if o := byRow[7]; o.Status != "failed" || o.Reasons == nil || !strings.Contains((*o.Reasons)[0], "第 7 行") {
		t.Errorf("坏价格那件应当 failed 并带行号原因：%+v", o)
	}
	dress := byRow[2]
	if dress.Status != "created" || dress.ProductId == nil || *dress.SkuCount != 2 ||
		dress.ImageUrls == nil || (*dress.ImageUrls)[0] != "https://img.example.com/a.jpg" {
		t.Fatalf("连衣裙回执不对：%+v", dress)
	}

	// 草稿、价格、库存（进了默认门店）、重量、规格都落对了。
	var detail api.AdminProductDetail
	decodeInto(t, reqAs(t, http.MethodGet, sh.Host, fmt.Sprintf("/api/v1/admin/products/%d", *dress.ProductId), "", sh.Token),
		http.StatusOK, "读导入的商品", &detail)
	if detail.Status != 0 || detail.CategoryId != fx.women || *detail.Subtitle != "夏季新款" ||
		*detail.Description != "100% 雪纺" {
		t.Errorf("导入的商品应当是「女装」下的草稿：%+v", detail)
	}
	if len(detail.Skus) != 2 {
		t.Fatalf("应当有 2 个 SKU：%+v", detail.Skus)
	}
	for _, s := range detail.Skus {
		if s.SkuCode == "DR-"+sh.Suffix+"-RM" {
			if s.PriceCents != 19990 || s.AvailableQty != 10 || *s.WeightGram != 300 || (*s.SpecValues)["颜色"] != "红" {
				t.Errorf("SKU 落库不对：%+v", s)
			}
		}
	}
	if qty := adminQueryInt64(t, `SELECT i.available_qty FROM inventories i JOIN skus s ON s.id = i.sku_id
		WHERE s.merchant_id = $1 AND s.sku_code = $2 AND i.store_id = $3`, sh.MerchantID, "DR-"+sh.Suffix+"-RM", sh.StoreID); qty != 10 {
		t.Errorf("库存应当写进默认门店：%d", qty)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM products WHERE merchant_id = $1 AND category_id = $2`,
		sh.MerchantID, fx.coffee); n != 2 {
		t.Errorf("咖啡器具下应当有 2 件（咖啡壶 + 人工选的盲盒），得到 %d", n)
	}

	count := productCount(t, sh)

	// 第一层：同一把钥匙重放。
	w = importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", fx.file, choices, key)
	var replay api.ProductImportResult
	decodeInto(t, w, http.StatusCreated, "同一把钥匙重放", &replay)
	if w.Header().Get("Idempotency-Replayed") != "true" || replay.ImportId != res.ImportId {
		t.Errorf("同一把钥匙应当重放首次结果：%v %d/%d", w.Header(), replay.ImportId, res.ImportId)
	}

	// 第二层：换一把钥匙、换一种类目选择，同一份文件 —— 不再建任何东西。
	w = importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", fx.file,
		choicesJSON(t, map[int]int64{2: fx.coffee, 6: fx.coffee}), freshIdemKey())
	var again api.ProductImportResult
	decodeInto(t, w, http.StatusCreated, "同一份文件再确认", &again)
	if !again.AlreadyImported || again.ImportId != res.ImportId || again.CreatedProducts != res.CreatedProducts {
		t.Errorf("同一份文件应当返回那一次的回执并标 already_imported：%+v", again)
	}
	if got := productCount(t, sh); got != count {
		t.Fatalf("重复确认不该建商品：%d → %d", count, got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM product_import_batches WHERE merchant_id = $1`, sh.MerchantID); n != 1 {
		t.Errorf("导入记录应当只有一行，得到 %d", n)
	}

	// 预检同一份文件：告诉商家已经导入过；里面的编码也都已存在。
	pv2 := previewImport(t, sh, fx.file)
	if pv2.PreviousImport == nil || pv2.PreviousImport.ImportId != res.ImportId {
		t.Errorf("预检应当提示这份文件已导入过：%+v", pv2.PreviousImport)
	}
	if !hasImportIssue(findRow(t, pv2, 2).Errors, catalogimport.CodeSKUCodeExists) {
		t.Errorf("已导入的编码应当报 sku_code_exists：%v", issueCodes(findRow(t, pv2, 2).Errors))
	}

	// 修好第 7 行（另一份文件）再导：只补上那一件，已建的因编码已存在而跳过。
	fixed := bytes.Replace(fx.file, []byte("12.345"), []byte("12.34"), 1)
	w = importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", fixed, "", freshIdemKey())
	var patch api.ProductImportResult
	decodeInto(t, w, http.StatusCreated, "修好之后再导", &patch)
	if patch.AlreadyImported || patch.CreatedProducts != 1 || patch.ImportId == res.ImportId {
		t.Errorf("修好的文件应当只补上那一件：%+v", patch)
	}
}

func TestImportCommitNeedsACategoryAndSomethingToImport(t *testing.T) {
	sh := newAdminShop(t)
	namedCategory(t, sh, "女装", nil)
	file := []byte(importHeader + "神秘盲盒,,,,,NC-" + sh.Suffix + ",1,1,,,\n")
	w := importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file, "", freshIdemKey())
	if got := problemType(t, w, http.StatusUnprocessableEntity, "一件都导不了"); got != "https://keel.dev/problems/import-nothing-to-import" {
		t.Errorf("没选类目的唯一一件：期望 import-nothing-to-import，得到 %s", got)
	}
	if n := adminQueryInt64(t, `SELECT count(*) FROM product_import_batches WHERE merchant_id = $1`, sh.MerchantID); n != 0 {
		t.Errorf("什么都没导入时不该留导入记录（整个事务回滚），得到 %d 行", n)
	}
	// 同一份文件、选上类目之后能导：上一次的失败没有占住「这份文件导入过」。
	cat := namedCategory(t, sh, "杂货", nil)
	w = importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file,
		choicesJSON(t, map[int]int64{2: cat}), freshIdemKey())
	var res api.ProductImportResult
	decodeInto(t, w, http.StatusCreated, "选了类目再导", &res)
	if res.CreatedProducts != 1 {
		t.Errorf("选了类目应当导入 1 件：%+v", res)
	}
	// 选一个不存在的类目：那一件 failed（而这里只有这一件，所以还是 422）。
	file2 := []byte(importHeader + "另一个盲盒,,,,,NC2-" + sh.Suffix + ",1,1,,,\n")
	w = importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file2,
		choicesJSON(t, map[int]int64{2: 999999999}), freshIdemKey())
	problemType(t, w, http.StatusUnprocessableEntity, "选了不存在的类目")
}

func TestImportRejectsBadFilesAndRequests(t *testing.T) {
	sh := newAdminShop(t)
	base := "/api/v1/admin/product-imports"
	for _, c := range []struct {
		name, path string
		file       []byte
		key        string
		code       int
		typ        string
	}{
		{"超过 5 MB", base + "/preview", bytes.Repeat([]byte("a"), int(catalogimport.MaxFileBytes)+10), "", 413, "import-file-too-large"},
		{"老式 xls", base + "/preview", []byte("\xD0\xCF\x11\xE0 old"), "", 415, "import-unsupported-format"},
		{"缺必填列", base + "/preview", []byte("商品标题,库存\n连衣裙,1\n"), "", 422, "import-file-invalid"},
		{"只有表头", base + "/preview", []byte(importHeader), "", 422, "import-file-invalid"},
		{"确认时没带钥匙", base, []byte(importHeader + "x,,,,,K1,1,1,,,\n"), "", 422, "invalid-request"},
	} {
		w := importReq(t, sh.Host, sh.Token, c.path, c.file, "", c.key)
		if got := problemType(t, w, c.code, c.name); got != "https://keel.dev/problems/"+c.typ {
			t.Errorf("%s：期望 %s，得到 %s", c.name, c.typ, got)
		}
	}
	// 文件级错误要逐条说出原因。
	w := importReq(t, sh.Host, sh.Token, base+"/preview", []byte("商品标题,库存\n连衣裙,1\n"), "", "")
	var p api.Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Errors == nil || len(*p.Errors) != 2 || !strings.Contains(*(*p.Errors)[0].Message, "SKU 编码") {
		t.Errorf("缺两列应当列出两条原因：%s", w.Body.String())
	}
	// categories 不是 JSON。
	w = importReq(t, sh.Host, sh.Token, base, []byte(importHeader+"x,,,,,K2,1,1,,,\n"), "not-json", freshIdemKey())
	problemType(t, w, http.StatusUnprocessableEntity, "categories 不是 JSON")
	// 未登录。
	w = importReq(t, sh.Host, "", base+"/preview", []byte(importHeader), "", "")
	wantStatus(t, w, http.StatusUnauthorized, "未登录")
}

// TestImportXLSXEndToEnd 用一份 excelize 造的 xlsx（数字格价格、合并的标题）走完预检与确认。
func TestImportXLSXEndToEnd(t *testing.T) {
	sh := newAdminShop(t)
	women := namedCategory(t, sh, "女装", nil)
	f := excelize.NewFile()
	s := "Sheet1"
	for i, c := range catalogimport.Columns {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellStr(s, cell, c.Header)
	}
	_ = f.SetCellStr(s, "A2", "真丝吊带长裙")
	_ = f.MergeCell(s, "A2", "A3")
	_ = f.SetCellStr(s, "C2", "女装")
	_ = f.SetCellStr(s, "D2", "尺码")
	_ = f.SetCellStr(s, "E2", "S")
	_ = f.SetCellStr(s, "F2", "SILK-"+sh.Suffix+"-S")
	_ = f.SetCellValue(s, "G2", 0.29)
	_ = f.SetCellValue(s, "H2", 3)
	_ = f.SetCellStr(s, "D3", "尺码")
	_ = f.SetCellStr(s, "E3", "M")
	_ = f.SetCellStr(s, "F3", "SILK-"+sh.Suffix+"-M")
	_ = f.SetCellValue(s, "G3", 1299.5)
	_ = f.SetCellValue(s, "H3", 4)
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	pv := previewImport(t, sh, buf.Bytes())
	if pv.Format != "xlsx" || len(pv.Products) != 1 || !pv.Products[0].Importable {
		t.Fatalf("xlsx 预检不对：%+v", pv)
	}
	if r := findRow(t, pv, 3); !hasImportIssue(r.Warnings, catalogimport.CodeMergedCell) || *r.PriceCents != 129950 {
		t.Errorf("合并单元格提示 / 数字格价格不对：%+v", r)
	}
	var res api.ProductImportResult
	decodeInto(t, importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", buf.Bytes(), "", freshIdemKey()),
		http.StatusCreated, "xlsx 确认导入", &res)
	if res.CreatedSkus != 2 || *res.Products[0].CategoryId != women {
		t.Fatalf("xlsx 导入回执不对：%+v", res)
	}
	if got := adminQueryInt64(t, `SELECT price_cents FROM skus WHERE merchant_id = $1 AND sku_code = $2`,
		sh.MerchantID, "SILK-"+sh.Suffix+"-S"); got != 29 {
		t.Errorf("0.29 元应当是 29 分，得到 %d", got)
	}
}

// TestImportDegradesWhenEngineIsMissingOrDown：推理引擎没配 / 挂了，预检照常 200，
// 需要推荐的商品都是 unavailable，确认导入不受影响。
func TestImportDegradesWhenEngineIsMissingOrDown(t *testing.T) {
	for _, c := range []struct {
		name   string
		emb    inference.Embedder
		engine string
	}{
		{"没配引擎", nil, "not_configured"},
		{"引擎挂了", deadEngineClient(t), "unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			eng := app.Router(testPool, tenant.NewResolver(testPool, tenant.Config{BaseDomain: baseDomain}),
				testSigner, testOrders, service.PaymentConfig{Sandbox: true}, c.emb)
			sh := newAdminShop(t)
			women := namedCategory(t, sh, "女装", nil)
			file := []byte(importHeader + "雪纺碎花连衣裙,,,,,ENG-" + sh.Suffix + ",1,1,,,\n")

			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			part, _ := mw.CreateFormFile("file", "x.csv")
			_, _ = part.Write(file)
			_ = mw.Close()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/product-imports/preview", &buf)
			r.Host = sh.Host
			r.Header.Set("Content-Type", mw.FormDataContentType())
			r.Header.Set("Authorization", "Bearer "+sh.Token)
			w := httptest.NewRecorder()
			eng.ServeHTTP(w, r.WithContext(context.Background()))
			var pv api.ProductImportPreview
			decodeInto(t, w, http.StatusOK, "引擎不可用时的预检", &pv)
			if string(pv.CategoryEngine) != c.engine || pv.Products[0].Category.Status != "unavailable" {
				t.Fatalf("期望 %s / unavailable，得到 %s / %s", c.engine, pv.CategoryEngine, pv.Products[0].Category.Status)
			}
			if !pv.Products[0].Importable {
				t.Error("引擎不可用不能让商品变得不可导入")
			}
			var res api.ProductImportResult
			decodeInto(t, importReq(t, sh.Host, sh.Token, "/api/v1/admin/product-imports", file,
				choicesJSON(t, map[int]int64{2: women}), freshIdemKey()), http.StatusCreated, "手选类目后导入", &res)
			if res.CreatedProducts != 1 {
				t.Errorf("手选类目后应当照常导入：%+v", res)
			}
		})
	}
}
