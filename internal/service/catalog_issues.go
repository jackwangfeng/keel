package service

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/keel/keel/internal/inventory"
	"github.com/keel/keel/internal/repository"
	"github.com/keel/keel/internal/understanding"
)

// 上新整理：catalog_issues（MCP 只读工具），逐件商品体检。
// 设计：docs/superpowers/specs/2026-10-16-ai-ops-onboarding-restock-coupon-design.md 第一节。
//
// 每条问题都带「码 + 可复现的依据」，AI 员工按码决定能不能自己修（提改文案提案），
// 修不了的（缺图、价格异常、疑似重复）写进简报交给人。判据全部写死在这个文件里，
// 店长能逐条手算核对 —— 与成绩单那一套同一个要求。

const (
	CatalogIssueTitleNoise     = "title_noise"
	CatalogIssueTitleTooLong   = "title_too_long"
	CatalogIssueNoSubtitle     = "no_subtitle"
	CatalogIssueForbiddenWords = "forbidden_words"
	CatalogIssueUncategorized  = "uncategorized"
	CatalogIssueNoImage        = "no_image"
	CatalogIssuePriceOutlier   = "price_outlier"
	CatalogIssueDuplicateTitle = "duplicate_title"
	CatalogIssueZeroStock      = "zero_stock"

	// 手机列表里标题两行放得下的上限（按字计，不按字节）。
	catalogTitleMaxRunes = 30
	// 同类目至少这么多个 SKU 才判价格异常：样本太少时「中位数」没有意义。
	catalogPriceMinSamples = 5
	// 偏离中位数的倍数：≥ 5 倍或 ≤ 1/5 才算异常 —— 多打一个 0、少打一个 0 都落在这里，
	// 正常的大小包装（5kg 对 500g 也就 10 倍以内且通常不在一个类目里）大多不会误报。
	catalogPriceOutlierRatio = 5.0

	catalogDefaultLimit = 200
	catalogMaxLimit     = 500
)

// 标题里的装饰符号与营销词。符号逐个列出而不是按 Unicode 类别判：
// 「·」「/」「-」在商品名里是正常的分隔，不能当噪音。
var (
	catalogNoiseSymbols = regexp.MustCompile(`[【】\[\]★☆♥❤♡✔✨🔥!！~～#＃]`)
	catalogNoiseWords   = regexp.MustCompile(`特价|包邮|爆款|热卖|秒杀|清仓|限时|促销|正品保证|厂家直销|亏本|跳楼价|抢购|买一送一|超值|新品上市`)
	// 归一化判重时额外去掉的：规格与数量（2.5kg、500ml、×24、30 枚、12 包 ……）。
	catalogSpecWords = regexp.MustCompile(`\d+(\.\d+)?\s*(kg|g|ml|l|斤|克|千克|升|毫升|枚|包|袋|盒|瓶|罐|只|个|片|抽|装|连包|件)*|[×x*]\s*\d+`)
	// 视为「还没分类」的类目名。
	catalogUncategorizedNames = map[string]bool{"未分类": true, "默认分类": true, "": true}
)

// CatalogIssue 是一件商品上的一个问题。
type CatalogIssue struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// CatalogIssueItem 是一件有问题的商品。
type CatalogIssueItem struct {
	ProductID    int64          `json:"product_id"`
	Title        string         `json:"title"`
	Subtitle     string         `json:"subtitle,omitempty"`
	Status       int16          `json:"status"`
	CategoryID   int64          `json:"category_id"`
	CategoryName string         `json:"category_name"`
	Issues       []CatalogIssue `json:"issues"`
}

// CatalogIssuesResult 是 catalog_issues 的输出。
type CatalogIssuesResult struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Scanned     int                `json:"scanned"`
	Summary     map[string]int     `json:"summary"` // 各问题码的件数（一件商品同一个码只算一次）
	Items       []CatalogIssueItem `json:"items"`   // 只列有问题的商品，按问题数从多到少
	Truncated   bool               `json:"truncated"`
	// Categories 是店里启用中的叶子类目（带名字路径）：修 uncategorized 时从这里挑 category_id。
	// 2026-10-16 实跑时 AI 员工没有别的工具列类目，只好用 query_sql 去查。
	Categories []repository.CatalogCategory `json:"categories"`
}

// CatalogIssuesQuery 是入参。Statuses 为空时看草稿与上架（0、1）。
type CatalogIssuesQuery struct {
	Statuses []int16
	Limit    int
}

type CatalogIssuesRepository interface {
	WithTenant(ctx context.Context, fn func(repository.Tx) error) error
}

type CatalogIssuesService struct {
	repo       CatalogIssuesRepository
	inv        inventory.Service
	compliance understanding.ComplianceCheck
	now        func() time.Time
}

