package catalogimport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPriceCents 是任何一个单价（SKU 价、大区 / 门店价、活动特价）的上限：一亿元。
// 导入、后台录入、数据库约束（00142）三处同一个数。它同时保证金额求和不会溢出 int64：
// 一亿元 × 单行 999 件 × 50 行 ≈ 5×10¹⁴ 分，离 int64 上限（约 9.2×10¹⁸）很远。
// 2026-09-28 破坏性测试：后台录入 int64 最大值作价格，结算求和溢出成负数、接口 500，单买一件能下出天价订单。
const MaxPriceCents int64 = 100_000_000_00

// Issue 是一行里的一条问题。Code 是给程序认的（契约 ProductImportIssue.code
// 列出了全部取值），Message 是给人看的。
type Issue struct {
	Column  string // 表头名；与某一列无关时为空
	Code    string
	Message string
}

// Issue 的 Code。契约里逐个列出，改名就是改契约。
const (
	CodeRequired         = "required"
	CodeTooLong          = "too_long"
	CodeInvalidPrice     = "invalid_price"
	CodeInvalidInteger   = "invalid_integer"
	CodeFormulaCell      = "formula_cell"
	CodeDateCell         = "date_cell"
	CodeErrorCell        = "error_cell"
	CodeNumericSKUCode   = "numeric_sku_code"
	CodeMergedCell       = "merged_cell"
	CodeDuplicateSKUCode = "duplicate_sku_code"
	CodeSKUCodeExists    = "sku_code_exists"
	CodeSpecInvalid      = "spec_invalid"
	CodeSpecMismatch     = "spec_mismatch"
	CodeSpecDuplicate    = "spec_duplicate"
	CodeGroupConflict    = "group_conflict"
	CodeGroupBlocked     = "group_blocked"
	CodeImageURLInvalid  = "image_url_invalid"
	CodePriceZero        = "price_zero"
	CodeNotContiguous    = "not_contiguous"
	CodeCategoryUnknown  = "category_unmatched"
)

// Row 是校验之后的一行（一个 SKU）。数值字段为 nil 表示那一格有错（Errors 里有原因）
// 或者可空而没填。
type Row struct {
	Line        int
	Group       int // Parsed.Groups 的下标
	Title       string
	Subtitle    string
	Category    string
	Description string
	SpecNames   []string
	SpecValues  map[string]string
	SKUCode     string
	PriceCents  *int64
	Stock       *int32
	WeightGram  *int32
	ImageURLs   []string

	Errors   []Issue
	Warnings []Issue
}

func (r *Row) errf(col ColumnKey, code, format string, args ...any) {
	r.Errors = append(r.Errors, Issue{Column: headerOf(col), Code: code, Message: fmt.Sprintf(format, args...)})
}

func (r *Row) warnf(col ColumnKey, code, format string, args ...any) {
	r.Warnings = append(r.Warnings, Issue{Column: headerOf(col), Code: code, Message: fmt.Sprintf(format, args...)})
}

// AddError 给这一行追加一条错误（service 用它挂「编码已存在」这类要查库才知道的错）。
func (r *Row) AddError(col ColumnKey, code, message string) {
	r.Errors = append(r.Errors, Issue{Column: headerOf(col), Code: code, Message: message})
}

// AddWarning 同上，追加提示。
func (r *Row) AddWarning(col ColumnKey, code, message string) {
	r.Warnings = append(r.Warnings, Issue{Column: headerOf(col), Code: code, Message: message})
}

func headerOf(col ColumnKey) string {
	if col == "" {
		return ""
	}
	return ColumnByKey(col).Header
}

// Group 是一件商品：同一个标题的若干行。
type Group struct {
	FirstLine   int
	Rows        []int // Parsed.Rows 的下标，按文件顺序
	Title       string
	Subtitle    string
	Category    string
	Description string
	// 三个商品级字段各自取自哪一行（合规命中要标在那一行上）。0 表示没填。
	SubtitleLine, CategoryLine, DescriptionLine int
	SpecNames                                   []string
	ImageURLs                                   []string
}

// Parsed 是一份文件的完整校验结果。
type Parsed struct {
	Format  Format
	SHA256  string // 文件字节的 sha256（十六进制），「同一份文件」的判据
	Rows    []Row
	Groups  []Group
	Notices []string
}

