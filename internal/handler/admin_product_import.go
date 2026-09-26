package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/api"
	"github.com/keel/keel/internal/catalogimport"
	"github.com/keel/keel/internal/problem"
	"github.com/keel/keel/internal/service"
)

// 商品批量导入的预检与确认（契约 /admin/product-imports/preview 与 /admin/product-imports）。
// 下载模板那一条有 query 参数，单独在 admin_product_import_template.go 里 ——
// contract_test.go 的参数对账按文件解析 c.Query，理由同 admin_product_list.go。
//
// 这里没有 SQL、没有事务：解析与校验在 internal/catalogimport，落库在 service。
// handler 只做三件事：限大小地把文件读进来、把领域结果装成契约类型、把错误翻成状态码。

// ProductImportHandler 实现批量导入的三条接口。
type ProductImportHandler struct{ svc *service.ProductImportService }

func NewProductImportHandler(s *service.ProductImportService) *ProductImportHandler {
	return &ProductImportHandler{svc: s}
}

// readImportFile 从 multipart 里取 file 那一项，读满（带上限）。写了响应就返回 ok=false。
//
// 先限大小、再解析 —— 与 /admin/uploads 同一个顺序：反过来的话，一个声称 2 GB 的
// multipart 会在判断它超没超之前就被 net/http 落进临时文件。余量 1 MB 给 multipart
// 的边界与 categories 那一项；契约那个 5 MB 说的是文件本身，下面按字节再核一次。
func readImportFile(c *gin.Context) (data []byte, name string, ok bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, catalogimport.MaxFileBytes+(1<<20))
	if err := c.Request.ParseMultipartForm(maxUploadFormMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeImportTooLarge(c)
			return nil, "", false
		}
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
			"请求体不是合法的 multipart/form-data")
		return nil, "", false
	}
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest, "请求体里没有 file 这一项")
		return nil, "", false
	}
	defer func() { _ = file.Close() }()
	data, err = io.ReadAll(io.LimitReader(file, catalogimport.MaxFileBytes+1))
	if err != nil {
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "读取上传文件失败")
		return nil, "", false
	}
	if int64(len(data)) > catalogimport.MaxFileBytes {
		writeImportTooLarge(c)
		return nil, "", false
	}
	return data, header.Filename, true
}

func writeImportTooLarge(c *gin.Context) {
	problem.Write(c, http.StatusRequestEntityTooLarge, problem.TypeImportFileTooLarge,
		"导入文件超过 5 MB，请拆成多个文件")
}

// writeImportError 翻批量导入特有的错误，其余交给 writeCatalogError
// （权限、幂等、SKU 编码撞车这些与商品写接口共用同一张表）。
func writeImportError(c *gin.Context, err error) {
	var fe *catalogimport.FileError
	switch {
	case errors.Is(err, catalogimport.ErrTooLarge):
		writeImportTooLarge(c)
	case errors.Is(err, catalogimport.ErrUnsupportedFormat):
		problem.Write(c, http.StatusUnsupportedMediaType, problem.TypeImportUnsupportedFmt,
			"只支持 xlsx 与 csv（老式 .xls 与加密的工作簿请另存为不加密的 .xlsx）")
	case errors.As(err, &fe):
		items := make([]api.FieldError, 0, len(fe.Issues))
		for _, msg := range fe.Issues {
			m := msg
			items = append(items, api.FieldError{Message: &m})
		}
		problem.WriteValue(c, http.StatusUnprocessableEntity, api.Problem{
			Type:   problem.TypeImportFileInvalid,
			Title:  "导入文件不成立：" + strings.Join(fe.Issues, "；"),
			Status: http.StatusUnprocessableEntity,
			Errors: &items,
		})
	case errors.Is(err, service.ErrImportNothingToImport):
		problem.Write(c, http.StatusUnprocessableEntity, problem.TypeImportNothingToImport,
			"没有一件商品可以导入：每件都有错误或没有选类目，请回到预检结果修改")
	default:
		writeCatalogError(c, err)
	}
}

// Preview 实现 POST /api/v1/admin/product-imports/preview。
func (h *ProductImportHandler) Preview(c *gin.Context) {
	data, _, ok := readImportFile(c)
	if !ok {
		return
	}
	pv, err := h.svc.Preview(c.Request.Context(), data)
	if err != nil {
		writeImportError(c, err)
		return
	}
	if pv.EngineErr != nil {
		// 推荐降级了。响应照常 200（category_engine = unavailable），日志要留：
		// 它是「引擎忙 / 挂了」在这条路上的唯一信号。
		_ = c.Error(pv.EngineErr)
	}
	c.JSON(http.StatusOK, apiImportPreview(pv))
}