func NewCatalogIssuesService(r CatalogIssuesRepository, inv inventory.Service) *CatalogIssuesService {
	return &CatalogIssuesService{repo: r, inv: inv, compliance: understanding.ComplianceCheck{}, now: time.Now}
}

// Scan 实现 catalog_issues。商品是全店的：门店管理员也能看（只读，不涉及哪家店的数）。
func (s *CatalogIssuesService) Scan(ctx context.Context, q CatalogIssuesQuery) (CatalogIssuesResult, error) {
	if _, err := requireStaff(ctx); err != nil {
		return CatalogIssuesResult{}, err
	}
	statuses := q.Statuses
	if len(statuses) == 0 {
		statuses = []int16{0, 1}
	}
	for _, st := range statuses {
		if st < 0 || st > 2 {
			return CatalogIssuesResult{}, fmt.Errorf("%w: status 只能是 0 草稿 / 1 上架 / 2 下架", ErrAdminListBadRequest)
		}
	}
	limit := q.Limit
	if limit == 0 {
		limit = catalogDefaultLimit
	}
	if limit < 1 || limit > catalogMaxLimit {
		return CatalogIssuesResult{}, fmt.Errorf("%w: limit 取 1–%d", ErrAdminListBadRequest, catalogMaxLimit)
	}
	var products []repository.CatalogProduct
	var prices []repository.CatalogSKUPrice
	var cats []repository.CatalogCategory
	err := s.repo.WithTenant(ctx, func(tx repository.Tx) error {
		var err error
		if cats, err = tx.CatalogLeafCategories(ctx); err != nil {
			return err
		}
		// 多取一件，用来判断是不是截断了
		if products, err = tx.CatalogIssueProducts(ctx, statuses, int32(limit+1)); err != nil {
			return err
		}
		prices, err = tx.CatalogSKUPrices(ctx)
		return err
	})
	if err != nil {
		return CatalogIssuesResult{}, err
	}
	truncated := len(products) > limit
	if truncated {
		products = products[:limit]
	}
	scanned := map[int64]bool{}
	for _, p := range products {
		scanned[p.ID] = true
	}
	var skuIDs []int64
	for _, sp := range prices {
		if scanned[sp.ProductID] {
			skuIDs = append(skuIDs, sp.SKUID)
		}
	}
	totals := map[int64]inventory.Total{}
	if len(skuIDs) > 0 {
		if totals, err = s.inv.SKUTotals(ctx, skuIDs); err != nil {
			return CatalogIssuesResult{}, err
		}
	}
	items := make([]CatalogIssueItem, 0, len(products))
	for _, p := range products {
		v, err := s.compliance.Check(ctx, understanding.ProductInput{ProductID: p.ID, Title: p.Title,
			Subtitle: p.Subtitle, Description: p.Description})
		if err != nil {
			return CatalogIssuesResult{}, err
		}
		items = append(items, CatalogIssueItem{ProductID: p.ID, Title: p.Title, Subtitle: p.Subtitle, Status: p.Status,
			CategoryID: p.CategoryID, CategoryName: p.CategoryName, Issues: productTextIssues(p, v)})
	}
	addCrossProductIssues(items, products, prices, totals)
	out := CatalogIssuesResult{GeneratedAt: s.now().UTC(), Scanned: len(products), Summary: map[string]int{},
		Items: []CatalogIssueItem{}, Truncated: truncated, Categories: cats}
	for _, it := range items {
		if len(it.Issues) == 0 {
			continue
		}
		seen := map[string]bool{}
		for _, is := range it.Issues {
			if !seen[is.Code] {
				seen[is.Code] = true
				out.Summary[is.Code]++
			}
		}
		out.Items = append(out.Items, it)
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return len(out.Items[i].Issues) > len(out.Items[j].Issues) })
	return out, nil
}

// productTextIssues 是只看这一件商品自己就能判的问题。
func productTextIssues(p repository.CatalogProduct, violations []understanding.Violation) []CatalogIssue {
	var out []CatalogIssue
	if m := catalogNoiseFound(p.Title); len(m) > 0 {
		out = append(out, CatalogIssue{CatalogIssueTitleNoise, "标题里有装饰符号或营销词：" + strings.Join(m, " ")})
	}
	if n := utf8.RuneCountInString(p.Title); n > catalogTitleMaxRunes {
		out = append(out, CatalogIssue{CatalogIssueTitleTooLong, fmt.Sprintf("标题 %d 字，超过 %d 字", n, catalogTitleMaxRunes)})
	}
	if strings.TrimSpace(p.Subtitle) == "" {
		out = append(out, CatalogIssue{CatalogIssueNoSubtitle, "没有副标题"})
	}
	if len(violations) > 0 {
		terms := make([]string, 0, len(violations))
		seen := map[string]bool{}
		for _, v := range violations {
			k := v.Field + ":" + v.Term
			if !seen[k] {
				seen[k] = true
				terms = append(terms, fmt.Sprintf("%s「%s」", catalogFieldName(v.Field), v.Term))
			}
		}
		out = append(out, CatalogIssue{CatalogIssueForbiddenWords, "命中广告法违禁词：" + strings.Join(terms, "、")})
	}
	if catalogUncategorizedNames[strings.TrimSpace(p.CategoryName)] {
		name := p.CategoryName
		if name == "" {
			name = "（类目已删除）"
		}
		out = append(out, CatalogIssue{CatalogIssueUncategorized, "还在「" + name + "」下"})
	}
	if p.ImageCount == 0 {
		out = append(out, CatalogIssue{CatalogIssueNoImage, "没有商品图"})
	}
	return out
}