// GroupOK 说这件商品能不能导入：它的每一行都没有错误。
//
// **一行错、整件商品不导**，而不是只导对的那几个规格：规格是一件商品的组成部分，
// 导进一半的规格会得到一件「只有红色、没有蓝色」的商品，而商家只会在买家问起时
// 才发现 —— 比整件没进来更难察觉。
func (p *Parsed) GroupOK(g int) bool {
	for _, i := range p.Groups[g].Rows {
		if len(p.Rows[i].Errors) > 0 {
			return false
		}
	}
	return true
}

// LineRow 按行号找行。
func (p *Parsed) LineRow(line int) *Row {
	for i := range p.Rows {
		if p.Rows[i].Line == line {
			return &p.Rows[i]
		}
	}
	return nil
}

// FinalizeGroups 给「被同组别的行拖累」的行补一条提示。service 在挂完查库才知道
// 的错误之后要再调一次，所以单独导出；重复调用是安全的（先清掉旧的那条）。
func (p *Parsed) FinalizeGroups() {
	for g := range p.Groups {
		bad := 0
		for _, i := range p.Groups[g].Rows {
			if len(p.Rows[i].Errors) > 0 {
				bad = p.Rows[i].Line
				break
			}
		}
		for _, i := range p.Groups[g].Rows {
			r := &p.Rows[i]
			kept := r.Warnings[:0]
			for _, w := range r.Warnings {
				if w.Code != CodeGroupBlocked {
					kept = append(kept, w)
				}
			}
			r.Warnings = kept
			if bad != 0 && len(r.Errors) == 0 {
				r.warnf("", CodeGroupBlocked,
					"同一商品的第 %d 行有错误，整件商品（含这一行）都不会导入", bad)
			}
		}
	}
}

