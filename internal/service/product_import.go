package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/keel/keel/internal/catalogimport"
	"github.com/keel/keel/internal/inference"
	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/understanding"
)

// 商品批量导入的业务层（契约 /admin/product-imports，三条）。
//
// ===========================================================================
// 一、预检不落库，确认时重传同一份文件
// ===========================================================================
//
// 预检（Preview）只读：解析、逐行校验、查「哪些 SKU 编码店里已经有了」、跑违禁词、
// 算类目推荐，然后把结果还给界面。确认（Commit）时客户端把**同一份文件**再传一次，
// 连同每件商品选定的类目；服务端重新解析、重新校验，**不信任**客户端回传的预检结果。
//
// 另一条路是预检时把解析结果存一份、确认时只传一个 id。没走它，理由两条：
// 存下来的东西要有过期与清理（预检过却从没确认的那些由谁删）；而更要紧的是
// 预检与确认之间店里的状态会变（别人刚建了一个同编码的 SKU、刚停用了一个类目），
// 确认时照样得重新校验一遍 —— 那「存一份」就只省下了解析文件的那几十毫秒。
//
// ===========================================================================
// 二、确认导入是一个事务，不走 jobs 队列
// ===========================================================================
//
// 规模由 catalogimport 的上限钉死（2000 行），一次确认大约是 8000 条语句、几秒量级，
// 一个事务装得下。一个事务换来的是最简单的语义：
//
//   - **幂等不需要善后**：抢幂等键、占「这份文件导入过」、建商品、写回执全在同一个
//     事务里，任何一步失败整批回滚，客户端原样重试即可（与 admin_idempotency.go
//     的 idempotentTx 同一个形状）。
//   - **没有「导了一半」**：走队列的话要处理「第 800 件失败了，前 799 件怎么办」，
//     要有进度表、要能续传、界面要轮询。
//
// 代价：确认请求是同步的，2000 行要等几秒；事务期间新建的商品对别的请求不可见。
// 上限往上调之前，先把这里改成入队（jobs 表已经在了）。
//
// 事务里**不调推理引擎**：确认的结果只取决于文件与客户端给的类目选择。推荐是
// 预检那一步的事，界面把推荐的类目原样带回来 —— 否则确认时引擎一抖，同一份
// 预检结果会导出不同的类目，而且事务要为一次网络调用持锁。
//
// ===========================================================================
// 三、幂等两层
// ===========================================================================
//
//  1. Idempotency-Key：同一次提交的重发（网断了、点了两次）。请求哈希认的是
//     「文件 sha256 + 类目选择」。
//  2. 文件 sha256（uk_product_import_batches_sha）：换了钥匙的「同一份文件又导一遍」。
//     事务第一句就占位，撞上了返回那一次的回执（AlreadyImported = true），
//     一件商品都不建。改过的文件是另一份文件 —— 那时已经建过的 SKU 编码在
//     校验里报 sku_code_exists，于是「修好几行再导一次」只会补上没导进去的那几件。
//
// ===========================================================================
// 四、图片 URL：只记录，不下载
// ===========================================================================
//
// 任务书给了两条路：只记录，或者带严格限制地下载。选前者，理由：
//
//   - 服务端替用户去抓一个任意 URL 是教科书式的 SSRF 入口。挡住它要的不只是
//     「仅 http/https」：要解析 DNS 后判内网段（含 IPv6、IPv4 映射地址）、防 DNS 重绑定
//     （判的地址与连的地址必须是同一个）、限制重定向且每一跳都重判、限大小限类型限时长。
//     每一条漏掉都不会报错，只会在某天把元数据服务的凭据当成一张「图片」存进来。
//   - 2000 行里每行几张图，同步下载会把确认请求拖到分钟级，就得走队列（第二节）。
//   - 抓回来的图要走 uploads 的归属与引用计数，这条链路今天只从 POST /admin/uploads 进。
//
// 所以本期把地址原样留在回执里（ProductImportOutcome.image_urls），界面列出来，
// 商家在商品页上传。要做自动抓取，另起一轮：白名单域名 + 上面那张清单。