func catalogFieldName(f string) string {
	switch f {
	case understanding.FieldTitle:
		return "标题"
	case understanding.FieldSubtitle:
		return "副标题"
	default:
		return "详情"
	}
}

// catalogNoiseFound 返回标题里出现的噪音（去重，按出现顺序）。
func catalogNoiseFound(title string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{catalogNoiseSymbols, catalogNoiseWords} {
		for _, m := range re.FindAllString(title, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// catalogTitleKey 是判重用的归一化标题：去噪音、去规格数量、只留汉字与字母并转小写。
func catalogTitleKey(title string) string {
	t := strings.ToLower(title)
	t = catalogNoiseSymbols.ReplaceAllString(t, "")
	t = catalogNoiseWords.ReplaceAllString(t, "")
	t = catalogSpecWords.ReplaceAllString(t, "")
	var b strings.Builder
	for _, r := range t {
		if unicode.Is(unicode.Han, r) || unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// addCrossProductIssues 是要和别的商品比才判得出的问题：疑似重复、价格异常、全部 SKU 无货。
// 价格的参照是全店同类目的 SKU（prices 是全店的），不只是这一批被体检的。
func addCrossProductIssues(items []CatalogIssueItem, products []repository.CatalogProduct,
	prices []repository.CatalogSKUPrice, totals map[int64]inventory.Total) {
	idx := make(map[int64]int, len(items))
	for i, it := range items {
		idx[it.ProductID] = i
	}
	// 疑似重复：只在这一批里互相比（被体检的通常就是新导入的那一批）
	byKey := map[string][]int{}
	for i, p := range products {
		if k := catalogTitleKey(p.Title); utf8.RuneCountInString(k) >= 2 {
			byKey[k] = append(byKey[k], i)
		}
	}
	for _, group := range byKey {
		if len(group) < 2 {
			continue
		}
		for _, i := range group {
			var others []string
			for _, j := range group {
				if j != i {
					others = append(others, fmt.Sprintf("#%d「%s」", products[j].ID, products[j].Title))
				}
			}
			items[i].Issues = append(items[i].Issues, CatalogIssue{CatalogIssueDuplicateTitle,
				"与 " + strings.Join(others, "、") + " 去掉规格和修饰后同名"})
		}
	}
	// 价格异常：同类目 SKU 价格的中位数
	byCat := map[int64][]int64{}
	for _, sp := range prices {
		byCat[sp.CategoryID] = append(byCat[sp.CategoryID], sp.PriceCents)
	}
	median := map[int64]int64{}
	for c, ps := range byCat {
		if len(ps) < catalogPriceMinSamples {
			continue
		}
		sorted := append([]int64(nil), ps...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		median[c] = sorted[len(sorted)/2]
	}
	skusOf := map[int64][]repository.CatalogSKUPrice{}
	for _, sp := range prices {
		if _, ok := idx[sp.ProductID]; ok {
			skusOf[sp.ProductID] = append(skusOf[sp.ProductID], sp)
		}
	}
	for pid, skus := range skusOf {
		i := idx[pid]
		var odd []string
		allZero := true
		for _, sp := range skus {
			if m, ok := median[sp.CategoryID]; ok && m > 0 && sp.PriceCents > 0 {
				r := float64(sp.PriceCents) / float64(m)
				if r >= catalogPriceOutlierRatio || r <= 1/catalogPriceOutlierRatio {
					odd = append(odd, fmt.Sprintf("SKU #%d ¥%s 是同类目中位数 ¥%s 的 %.1f 倍",
						sp.SKUID, yuanString(sp.PriceCents), yuanString(m), r))
				}
			}
			if totals[sp.SKUID].Available > 0 {
				allZero = false
			}
		}
		if len(odd) > 0 {
			items[i].Issues = append(items[i].Issues, CatalogIssue{CatalogIssuePriceOutlier, strings.Join(odd, "；")})
		}
		if allZero {
			items[i].Issues = append(items[i].Issues, CatalogIssue{CatalogIssueZeroStock, "全部 SKU 可售都是 0"})
		}
	}
}

func yuanString(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