// Commit 实现 POST /api/v1/admin/product-imports。
func (h *ProductImportHandler) Commit(c *gin.Context) {
	// 缺幂等键在读 5 MB 请求体之前就拒（service 里还有一道，那一道是权威）。
	if idemKeyOf(c) == "" {
		writeImportError(c, service.ErrIdempotencyKeyMissing)
		return
	}
	data, name, ok := readImportFile(c)
	if !ok {
		return
	}
	raw, ok := categoriesField(c)
	if !ok {
		return
	}
	var choices []api.ProductImportCategoryChoice
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &choices); err != nil {
			problem.Write(c, http.StatusUnprocessableEntity, problem.TypeInvalidRequest,
				"categories 不是合法的 JSON 数组（元素形如 {\"first_row\":2,\"category_id\":7}）")
			return
		}
	}
	in := make([]service.ImportCategoryChoice, 0, len(choices))
	for _, ch := range choices {
		in = append(in, service.ImportCategoryChoice{FirstRow: ch.FirstRow, CategoryID: ch.CategoryId})
	}
	res, replayed, err := h.svc.Commit(c.Request.Context(), data, name, in, idemKeyOf(c))
	if err != nil {
		writeImportError(c, err)
		return
	}
	markReplayed(c, replayed)
	c.JSON(http.StatusCreated, apiImportResult(res))
}

// categoriesField 取 multipart 里的 categories 那一项。
//
// **两种形状都要认**：契约给它写的是 `encoding: contentType: application/json`，
// 照契约生成的客户端（和浏览器里 `form.append("categories", new Blob([...], {type:
// "application/json"}))`）会把它发成一个**带文件名的 part**，而 net/http 把带文件名的
// part 放进 MultipartForm.File、不放进 FormValue。只读 FormValue 的话，这些客户端
// 选的类目会被静默丢掉 —— 后台界面上实测过：推荐的类目全部变成「没有选类目」，
// 而请求是 201。curl 的 `-F categories='[...]'` 则是普通字段。
func categoriesField(c *gin.Context) (string, bool) {
	if v := strings.TrimSpace(c.Request.FormValue("categories")); v != "" {
		return v, true
	}
	if c.Request.MultipartForm == nil {
		return "", true
	}
	files := c.Request.MultipartForm.File["categories"]
	if len(files) == 0 {
		return "", true
	}
	f, err := files[0].Open()
	if err != nil {
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "读取 categories 失败")
		return "", false
	}
	defer func() { _ = f.Close() }()
	// 2000 件商品 × 每条几十字节，1 MB 绰绰有余；再大就是请求本身有问题。
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		_ = c.Error(err)
		problem.Write(c, http.StatusInternalServerError, problem.TypeInternal, "读取 categories 失败")
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// ---------------------------------------------------------------------------
// 领域类型 → 契约类型
// ---------------------------------------------------------------------------

func apiImportIssues(in []catalogimport.Issue) []api.ProductImportIssue {
	out := make([]api.ProductImportIssue, 0, len(in))
	for _, i := range in {
		it := api.ProductImportIssue{Code: i.Code, Message: i.Message}
		if i.Column != "" {
			col := i.Column
			it.Column = &col
		}
		out = append(out, it)
	}
	return out
}