// ErrImportNothingToImport：确认导入时一件可导入的商品都没有（契约 422）。
var ErrImportNothingToImport = errors.New("没有一件商品可以导入")

// scopeAdminProductImport 是确认导入在 idempotency_keys 里的作用域。
const scopeAdminProductImport = "admin.products.import"

// defaultRecommendBudget 是预检里类目推荐的总预算。2000 件商品约 32 批嵌入，
// 独占引擎时几秒；超过预算就降级为「需手选」，预检照常返回 —— 推荐不能阻塞导入。
const defaultRecommendBudget = 15 * time.Second

// ProductImportRepository 是本服务要的仓储能力，与 AdminCatalogRepository 相同：
// 只有租户作用域。
type ProductImportRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

// ProductImportService 实现批量导入。
type ProductImportService struct {
	repo        ProductImportRepository
	recommender *understanding.CategoryRecommender
	hasEngine   bool

	// Compliance 是违禁词检查器（与发布时同一个）。预检里它只是提示，不阻断。
	Compliance ComplianceChecker
	// Gate 是类目「自动选中」的两道门。零值时用 understanding.DefaultCategoryGate。
	Gate understanding.CategoryGate
	// RecommendBudget <= 0 时用 defaultRecommendBudget。给测试调短。
	RecommendBudget time.Duration

	// inv 是库存服务（微服务拆分阶段 1a）：导入建出来的 SKU 的初始库存经它建行。
	inv inventory.Service
}

// NewProductImportService 建一个。emb 为 nil 是正常形态（没配 KEEL_EMBED_ENDPOINT
// 的部署）：那时类目推荐一律降级为「需手选」，导入本身不受影响。
//
// 传进来的必须是**真的 nil 接口**，不是装着 nil 指针的接口 —— 理由写在
// app.Run 里 searchEmbedder 那一段。
func NewProductImportService(r ProductImportRepository, emb inference.Embedder, inv inventory.Service) *ProductImportService {
	return &ProductImportService{
		repo:        r,
		inv:         inv,
		recommender: understanding.NewCategoryRecommender(emb),
		hasEngine:   emb != nil,
		Compliance:  understanding.ComplianceCheck{},
	}
}

func (s *ProductImportService) gate() understanding.CategoryGate {
	if s.Gate == (understanding.CategoryGate{}) {
		return understanding.DefaultCategoryGate
	}
	return s.Gate
}

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

// ImportTemplate 是一份模板文件。
type ImportTemplate struct {
	Data        []byte
	ContentType string
	FileName    string // ASCII 文件名（Content-Disposition 的回退值）
	FileNameUTF string // 中文文件名（filename*）
}

// Template 实现 GET /admin/product-imports/template。
func (s *ProductImportService) Template(ctx context.Context, format string) (ImportTemplate, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return ImportTemplate{}, err
	}
	switch catalogimport.Format(format) {
	case "", catalogimport.FormatXLSX:
		b, err := catalogimport.TemplateXLSX()
		if err != nil {
			return ImportTemplate{}, err
		}
		return ImportTemplate{Data: b,
			ContentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			FileName:    "product-import-template.xlsx", FileNameUTF: "商品导入模板.xlsx"}, nil
	case catalogimport.FormatCSV:
		return ImportTemplate{Data: catalogimport.TemplateCSV(), ContentType: "text/csv; charset=utf-8",
			FileName: "product-import-template.csv", FileNameUTF: "商品导入模板.csv"}, nil
	default:
		return ImportTemplate{}, fmt.Errorf("%w: format 只能是 xlsx 或 csv", ErrCatalogBadRequest)
	}
}

// ---------------------------------------------------------------------------
// 类目：名字匹配与叶子清单
// ---------------------------------------------------------------------------

// categoryIndex 是一家店启用中的类目，按「能不能被导入选中」整理过。
type categoryIndex struct {
	active   map[int64]string // 启用中（自己与全部祖先都启用、未软删）的类目 → 路径名
	segments map[int64][]string
	leaves   []understanding.CategoryOption
}

// pathSep 是路径名的连接符，与契约 ProductImportCategoryCandidate.path_name 一致。
const pathSep = " > "