// Parse 读文件并逐行校验。返回的错误只有三种：ErrTooLarge、ErrUnsupportedFormat、
// *FileError —— 行级的问题一律进 Row.Errors / Row.Warnings，不返回错误，
// 因为预检要把**全部**问题一次列出来，而不是改一个报一个。
func Parse(data []byte) (*Parsed, error) {
	sheet, err := Read(data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	p := &Parsed{Format: sheet.Format, SHA256: hex.EncodeToString(sum[:]), Notices: sheet.Notices}

	colIdx, notices, err := mapHeader(sheet.Header)
	if err != nil {
		return nil, err
	}
	p.Notices = append(p.Notices, notices...)

	for _, sr := range sheet.Rows {
		p.Rows = append(p.Rows, parseRow(sr, colIdx))
	}
	p.group()
	p.FinalizeGroups()
	return p, nil
}

// mapHeader 把表头映射到列。缺必填列、同一列出现两次是文件级错误；
// 认不出的列忽略并提示（用户常在最后加一列「备注」）。
func mapHeader(header []string) (map[ColumnKey]int, []string, error) {
	idx := map[ColumnKey]int{}
	var notices []string
	var issues []string
	for i, h := range header {
		if strings.TrimSpace(h) == "" {
			continue
		}
		k, ok := headerKey(h)
		if !ok {
			notices = append(notices, fmt.Sprintf("列「%s」不认得，已忽略", strings.TrimSpace(h)))
			continue
		}
		if prev, dup := idx[k]; dup {
			issues = append(issues, fmt.Sprintf("「%s」这一列出现了两次（第 %d 列与第 %d 列）",
				ColumnByKey(k).Header, prev+1, i+1))
			continue
		}
		idx[k] = i
	}
	for _, c := range Columns {
		if _, ok := idx[c.Key]; !ok && c.Required {
			issues = append(issues, fmt.Sprintf("缺少必填列「%s」（表头要在第一个非空行）", c.Header))
		}
	}
	if len(issues) > 0 {
		return nil, nil, &FileError{Issues: issues}
	}
	return idx, notices, nil
}

// rowCtx 把「取某一列的格」与「按格的类型报错」收在一起。
type rowCtx struct {
	row   *Row
	cells []Cell
	idx   map[ColumnKey]int
}

func (c rowCtx) cell(k ColumnKey) (Cell, bool) {
	i, ok := c.idx[k]
	if !ok || i >= len(c.cells) {
		return Cell{}, false
	}
	return c.cells[i], true
}

// raw 取一格的值，并把不能当值用的格类型报成错误。第二个返回值为 false 表示
// 这一格不可用（已经记了错误）。数字格按 numberText 变成十进制串。
func (c rowCtx) raw(k ColumnKey) (string, CellKind, bool) {
	cell, ok := c.cell(k)
	if !ok {
		return "", KindText, true
	}
	if cell.Merged {
		c.row.warnf(k, CodeMergedCell, "这一格是合并单元格，已按合并区域左上角的值填入")
	}
	switch cell.Kind {
	case KindFormula:
		c.row.errf(k, CodeFormulaCell, "这一格是公式，请在 Excel 里复制后「粘贴为数值」")
		return "", cell.Kind, false
	case KindDate:
		c.row.errf(k, CodeDateCell,
			"这一格被 Excel 识别成了日期或时间（原值 %s），请把这一列设为文本后重新填写", strings.TrimSpace(cell.Value))
		return "", cell.Kind, false
	case KindError:
		c.row.errf(k, CodeErrorCell, "这一格是错误值 %s", strings.TrimSpace(cell.Value))
		return "", cell.Kind, false
	case KindBool:
		c.row.errf(k, CodeErrorCell, "这一格是逻辑值（TRUE / FALSE），不是这一列要的内容")
		return "", cell.Kind, false
	case KindNumber:
		s, ok := numberText(cell.Value)
		if !ok {
			c.row.errf(k, CodeInvalidInteger,
				"这一格是超出精度的数字（%s），Excel 已经无法原样保存它；请把这一列设为文本后重新填写",
				strings.TrimSpace(cell.Value))
			return "", cell.Kind, false
		}
		return s, cell.Kind, true
	default:
		return cleanText(cell.Value), cell.Kind, true
	}
}

// text 取一段文本，按字符数（不是字节数）限长；singleLine 时把换行折成空格。
func (c rowCtx) text(k ColumnKey, max int, singleLine bool) string {
	v, _, ok := c.raw(k)
	if !ok {
		return ""
	}
	if singleLine {
		v = strings.Join(strings.Fields(v), " ")
	}
	if n := utf8.RuneCountInString(v); max > 0 && n > max {
		c.row.errf(k, CodeTooLong, "有 %d 个字，上限 %d", n, max)
		return ""
	}
	return v
}

// cleanText 去首尾空白与零宽字符（从网页复制过来的文字常带着它们，
// 而一个带零宽空格的 SKU 编码与不带的看起来一模一样，却是两个编码）。
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\ufeff' {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

const (
	maxTitle       = 200
	maxSubtitle    = 200
	maxCategory    = 200
	maxDescription = 10000
	maxSKUCode     = 64
	maxSpecPart    = 50
	maxSpecDims    = 5
	maxImageURLs   = 20
	maxURLLen      = 2048
	// maxPriceCents 是一亿元（MaxPriceCents）。再往上几乎只可能是填错了单位（把分当元）或多敲了几个 0。
	maxPriceCents = MaxPriceCents
)

func parseRow(sr SheetRow, idx map[ColumnKey]int) Row {
	r := Row{Line: sr.Line, SpecValues: map[string]string{}}
	c := rowCtx{row: &r, cells: sr.Cells, idx: idx}

	r.Title = c.text(ColTitle, maxTitle, true)
	if r.Title == "" && !hasCode(r.Errors, ColTitle) {
		r.errf(ColTitle, CodeRequired, "商品标题必填")
	}
	r.Subtitle = c.text(ColSubtitle, maxSubtitle, true)
	r.Category = c.text(ColCategory, maxCategory, true)
	r.Description = c.text(ColDescription, maxDescription, false)

	// SKU 编码：数字格要特别提示 —— 前导 0 已经在 Excel 那一侧丢了，这里救不回来。
	if code, kind, ok := c.raw(ColSKUCode); ok {
		code = strings.Join(strings.Fields(code), "")
		switch {
		case code == "":
			r.errf(ColSKUCode, CodeRequired, "SKU 编码必填")
		case utf8.RuneCountInString(code) > maxSKUCode:
			r.errf(ColSKUCode, CodeTooLong, "有 %d 个字，上限 %d", utf8.RuneCountInString(code), maxSKUCode)
		default:
			r.SKUCode = code
			if kind == KindNumber {
				r.warnf(ColSKUCode, CodeNumericSKUCode,
					"SKU 编码被 Excel 存成了数字（%s）：如果原编码有前导 0，它已经丢了。建议把这一列设为文本", code)
			}
		}
	}

	if v, _, ok := c.raw(ColPrice); ok {
		if v == "" {
			r.errf(ColPrice, CodeRequired, "基准价必填")
		} else if cents, err := ParseYuan(v); err != nil {
			r.errf(ColPrice, CodeInvalidPrice, "%v", err)
		} else {
			r.PriceCents = &cents
			if cents == 0 {
				r.warnf(ColPrice, CodePriceZero, "基准价是 0 元，确认不是漏填？")
			}
		}
	}

	if v, _, ok := c.raw(ColStock); ok {
		if v == "" {
			r.errf(ColStock, CodeRequired, "库存必填")
		} else if n, err := parseCount(v); err != nil {
			r.errf(ColStock, CodeInvalidInteger, "库存%v", err)
		} else {
			r.Stock = &n
		}
	}

	if v, _, ok := c.raw(ColWeight); ok && v != "" {
		if n, err := parseCount(v); err != nil {
			r.errf(ColWeight, CodeInvalidInteger, "重量%v", err)
		} else {
			r.WeightGram = &n
		}
	}

	parseSpec(&r, c)
	parseImages(&r, c)
	return r
}

func hasCode(issues []Issue, col ColumnKey) bool {
	h := headerOf(col)
	for _, i := range issues {
		if i.Column == h {
			return true
		}
	}
	return false
}

var yuanRE = regexp.MustCompile(`^(\d{1,9})(?:\.(\d{1,2}))?$`)

// ParseYuan 把「元」的十进制串换成分。**全程字符串运算，一次浮点都没有**：
// 整数部分 × 100 + 小数部分补齐两位，都是整数。
//
// 接受：199、199.9、199.90、¥199、199元、「 199.90 」。
// 拒绝：负数、三位以上小数（199.999 是写错了，不是要四舍五入）、千分位逗号
// （1,299 在别的地区是 1.299，猜哪一种都可能错）、科学计数法、超过一亿元。
func ParseYuan(s string) (int64, error) {
	v := strings.TrimSpace(s)
	for _, p := range []string{"¥", "￥", "RMB", "CNY"} {
		v = strings.TrimPrefix(v, p)
	}
	v = strings.TrimSuffix(strings.TrimSpace(v), "元")
	v = strings.TrimSpace(v)
	if strings.Contains(v, ",") || strings.Contains(v, "，") {
		return 0, fmt.Errorf("「%s」带千分位逗号，请写成纯数字，如 1299.00", s)
	}
	m := yuanRE.FindStringSubmatch(v)
	if m == nil {
		if strings.Count(v, ".") == 1 {
			if parts := strings.SplitN(v, ".", 2); len(parts[1]) > 2 && yuanRE.MatchString(parts[0]) {
				return 0, fmt.Errorf("「%s」超过两位小数（价格精确到分）", s)
			}
		}
		return 0, fmt.Errorf("「%s」不是合法的金额（单位元，最多两位小数，不能为负）", s)
	}
	yuan, _ := strconv.ParseInt(m[1], 10, 64)
	frac := m[2]
	for len(frac) < 2 {
		frac += "0"
	}
	fen, _ := strconv.ParseInt(frac, 10, 64)
	cents := yuan*100 + fen
	if cents > maxPriceCents {
		return 0, fmt.Errorf("「%s」超过一亿元，请核对单位", s)
	}
	return cents, nil
}

var countRE = regexp.MustCompile(`^\d{1,10}$`)

// parseCount 读一个非负整数（库存、克重）。数字格已经被 numberText 变成最短十进制串，
// 所以「10」在这里就是「10」，「10.5」仍然是「10.5」并被拒。
func parseCount(s string) (int32, error) {
	v := strings.TrimSpace(s)
	if !countRE.MatchString(v) {
		return 0, fmt.Errorf("「%s」不是非负整数", s)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n > math.MaxInt32 {
		return 0, fmt.Errorf("「%s」太大了", s)
	}
	return int32(n), nil
}

// specSplit 按半角 / 全角分号或竖线切规格。不认逗号：规格值里本来就可能有逗号
// （「长袖,加绒」）。
func specSplit(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '；' || r == '|' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

func parseSpec(r *Row, c rowCtx) {
	namesRaw, _, ok1 := c.raw(ColSpecNames)
	valuesRaw, _, ok2 := c.raw(ColSpecValues)
	if !ok1 || !ok2 {
		return
	}
	if namesRaw == "" && valuesRaw == "" {
		return
	}
	names, values := specSplit(namesRaw), specSplit(valuesRaw)
	switch {
	case namesRaw == "":
		r.errf(ColSpecNames, CodeSpecInvalid, "填了规格值却没填规格名")
		return
	case valuesRaw == "":
		r.errf(ColSpecValues, CodeSpecInvalid, "填了规格名却没填规格值")
		return
	case len(names) != len(values):
		r.errf(ColSpecValues, CodeSpecInvalid, "规格名有 %d 个、规格值有 %d 个，要一一对应（用分号分隔）",
			len(names), len(values))
		return
	case len(names) > maxSpecDims:
		r.errf(ColSpecNames, CodeSpecInvalid, "规格维度最多 %d 个", maxSpecDims)
		return
	}
	seen := map[string]bool{}
	for i, n := range names {
		v := values[i]
		if n == "" || v == "" {
			r.errf(ColSpecNames, CodeSpecInvalid, "规格名或规格值里有空的一项（第 %d 个）", i+1)
			return
		}
		if utf8.RuneCountInString(n) > maxSpecPart || utf8.RuneCountInString(v) > maxSpecPart {
			r.errf(ColSpecNames, CodeTooLong, "单个规格名 / 规格值不超过 %d 字", maxSpecPart)
			return
		}
		if seen[n] {
			r.errf(ColSpecNames, CodeSpecInvalid, "规格名「%s」重复了", n)
			return
		}
		seen[n] = true
	}
	r.SpecNames = names
	for i, n := range names {
		r.SpecValues[n] = values[i]
	}
}

func parseImages(r *Row, c rowCtx) {
	raw, _, ok := c.raw(ColImageURL)
	if !ok || raw == "" {
		return
	}
	parts := strings.FieldsFunc(raw, func(ch rune) bool {
		return ch == ';' || ch == '；' || ch == '|' || unicode.IsSpace(ch)
	})
	for _, p := range parts {
		if len(r.ImageURLs) >= maxImageURLs {
			r.errf(ColImageURL, CodeImageURLInvalid, "一行最多 %d 个图片地址", maxImageURLs)
			return
		}
		if err := checkImageURL(p); err != nil {
			r.errf(ColImageURL, CodeImageURLInvalid, "「%s」%v", truncate(p, 80), err)
			return
		}
		r.ImageURLs = append(r.ImageURLs, p)
	}
}

// checkImageURL 只核**形状**：http / https、有主机名、不超长。
//
// 本期只记录不下载，所以这里不解析 DNS、不判内网地址 —— 那些判断属于「下载」
// 那一步，而下载这一步没有做（理由写在 service/product_import.go 的文件头）。
// 把 SSRF 防护写在一个不发请求的地方，只会让人以为它已经在保护着什么。
func checkImageURL(s string) error {
	if len(s) > maxURLLen {
		return fmt.Errorf("超过 %d 个字符", maxURLLen)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("不是合法的 URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只接受 http / https 地址")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("缺少主机名")
	}
	if u.User != nil {
		return fmt.Errorf("不能带用户名密码")
	}
	return nil
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}

// group 按标题把行合并成商品，并执行合并规则。
//
// 合并规则（契约 ProductImportPreview 的描述里写着同样几条）：
//
//  1. **同一个标题 = 同一件商品**，不要求相邻；不相邻时给一条提示（多半是排序乱了，
//     也可能真是两件不同的商品起了同一个名字 —— 那种情况要改标题）。
//  2. 副标题、类目、描述是商品级字段：以组内第一个非空值为准；后面的行可以留空，
//     填了就必须相同，否则那一行报错（不猜哪一个是对的）。
//  3. 组内多于一行时，每一行都必须有规格，且规格名集合相同；规格值组合不能重复。
//     只有一行的商品可以不填规格（单规格商品）。
//  4. SKU 编码在整个文件里唯一；重复的那一行（后出现的）报错。
func (p *Parsed) group() {
	byTitle := map[string]int{}
	lastRowOfGroup := map[int]int{} // 组 → 上一行在 Rows 里的下标
	skuFirst := map[string]int{}

	for i := range p.Rows {
		r := &p.Rows[i]
		if r.SKUCode != "" {
			if first, dup := skuFirst[r.SKUCode]; dup {
				r.errf(ColSKUCode, CodeDuplicateSKUCode, "SKU 编码「%s」与第 %d 行重复", r.SKUCode, first)
			} else {
				skuFirst[r.SKUCode] = r.Line
			}
		}
		if r.Title == "" {
			// 没有标题的行归不进任何商品：自成一组，好让它照样出现在结果里。
			p.Groups = append(p.Groups, Group{FirstLine: r.Line, Rows: []int{i}})
			r.Group = len(p.Groups) - 1
			continue
		}
		g, ok := byTitle[r.Title]
		if !ok {
			p.Groups = append(p.Groups, Group{FirstLine: r.Line, Title: r.Title})
			g = len(p.Groups) - 1
			byTitle[r.Title] = g
		} else if last := lastRowOfGroup[g]; last != i-1 {
			r.warnf(ColTitle, CodeNotContiguous,
				"与第 %d 行标题相同，已合并为同一件商品；如果它们其实是两件商品，请改成不同的标题",
				p.Groups[g].FirstLine)
		}
		lastRowOfGroup[g] = i
		r.Group = g
		p.Groups[g].Rows = append(p.Groups[g].Rows, i)
	}

	for g := range p.Groups {
		grp := &p.Groups[g]
		if grp.Title == "" {
			continue
		}
		mergeField := func(get func(*Row) string, col ColumnKey, dst *string, line *int) {
			for _, i := range grp.Rows {
				r := &p.Rows[i]
				v := get(r)
				if v == "" {
					continue
				}
				if *line == 0 {
					*dst, *line = v, r.Line
					continue
				}
				if v != *dst {
					r.errf(col, CodeGroupConflict, "同一商品的%s与第 %d 行不一致；后面的行留空或填相同内容",
						ColumnByKey(col).Header, *line)
				}
			}
		}
		mergeField(func(r *Row) string { return r.Subtitle }, ColSubtitle, &grp.Subtitle, &grp.SubtitleLine)
		mergeField(func(r *Row) string { return r.Category }, ColCategory, &grp.Category, &grp.CategoryLine)
		mergeField(func(r *Row) string { return r.Description }, ColDescription, &grp.Description, &grp.DescriptionLine)

		// 规格
		first := &p.Rows[grp.Rows[0]]
		grp.SpecNames = first.SpecNames
		if len(grp.Rows) > 1 {
			combos := map[string]int{}
			for _, i := range grp.Rows {
				r := &p.Rows[i]
				if hasCode(r.Errors, ColSpecNames) || hasCode(r.Errors, ColSpecValues) {
					continue
				}
				if len(r.SpecNames) == 0 {
					r.errf(ColSpecNames, CodeSpecMismatch,
						"这件商品有 %d 行（%d 个 SKU），每一行都要填规格名与规格值来区分", len(grp.Rows), len(grp.Rows))
					continue
				}
				if !sameSet(r.SpecNames, grp.SpecNames) {
					if len(grp.SpecNames) == 0 {
						grp.SpecNames = r.SpecNames
					} else {
						r.errf(ColSpecNames, CodeSpecMismatch, "规格名「%s」与第 %d 行的「%s」不一致",
							strings.Join(r.SpecNames, ";"), grp.FirstLine, strings.Join(grp.SpecNames, ";"))
						continue
					}
				}
				key := comboKey(grp.SpecNames, r.SpecValues)
				if prev, dup := combos[key]; dup {
					r.errf(ColSpecValues, CodeSpecDuplicate, "规格组合与第 %d 行重复", prev)
					continue
				}
				combos[key] = r.Line
			}
		}

		seen := map[string]bool{}
		for _, i := range grp.Rows {
			for _, u := range p.Rows[i].ImageURLs {
				if !seen[u] {
					seen[u] = true
					grp.ImageURLs = append(grp.ImageURLs, u)
				}
			}
		}
		if len(grp.ImageURLs) > maxImageURLs {
			grp.ImageURLs = grp.ImageURLs[:maxImageURLs]
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func comboKey(names []string, values map[string]string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(0)
		b.WriteString(values[n])
		b.WriteByte(0)
	}
	return b.String()
}