func apiImportPreview(pv service.ImportPreview) api.ProductImportPreview {
	p := pv.Parsed
	out := api.ProductImportPreview{
		FileSha256:     p.SHA256,
		Format:         api.ProductImportFormat(p.Format),
		TotalRows:      len(p.Rows),
		Notices:        append([]string{}, p.Notices...),
		CategoryEngine: api.ProductImportPreviewCategoryEngine(pv.Engine),
		Products:       make([]api.ProductImportProduct, 0, len(pv.Products)),
		Rows:           make([]api.ProductImportRow, 0, len(p.Rows)),
	}
	out.CategoryGate.MinScore = float32(pv.Gate.MinScore)
	out.CategoryGate.MinMargin = float32(pv.Gate.MinMargin)
	if pv.Previous != nil {
		out.PreviousImport = &struct {
			CreatedAt time.Time `json:"created_at"`
			ImportId  int64     `json:"import_id"`
		}{CreatedAt: pv.Previous.CreatedAt, ImportId: pv.Previous.ID}
	}
	firstLine := map[int]int{}
	for _, g := range p.Groups {
		for _, i := range g.Rows {
			firstLine[p.Rows[i].Line] = g.FirstLine
		}
	}
	for _, r := range p.Rows {
		row := api.ProductImportRow{
			Row:        r.Line,
			FirstRow:   firstLine[r.Line],
			SkuCode:    r.SKUCode,
			SpecValues: map[string]string{},
			Errors:     apiImportIssues(r.Errors),
			Warnings:   apiImportIssues(r.Warnings),
			Violations: []api.FieldError{},
		}
		for k, v := range r.SpecValues {
			row.SpecValues[k] = v
		}
		row.Title = optStr(r.Title)
		row.Subtitle = optStr(r.Subtitle)
		row.Category = optStr(r.Category)
		row.Description = optStr(r.Description)
		if r.PriceCents != nil {
			m := api.Money(*r.PriceCents)
			row.PriceCents = &m
		}
		if r.Stock != nil {
			n := int(*r.Stock)
			row.Stock = &n
		}
		if r.WeightGram != nil {
			n := int(*r.WeightGram)
			row.WeightGram = &n
		}
		if len(r.ImageURLs) > 0 {
			urls := append([]string{}, r.ImageURLs...)
			row.ImageUrls = &urls
		}
		for _, v := range pv.Violations[r.Line] {
			field, msg := v.Field, v.Message()
			offset, length := v.Offset, v.Length
			row.Violations = append(row.Violations, api.FieldError{
				Field: &field, Message: &msg, Offset: &offset, Length: &length,
			})
		}
		if len(r.Errors) > 0 {
			out.ErrorRows++
		}
		out.Rows = append(out.Rows, row)
	}
	for _, prod := range pv.Products {
		d := api.ProductImportCategoryDecision{
			Status:     api.ProductImportCategoryDecisionStatus(prod.Category.Status),
			CategoryId: prod.Category.CategoryID,
			PathName:   optStr(prod.Category.PathName),
			Candidates: make([]api.ProductImportCategoryCandidate, 0, len(prod.Category.Candidates)),
		}
		for _, cand := range prod.Category.Candidates {
			d.Candidates = append(d.Candidates, api.ProductImportCategoryCandidate{
				CategoryId: cand.ID, PathName: cand.PathName, Score: float32(cand.Score),
			})
		}
		out.Products = append(out.Products, api.ProductImportProduct{
			FirstRow: prod.FirstLine, Rows: append([]int{}, prod.Lines...), Title: prod.Title,
			Importable: prod.Importable, Category: d,
		})
	}
	return out
}

func apiImportResult(r service.ImportResult) api.ProductImportResult {
	out := api.ProductImportResult{
		ImportId:        r.ImportID,
		FileSha256:      r.FileSHA256,
		AlreadyImported: r.AlreadyImported,
		TotalRows:       r.TotalRows,
		CreatedProducts: r.CreatedProducts,
		CreatedSkus:     r.CreatedSKUs,
		FailedRows:      r.FailedRows,
		CreatedAt:       r.CreatedAt,
		Products:        make([]api.ProductImportOutcome, 0, len(r.Products)),
	}
	for _, o := range r.Products {
		item := api.ProductImportOutcome{
			FirstRow:   o.FirstRow,
			Rows:       append([]int{}, o.Rows...),
			Title:      o.Title,
			Status:     api.ProductImportOutcomeStatus(o.Status),
			ProductId:  o.ProductID,
			CategoryId: o.CategoryID,
		}
		if o.Status == service.OutcomeCreated {
			n := o.SKUCount
			item.SkuCount = &n
		}
		if len(o.Reasons) > 0 {
			rs := append([]string{}, o.Reasons...)
			item.Reasons = &rs
		}
		if len(o.ImageURLs) > 0 {
			urls := append([]string{}, o.ImageURLs...)
			item.ImageUrls = &urls
		}
		out.Products = append(out.Products, item)
	}
	sort.SliceStable(out.Products, func(i, j int) bool { return out.Products[i].FirstRow < out.Products[j].FirstRow })
	return out
}