func buildCategoryIndex(cats []repository.AdminCategory) categoryIndex {
	byID := make(map[int64]repository.AdminCategory, len(cats))
	for _, c := range cats {
		byID[c.ID] = c
	}
	idx := categoryIndex{active: map[int64]string{}, segments: map[int64][]string{}}
	var chain func(id int64, depth int) ([]string, bool)
	chain = func(id int64, depth int) ([]string, bool) {
		c, ok := byID[id]
		// depth 上限只是防御：类目树有判环（MoveCategory），但一个坏数据不该让这里死循环。
		if !ok || c.Status != 1 || c.DeletedAt != nil || depth > 32 {
			return nil, false
		}
		if c.ParentID == nil {
			return []string{c.Name}, true
		}
		up, ok := chain(*c.ParentID, depth+1)
		if !ok {
			return nil, false
		}
		return append(append([]string{}, up...), c.Name), true
	}
	hasActiveChild := map[int64]bool{}
	for _, c := range cats {
		segs, ok := chain(c.ID, 0)
		if !ok {
			continue
		}
		idx.active[c.ID] = strings.Join(segs, pathSep)
		idx.segments[c.ID] = segs
		if c.ParentID != nil {
			hasActiveChild[*c.ParentID] = true
		}
	}
	// 叶子按路径名排序：推荐结果里分数相同时顺序要确定，否则同一份文件两次预检
	// 可能给出不同的 Top-1。
	for id, name := range idx.active {
		if !hasActiveChild[id] {
			idx.leaves = append(idx.leaves, understanding.CategoryOption{ID: id, PathName: name})
		}
	}
	sort.Slice(idx.leaves, func(i, j int) bool {
		if idx.leaves[i].PathName != idx.leaves[j].PathName {
			return idx.leaves[i].PathName < idx.leaves[j].PathName
		}
		return idx.leaves[i].ID < idx.leaves[j].ID
	})
	return idx
}

func splitCategoryPath(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '>' || r == '/' || r == '＞' || r == '／' || r == '\\' || r == '》'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// match 把类目列的原文对到一个启用中的类目。
//
// 认三种写法：完整路径（「服装 > 女装 > 连衣裙」）、路径的末尾几段（「女装 > 连衣裙」）、
// 单个名字（「连衣裙」）。**对上多个就不算对上**（两个一级类目下各有一个「外套」时，
// 只写「外套」猜哪个都可能错），交给推荐与人工。返回 ambiguous 让提示说清楚原因。
func (idx categoryIndex) match(text string) (id int64, ambiguous bool) {
	want := splitCategoryPath(text)
	if len(want) == 0 {
		return 0, false
	}
	var hits []int64
	for cid, segs := range idx.segments {
		if len(segs) < len(want) {
			continue
		}
		tail := segs[len(segs)-len(want):]
		same := true
		for i := range want {
			if !strings.EqualFold(tail[i], want[i]) {
				same = false
				break
			}
		}
		if same {
			hits = append(hits, cid)
		}
	}
	// 完整路径优先：「女装」既是一级类目的全名，又可能是某个二级类目的末段。
	if len(hits) > 1 {
		var full []int64
		for _, cid := range hits {
			if len(idx.segments[cid]) == len(want) {
				full = append(full, cid)
			}
		}
		if len(full) == 1 {
			return full[0], false
		}
	}
	switch len(hits) {
	case 0:
		return 0, false
	case 1:
		return hits[0], false
	default:
		return 0, true
	}
}

// ---------------------------------------------------------------------------
// 预检
// ---------------------------------------------------------------------------

// 类目决定的四种状态，与契约 ProductImportCategoryDecision.status 逐值一致。
const (
	CategoryMatched     = "matched"
	CategoryRecommended = "recommended"
	CategoryNeedsReview = "needs_review"
	CategoryUnavailable = "unavailable"
)

// 类目推荐这一次跑成了没有，与契约 ProductImportPreview.category_engine 逐值一致。
const (
	EngineOK            = "ok"
	EngineUnavailable   = "unavailable"
	EngineNotConfigured = "not_configured"
)

// CategoryDecision 是一件商品的类目怎么定。
type CategoryDecision struct {
	Status     string
	CategoryID *int64
	PathName   string
	Candidates []understanding.CategoryCandidate
}

// ImportProduct 是预检结果里的一件商品。
type ImportProduct struct {
	Group      int // Parsed.Groups 的下标
	FirstLine  int
	Lines      []int
	Title      string
	Importable bool
	Category   CategoryDecision
}

// ImportPreview 是预检的全部结果。
type ImportPreview struct {
	Parsed   *catalogimport.Parsed
	Products []ImportProduct
	// Violations 按行号挂违禁词命中（挂在提供那段文字的那一行上）。
	Violations map[int][]understanding.Violation
	Engine     string
	// EngineErr 是推荐失败的原因（引擎不可用 / 超时）。只给日志，不进响应。
	EngineErr error
	Gate      understanding.CategoryGate
	Previous  *repository.ProductImport
}

// Preview 实现 POST /admin/product-imports/preview。不写库。
func (s *ProductImportService) Preview(ctx context.Context, data []byte) (ImportPreview, error) {
	if _, err := requireMerchantWide(ctx); err != nil {
		return ImportPreview{}, err
	}
	parsed, err := catalogimport.Parse(data)
	if err != nil {
		return ImportPreview{}, err
	}

	var cats []repository.AdminCategory
	var existing []string
	var prev *repository.ProductImport
	// 三次读在同一个事务里（同一个快照）。这个事务**只读**，而且在调推理引擎
	// 之前就结束了：不为一次网络调用持有数据库连接。
	err = s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var e error
		if cats, e = tx.AdminListCategories(ctx); e != nil {
			return e
		}
		if existing, e = tx.ExistingSKUCodes(ctx, skuCodes(parsed)); e != nil {
			return e
		}
		p, e := tx.FindProductImportBySHA(ctx, parsed.SHA256)
		switch {
		case e == nil:
			prev = &p
		case !errors.Is(e, repository.ErrProductImportNotFound):
			return e
		}
		return nil
	})
	if err != nil {
		return ImportPreview{}, err
	}
	markExistingSKUCodes(parsed, existing)
	idx := buildCategoryIndex(cats)
	decisions := matchCategories(parsed, idx)
	parsed.FinalizeGroups()

	out := ImportPreview{Parsed: parsed, Gate: s.gate(), Previous: prev}
	out.Violations = s.checkCompliance(ctx, parsed)
	out.Engine, out.EngineErr = s.recommend(ctx, parsed, idx, decisions)

	for g := range parsed.Groups {
		grp := parsed.Groups[g]
		lines := make([]int, 0, len(grp.Rows))
		for _, i := range grp.Rows {
			lines = append(lines, parsed.Rows[i].Line)
		}
		out.Products = append(out.Products, ImportProduct{
			Group: g, FirstLine: grp.FirstLine, Lines: lines, Title: grp.Title,
			Importable: parsed.GroupOK(g), Category: decisions[g],
		})
	}
	return out, nil
}

func skuCodes(p *catalogimport.Parsed) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range p.Rows {
		if r.SKUCode != "" && !seen[r.SKUCode] {
			seen[r.SKUCode] = true
			out = append(out, r.SKUCode)
		}
	}
	return out
}

func markExistingSKUCodes(p *catalogimport.Parsed, existing []string) {
	if len(existing) == 0 {
		return
	}
	taken := map[string]bool{}
	for _, c := range existing {
		taken[c] = true
	}
	for i := range p.Rows {
		r := &p.Rows[i]
		if taken[r.SKUCode] {
			r.AddError(catalogimport.ColSKUCode, catalogimport.CodeSKUCodeExists,
				fmt.Sprintf("SKU 编码「%s」在店里已经存在（可能是之前导入过，或者被一个已删除的 SKU 占着）", r.SKUCode))
		}
	}
}

// matchCategories 给每件商品按类目列定一个初步决定：对上了就是 matched，
// 否则先记成 unavailable（等推荐那一步改写）。对不上的在提供类目那一行留提示。
func matchCategories(p *catalogimport.Parsed, idx categoryIndex) []CategoryDecision {
	out := make([]CategoryDecision, len(p.Groups))
	for g := range p.Groups {
		grp := p.Groups[g]
		out[g] = CategoryDecision{Status: CategoryUnavailable}
		if grp.Category == "" {
			continue
		}
		id, ambiguous := idx.match(grp.Category)
		if id != 0 {
			id := id
			out[g] = CategoryDecision{Status: CategoryMatched, CategoryID: &id, PathName: idx.active[id]}
			continue
		}
		if r := p.LineRow(grp.CategoryLine); r != nil {
			msg := fmt.Sprintf("类目「%s」对不上店里启用中的类目，已按标题推荐", grp.Category)
			if ambiguous {
				msg = fmt.Sprintf("类目「%s」对得上不止一个类目，请写完整路径（如「服装 > 女装 > 外套」）；已按标题推荐", grp.Category)
			}
			r.AddWarning(catalogimport.ColCategory, catalogimport.CodeCategoryUnknown, msg)
		}
	}
	return out
}

// recommend 给没对上类目的商品算推荐，原地改写 decisions。
// 引擎没配 / 不可用 / 超时一律降级为 unavailable，**不返回错误**：推荐不能阻塞导入。
func (s *ProductImportService) recommend(ctx context.Context, p *catalogimport.Parsed,
	idx categoryIndex, decisions []CategoryDecision) (string, error) {

	var groups []int
	var queries []string
	for g := range p.Groups {
		if decisions[g].Status == CategoryMatched || p.Groups[g].Title == "" {
			continue
		}
		groups = append(groups, g)
		queries = append(queries, understanding.CategoryQueryText(p.Groups[g].Title, p.Groups[g].Subtitle))
	}
	if !s.hasEngine {
		return EngineNotConfigured, nil
	}
	if len(queries) == 0 || len(idx.leaves) == 0 {
		return EngineOK, nil
	}
	budget := s.RecommendBudget
	if budget <= 0 {
		budget = defaultRecommendBudget
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res, err := s.recommender.Recommend(rctx, queries, idx.leaves)
	if err != nil {
		if errors.Is(err, understanding.ErrNoEmbedder) {
			return EngineNotConfigured, nil
		}
		return EngineUnavailable, err
	}
	gate := s.gate()
	for i, g := range groups {
		d := CategoryDecision{Status: CategoryNeedsReview, Candidates: res[i]}
		if gate.Confident(res[i]) {
			id := res[i][0].ID
			d.Status, d.CategoryID, d.PathName = CategoryRecommended, &id, res[i][0].PathName
		}
		decisions[g] = d
	}
	return EngineOK, nil
}

// checkCompliance 对每件商品的标题 / 副标题 / 描述跑违禁词，命中挂到提供那段文字的行上。
//
// 预检里它**只是提示**：导入的是草稿，而草稿不做合规拦截（compliance.go 文件头：
// 拦草稿只是在商家写到一半时打断人）。上架那一步照旧会拦。检查器出错时就不给提示 ——
// 这里没有「宁可误拒」的理由，真正的闸门在上架。
func (s *ProductImportService) checkCompliance(ctx context.Context, p *catalogimport.Parsed) map[int][]understanding.Violation {
	out := map[int][]understanding.Violation{}
	if s.Compliance == nil {
		return out
	}
	for _, grp := range p.Groups {
		if grp.Title == "" {
			continue
		}
		vs, err := s.Compliance.Check(ctx, understanding.ProductInput{
			Title: grp.Title, Subtitle: grp.Subtitle, Description: grp.Description,
		})
		if err != nil {
			continue
		}
		for _, v := range vs {
			line := grp.FirstLine
			switch v.Field {
			case understanding.FieldSubtitle:
				line = grp.SubtitleLine
			case understanding.FieldDescription:
				line = grp.DescriptionLine
			}
			out[line] = append(out[line], v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 确认导入
// ---------------------------------------------------------------------------

// ImportCategoryChoice 是客户端给一件商品选定的类目。
type ImportCategoryChoice struct {
	FirstRow   int   `json:"first_row"`
	CategoryID int64 `json:"category_id"`
}

// ImportOutcome 是回执里的一件商品。字段带 json tag：整个回执原样存进
// product_import_batches.result，「同一份文件再确认一次」时原样读回来。
type ImportOutcome struct {
	FirstRow   int      `json:"first_row"`
	Rows       []int    `json:"rows"`
	Title      string   `json:"title"`
	Status     string   `json:"status"` // created / failed
	ProductID  *int64   `json:"product_id,omitempty"`
	SKUCount   int      `json:"sku_count,omitempty"`
	CategoryID *int64   `json:"category_id,omitempty"`
	Reasons    []string `json:"reasons,omitempty"`
	ImageURLs  []string `json:"image_urls,omitempty"`
}

// 回执里一件商品的两种结局，与契约 ProductImportOutcome.status 一致。
const (
	OutcomeCreated = "created"
	OutcomeFailed  = "failed"
)

// ImportResult 是一次确认导入的回执。
type ImportResult struct {
	ImportID        int64           `json:"import_id"`
	FileSHA256      string          `json:"file_sha256"`
	AlreadyImported bool            `json:"already_imported"`
	TotalRows       int             `json:"total_rows"`
	CreatedProducts int             `json:"created_products"`
	CreatedSKUs     int             `json:"created_skus"`
	FailedRows      int             `json:"failed_rows"`
	Products        []ImportOutcome `json:"products"`
	CreatedAt       time.Time       `json:"created_at"`
}

// importFingerprint 是确认导入的 request_hash 素材：文件 + 类目选择（按首行排序）。
type importFingerprint struct {
	SHA256  string                 `json:"sha256"`
	Choices []ImportCategoryChoice `json:"choices"`
}

// maxImportFileName 是记进库里的文件名长度上限（只用于展示）。
const maxImportFileName = 200

// Commit 实现 POST /admin/product-imports。
//
// 返回的第二个值为 true 表示这是一次 Idempotency-Key 重放。「同一份文件早就导过」
// 不是重放，是 ImportResult.AlreadyImported。
func (s *ProductImportService) Commit(ctx context.Context, data []byte, fileName string,
	choices []ImportCategoryChoice, idemKey string) (ImportResult, bool, error) {

	id, err := requireMerchantWide(ctx)
	if err != nil {
		return ImportResult{}, false, err
	}
	// 缺钥匙在解析文件之前就拒：一个注定被拒的请求不该先把 5 MB 解一遍。
	if idemKey == "" {
		return ImportResult{}, false, ErrIdempotencyKeyMissing
	}
	chosen := map[int]int64{}
	for _, c := range choices {
		if c.FirstRow < 2 || c.CategoryID <= 0 {
			return ImportResult{}, false, fmt.Errorf("%w: categories 里的 first_row 从 2 起、category_id 为正整数", ErrCatalogBadRequest)
		}
		if _, dup := chosen[c.FirstRow]; dup {
			return ImportResult{}, false, fmt.Errorf("%w: categories 里 first_row=%d 出现了两次", ErrCatalogBadRequest, c.FirstRow)
		}
		chosen[c.FirstRow] = c.CategoryID
	}
	parsed, err := catalogimport.Parse(data)
	if err != nil {
		return ImportResult{}, false, err
	}
	sorted := append([]ImportCategoryChoice(nil), choices...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FirstRow < sorted[j].FirstRow })
	hash, err := adminRequestHash(nil, importFingerprint{SHA256: parsed.SHA256, Choices: sorted})
	if err != nil {
		return ImportResult{}, false, err
	}
	if n := utf8.RuneCountInString(fileName); n > maxImportFileName {
		fileName = string([]rune(fileName)[:maxImportFileName])
	}

	res, replayed, err := idempotentTx(ctx, s.repo, repository.StaffSubject(id.StaffID), scopeAdminProductImport,
		idemKey, hash, archivedCreated, func(tx repository.Tx) (ImportResult, error) {
			return s.commitInTx(ctx, tx, parsed, fileName, chosen, id.StaffID)
		})
	if err != nil {
		return ImportResult{}, false, err
	}
	// 初始库存：导入事务提交之后建（阶段 1a，与单个建 SKU 同一个理由，见 initSKUStock）。
	// 重放与「这份文件早就导过」两支也走这里 —— 建行可以放心重复，于是「重试」就是「补建」。
	if err := s.initImportStock(ctx, res, parsed); err != nil {
		return ImportResult{}, false, err
	}
	return res, replayed, nil
}

// initImportStock 给这次导入建出来的 SKU 建第一行库存（默认门店，初始量取文件里的库存列）。
//
// 不在导入事务里记下「建了哪些 SKU」，而是事后按回执里的 product_id 重新列一遍：
// 回执会被存档、会被重放，而它里面没有 SKU id；重新列一遍让首次提交、Idempotency-Key 重放、
// 同一份文件换一把钥匙再交（AlreadyImported）三条路走同一段代码。只认文件里出现过的货号 ——
// 商家之后手工加的规格不归这里管。已经有库存行的 SKU（上一次其实建成了，或者之后被改过）
// 由库存服务那一侧的守卫跳过，不会被初始值覆盖。
//
// 失败时（拆分形态下库存服务不可用）回错误：商品与 SKU 已经导进去了，用同一个
// Idempotency-Key 重试即可补上初始库存。
func (s *ProductImportService) initImportStock(ctx context.Context, res ImportResult,
	parsed *catalogimport.Parsed) error {
	stockByCode := make(map[string]int32, len(parsed.Rows))
	for _, r := range parsed.Rows {
		if r.Stock != nil {
			stockByCode[r.SKUCode] = *r.Stock
		}
	}
	var productIDs []int64
	for _, o := range res.Products {
		if o.Status == OutcomeCreated && o.ProductID != nil {
			productIDs = append(productIDs, *o.ProductID)
		}
	}
	if len(productIDs) == 0 {
		return nil
	}
	var rows []inventory.InitRow
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		storeID, ok, err := tx.DefaultStoreForNewSKU(ctx)
		if err != nil || !ok {
			// 没有默认门店：一行都不建，不是失败（缺行 ≡ 可售 0）。
			return err
		}
		for _, pid := range productIDs {
			skus, err := tx.AdminListProductSKUs(ctx, pid)
			if err != nil {
				return err
			}
			for _, sk := range skus {
				qty, ok := stockByCode[sk.SKUCode]
				if !ok {
					continue
				}
				rows = append(rows, inventory.InitRow{SKUID: sk.ID, StoreID: storeID, Available: qty})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.inv.InitSKUs(ctx, rows); err != nil {
		return fmt.Errorf("商品已导入，初始库存还没写进库存服务（用同一个 Idempotency-Key 重试即可补上）: %w", err)
	}
	return nil
}

func (s *ProductImportService) commitInTx(ctx context.Context, tx repository.Tx,
	parsed *catalogimport.Parsed, fileName string, chosen map[int]int64, staffID int64) (ImportResult, error) {

	importID, createdAt, claimed, err := tx.ClaimProductImport(ctx, repository.NewProductImport{
		FileSHA256: parsed.SHA256, FileName: fileName, FileFormat: string(parsed.Format),
		StaffID: staffID, TotalRows: int32(len(parsed.Rows)),
	})
	if err != nil {
		return ImportResult{}, err
	}
	if !claimed {
		prev, err := tx.FindProductImportBySHA(ctx, parsed.SHA256)
		if err != nil {
			return ImportResult{}, fmt.Errorf("同一份文件已经导入过，但读不到那一次的记录: %w", err)
		}
		var out ImportResult
		if err := json.Unmarshal(prev.Result, &out); err != nil {
			return ImportResult{}, fmt.Errorf("导入记录 %d 的回执解不开: %w", prev.ID, err)
		}
		out.AlreadyImported = true
		return out, nil
	}

	cats, err := tx.AdminListCategories(ctx)
	if err != nil {
		return ImportResult{}, err
	}
	existing, err := tx.ExistingSKUCodes(ctx, skuCodes(parsed))
	if err != nil {
		return ImportResult{}, err
	}
	markExistingSKUCodes(parsed, existing)
	idx := buildCategoryIndex(cats)
	decisions := matchCategories(parsed, idx)
	parsed.FinalizeGroups()

	res := ImportResult{
		ImportID: importID, FileSHA256: parsed.SHA256, TotalRows: len(parsed.Rows), CreatedAt: createdAt,
		Products: []ImportOutcome{},
	}
	type plan struct {
		g          int
		categoryID int64
	}
	var plans []plan
	for g := range parsed.Groups {
		grp := parsed.Groups[g]
		o := ImportOutcome{FirstRow: grp.FirstLine, Title: grp.Title, ImageURLs: grp.ImageURLs}
		for _, i := range grp.Rows {
			o.Rows = append(o.Rows, parsed.Rows[i].Line)
		}
		var categoryID int64
		if c, ok := chosen[grp.FirstLine]; ok {
			categoryID = c
		} else if decisions[g].Status == CategoryMatched {
			categoryID = *decisions[g].CategoryID
		}
		switch {
		case !parsed.GroupOK(g):
			o.Status, o.Reasons = OutcomeFailed, groupReasons(parsed, g)
		case categoryID == 0:
			o.Status, o.Reasons = OutcomeFailed, []string{"没有选类目（类目列为空或对不上，需要在预检结果里选一个）"}
		case idx.active[categoryID] == "":
			o.Status, o.Reasons = OutcomeFailed,
				[]string{fmt.Sprintf("选的类目 %d 不存在或已停用", categoryID)}
		default:
			cid := categoryID
			o.CategoryID = &cid
			plans = append(plans, plan{g: g, categoryID: cid})
		}
		res.Products = append(res.Products, o)
	}
	if len(plans) == 0 {
		return ImportResult{}, ErrImportNothingToImport
	}

	outcomeOf := map[int]*ImportOutcome{}
	for i := range res.Products {
		outcomeOf[res.Products[i].FirstRow] = &res.Products[i]
	}
	for _, pl := range plans {
		grp := parsed.Groups[pl.g]
		p, err := tx.CreateProduct(ctx, repository.NewProduct{
			CategoryID:  pl.categoryID,
			Title:       grp.Title,
			Subtitle:    optString(grp.Subtitle),
			Description: optString(grp.Description),
		})
		if err != nil {
			return ImportResult{}, fmt.Errorf("第 %d 行的商品没建成: %w", grp.FirstLine, err)
		}
		for _, i := range grp.Rows {
			r := parsed.Rows[i]
			spec, err := encodeSpecValues(r.SpecValues)
			if err != nil {
				return ImportResult{}, err
			}
			var weight int32
			if r.WeightGram != nil {
				weight = *r.WeightGram
			}
			if _, err := tx.CreateSKU(ctx, repository.NewSKU{
				ProductID:    p.ID,
				SKUCode:      r.SKUCode,
				SpecValues:   spec,
				PriceCents:   *r.PriceCents,
				WeightGram:   weight,
				Status:       1,
				AvailableQty: *r.Stock,
			}); err != nil {
				return ImportResult{}, fmt.Errorf("第 %d 行的 SKU 没建成: %w", r.Line, err)
			}
			res.CreatedSKUs++
		}
		o := outcomeOf[grp.FirstLine]
		o.Status, o.ProductID, o.SKUCount = OutcomeCreated, &p.ID, len(grp.Rows)
		res.CreatedProducts++
	}
	for _, o := range res.Products {
		if o.Status == OutcomeFailed {
			res.FailedRows += len(o.Rows)
		}
	}

	body, err := json.Marshal(res)
	if err != nil {
		return ImportResult{}, err
	}
	if err := tx.FinishProductImport(ctx, importID, repository.ProductImportTotals{
		CreatedProducts: int32(res.CreatedProducts), CreatedSKUs: int32(res.CreatedSKUs),
		FailedRows: int32(res.FailedRows),
	}, body); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// groupReasons 把一件商品各行的错误拼成回执里的原因，每条带行号。
func groupReasons(p *catalogimport.Parsed, g int) []string {
	var out []string
	for _, i := range p.Groups[g].Rows {
		r := p.Rows[i]
		for _, e := range r.Errors {
			col := ""
			if e.Column != "" {
				col = "「" + e.Column + "」"
			}
			out = append(out, fmt.Sprintf("第 %d 行%s：%s", r.Line, col, e.Message))
		}
	}
	return out
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
