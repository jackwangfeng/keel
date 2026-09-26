// Package catalogimport 是商品批量导入的**纯计算**那一半：读文件（xlsx / csv）、
// 逐行校验、按商品标题合并多规格行、生成模板。它不碰数据库、不调推理引擎 ——
// 那两件事在 internal/service/product_import.go 里，这里只回答「这份文件写了什么、
// 哪一行哪一格不成立」。
//
// ===========================================================================
// 为什么单独一个包
// ===========================================================================
//
// 预检（不落库）与确认导入（落库）必须对同一份文件得出**同一份**解析结果：
// 预检说「第 7 行价格不对」，确认时却把第 7 行建了出来，那是一次谁也解释不了的
// 事故。两条路径共用这一个包的同一个入口（Parse），差别只在后面接的是
// 「把结果回给界面」还是「把结果写进库」。
//
// 这个包刻意没有任何 I/O 之外的依赖（没有 repository、没有 inference），
// 所以它的全部分支都能在没有数据库的单元测试里跑到 —— 表格解析恰恰是
// 边界情况最多的那一块（公式、合并单元格、被 Excel 转成日期的编码、
// GBK 编码的 csv、浮点表示的价格），不该让它们只在要起 Postgres 的测试里才被看见。
//
// ===========================================================================
// 上限：5 MB / 2000 行
// ===========================================================================
//
// 两个数都是**同步请求**的预算推出来的，不是随手拍的：
//
//   - 预检要对每个商品的「标题 + 副标题」算一次向量（类目推荐），2000 个商品
//     是 32 批 × 64 条；infero 在 A4000 上批 64 约 0.6 秒，独占时约 20 秒以内，
//     和别的请求共用 GPU 时更慢。再往上，一次 HTTP 请求等不起。
//   - 确认导入是**一个事务**（见 service/product_import.go 的文件头）：2000 个 SKU
//     大约是 8000 条语句，本机实测在几秒量级。事务再长，持锁时间与失败重来的
//     代价一起涨。
//   - 5 MB 对 2000 行绰绰有余（一行 11 列的纯文本 xlsx 约 100 字节压缩后），
//     它挡的是「把整本带图片的工作簿传上来」。xlsx 是 zip，5 MB 的压缩包可以
//     解出几个 GB（zip 炸弹），所以另有一道**解压**上限（MaxUnzipBytes），
//     交给 excelize 在解压时执行。
//
// 真要导入更多，拆成多个文件；把这两个数调大之前，先把确认导入改成走 jobs 队列。
package catalogimport

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	// MaxFileBytes 是单个文件的上限（契约 413）。
	MaxFileBytes int64 = 5 << 20

	// MaxRows 是数据行（不含表头、不含整行空白）的上限（契约 422）。
	MaxRows = 2000

	// MaxUnzipBytes 是 xlsx 解压后的总大小上限。一个 2000 行的正常工作簿解压后
	// 不到 2 MB；64 MB 留足了带格式、带说明页的余量，又挡得住 zip 炸弹。
	MaxUnzipBytes int64 = 64 << 20

	// maxUnzipXMLBytes 是单个工作表 XML 留在内存里的上限，超出的部分 excelize
	// 会落到临时文件。必须 ≤ MaxUnzipBytes（excelize 的约束）。
	maxUnzipXMLBytes int64 = 16 << 20
)

// Format 是文件格式。
type Format string

const (
	FormatXLSX Format = "xlsx"
	FormatCSV  Format = "csv"
)

// CellKind 是一格内容在表格里的「原始类型」。
//
// 校验要它，因为同一串字符在不同类型下意思不同：SKU 编码列里一个**数字**格
// 意味着 Excel 已经吃掉了前导 0；价格列里一个**日期**格意味着「3.5」被 Excel
// 认成了三月五日。只拿格式化之后的字符串，这两件事都看不出来。
type CellKind int

const (
	KindText CellKind = iota
	KindNumber
	KindDate
	KindBool
	KindFormula
	KindError
)

// Cell 是一格。
type Cell struct {
	// Value 是**原始值**：数字是它在文件里存的样子（不套用单元格格式），
	// 文本原样。
	Value string
	Kind  CellKind
	// Merged 为 true 表示这一格本身是空的，值是从合并区域左上角那一格填下来的。
	Merged bool
}

// SheetRow 是一行数据。Line 是它在文件里的行号（表头是第 1 行），
// 界面上「第几行出错」说的就是它 —— 与用户在 Excel 左边看到的行号一致。
type SheetRow struct {
	Line  int
	Cells []Cell // 与 Sheet.Header 对齐，短行补空格
}

// Sheet 是读出来的一张表：表头 + 数据行，已跳过整行空白。
type Sheet struct {
	Format Format
	Header []string
	Rows   []SheetRow
	// Notices 是文件级的提示（不阻断），比如「读的是名为『商品』的工作表」。
	Notices []string
}

// FileError 是整份文件不成立（不是某一行的问题）：格式不认得、表头缺列、
// 行数超限、工作簿加了密码……。契约里它是 422 import-file-invalid，
// Issues 逐条列出原因。
type FileError struct {
	Issues []string
}

func (e *FileError) Error() string { return "导入文件不成立：" + strings.Join(e.Issues, "；") }

func fileErr(format string, args ...any) *FileError {
	return &FileError{Issues: []string{fmt.Sprintf(format, args...)}}
}

// ErrUnsupportedFormat 是文件既不是 xlsx 也不是 csv（契约 415）。
var ErrUnsupportedFormat = errors.New("只支持 xlsx 与 csv")

// ErrTooLarge 是文件超过 MaxFileBytes（契约 413）。
var ErrTooLarge = errors.New("文件超过 5 MB")

// SheetName 是模板里放数据的那张工作表的名字。读 xlsx 时优先找它，
// 找不到才读第一张 —— 模板里还有一张「填写说明」，它不能被当成数据读进来。
const SheetName = "商品"

// Sniff 按**内容**判断格式，不按文件名：文件名是客户端随便写的，
// 而把一个 xlsx 当 csv 读会得到一堆乱码行，报错指向「第 1 行缺标题」，与真因无关。
func Sniff(data []byte) (Format, error) {
	switch {
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return FormatXLSX, nil
	case bytes.HasPrefix(data, []byte("\xD0\xCF\x11\xE0")):
		// OLE2：老的 .xls（或加了密码的 xlsx —— 加密的 xlsx 也包在 OLE2 里）。
		return "", fmt.Errorf("%w：这是老式 .xls 或加了密码的工作簿，请在 Excel 里另存为 .xlsx（不加密码）", ErrUnsupportedFormat)
	case bytes.IndexByte(data, 0) >= 0:
		return "", fmt.Errorf("%w：文件里有二进制内容，不是表格", ErrUnsupportedFormat)
	default:
		return FormatCSV, nil
	}
}

// Read 读一份文件。data 的长度由调用方先按 MaxFileBytes 截过（handler 那道
// MaxBytesReader），这里再核一次，免得哪天换了调用方就把上限丢了。
func Read(data []byte) (*Sheet, error) {
	if int64(len(data)) > MaxFileBytes {
		return nil, ErrTooLarge
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fileErr("文件是空的")
	}
	f, err := Sniff(data)
	if err != nil {
		return nil, err
	}
	var s *Sheet
	if f == FormatXLSX {
		s, err = readXLSX(data)
	} else {
		s, err = readCSV(data)
	}
	if err != nil {
		return nil, err
	}
	if len(s.Rows) == 0 {
		return nil, fileErr("除了表头之外一行数据都没有")
	}
	if len(s.Rows) > MaxRows {
		return nil, fileErr("数据行有 %d 行，单次上限 %d 行：请拆成多个文件分批导入", len(s.Rows), MaxRows)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// csv
// ---------------------------------------------------------------------------

// readCSV 读 csv。
//
// **编码是 csv 最大的坑**：中文 Windows 上的 Excel「另存为 CSV」默认写 GBK，
// 而不是 UTF-8。按 UTF-8 读 GBK 得到的是满屏替换字符，表头一列都对不上，
// 用户看到的报错是「缺少商品标题列」—— 他明明填了。
//
// 所以：先剥 UTF-8 BOM（Excel 另存为「CSV UTF-8」时会带）；剥完仍不是合法 UTF-8
// 就按 GB18030（GBK 的超集）解一次。两者都解不出来才报错。
// 这是**猜**，不是协商 —— 一份碰巧也是合法 UTF-8 的 GBK 文件会被当成 UTF-8，
// 但那种字节序列在真实中文文本里几乎不存在，而反方向（UTF-8 被当 GBK）不会发生，
// 因为我们先试 UTF-8。
func readCSV(data []byte) (*Sheet, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	notices := []string(nil)
	if !utf8.Valid(data) {
		dec, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
		if err != nil || !utf8.Valid(dec) {
			return nil, fileErr("csv 既不是 UTF-8 也不是 GBK 编码，请用 Excel「另存为 → CSV UTF-8」重新保存")
		}
		data = dec
		notices = append(notices, "csv 不是 UTF-8 编码，已按 GBK（GB18030）读取")
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // 行长不一致不是错：Excel 会省略行尾的空格
	r.LazyQuotes = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, fileErr("csv 解析失败：%v", err)
	}
	if len(records) == 0 {
		return nil, fileErr("文件是空的")
	}
	s := &Sheet{Format: FormatCSV, Header: records[0], Notices: notices}
	for i, rec := range records[1:] {
		cells := make([]Cell, len(rec))
		for j, v := range rec {
			cells[j] = Cell{Value: v, Kind: KindText}
		}
		s.appendRow(i+2, cells)
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// xlsx
// ---------------------------------------------------------------------------

// readXLSX 读 xlsx。三个 Excel 特有的坑在这里各有一道处理：
//
//   - **公式**：取出来的是缓存值还是公式本身，取决于文件是谁存的（Excel 会存缓存值，
//     很多程序生成的文件不会）。同一份模板在两个人手里读出两种结果不可接受，
//     所以公式格一律标成 KindFormula，由校验报错「请粘贴为数值」—— 明确拒绝，不猜。
//   - **合并单元格**：只有左上角那一格有值。常见用法是把同一商品的标题跨几行合并，
//     所以把值填进合并区域的每一格（Cell.Merged 标出来，校验给一条提示）；
//     但表头行不许合并 —— 那意味着列与表头对不上。
//   - **日期**：读原始值（RawCellValue），并按单元格的数字格式判断它是不是日期。
//     「1-2」「3.5」这类 SKU 编码或价格被 Excel 自动转成日期后，存的是一个序列号
//     （44928），套用格式后显示成 2023/1/2 —— 两种读法都不是用户填的东西，
//     所以标成 KindDate，由校验报错。
func readXLSX(data []byte) (*Sheet, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		RawCellValue:      true,
		UnzipSizeLimit:    MaxUnzipBytes,
		UnzipXMLSizeLimit: maxUnzipXMLBytes,
	})
	if err != nil {
		if errors.Is(err, excelize.ErrWorkbookPassword) {
			return nil, fileErr("工作簿加了密码，请去掉密码后再传")
		}
		// 解压超限 excelize 报的是一句普通错误（没有 sentinel），原样带上。
		return nil, fileErr("xlsx 读取失败（文件损坏，或解压后超过 %d MB）：%v", MaxUnzipBytes>>20, err)
	}
	defer func() { _ = f.Close() }()

	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fileErr("工作簿里没有工作表")
	}
	sheet := sheets[0]
	var notices []string
	found := false
	for _, name := range sheets {
		if name == SheetName {
			sheet, found = name, true
			break
		}
	}
	if !found && len(sheets) > 1 {
		notices = append(notices, fmt.Sprintf("没有名为「%s」的工作表，读的是第一张「%s」", SheetName, sheet))
	}

	rows, err := f.GetRows(sheet, excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, fileErr("工作表「%s」读取失败：%v", sheet, err)
	}
	// 行数先粗查一次再逐格取类型：逐格那一步每格要查一次样式，
	// 一张十万行的表在那里会慢得多，而它注定要被拒。
	nonEmpty := 0
	for _, r := range rows {
		if !blankStrings(r) {
			nonEmpty++
		}
	}
	if nonEmpty-1 > MaxRows {
		return nil, fileErr("数据行有 %d 行，单次上限 %d 行：请拆成多个文件分批导入", nonEmpty-1, MaxRows)
	}

	// 表头：第一个非空行。
	headerIdx := -1
	for i, r := range rows {
		if !blankStrings(r) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil, fileErr("工作表「%s」是空的", sheet)
	}

	merges, err := f.GetMergeCells(sheet, true)
	if err != nil {
		return nil, fileErr("读取合并单元格失败：%v", err)
	}
	// fill[行号][列号] = 合并区域左上角的坐标（都是 1 起）。
	type pos struct{ col, row int }
	fill := map[pos]pos{}
	for _, m := range merges {
		c1, r1, err1 := excelize.CellNameToCoordinates(m.GetStartAxis())
		c2, r2, err2 := excelize.CellNameToCoordinates(m.GetEndAxis())
		if err1 != nil || err2 != nil {
			return nil, fileErr("合并单元格坐标解析失败：%s:%s", m.GetStartAxis(), m.GetEndAxis())
		}
		if r1 <= headerIdx+1 && r2 >= headerIdx+1 && (c1 != c2 || r1 != r2) {
			return nil, fileErr("表头行（第 %d 行）里有合并单元格 %s:%s，列与表头对不上；请取消合并",
				headerIdx+1, m.GetStartAxis(), m.GetEndAxis())
		}
		for r := r1; r <= r2; r++ {
			for c := c1; c <= c2; c++ {
				if r == r1 && c == c1 {
					continue
				}
				fill[pos{c, r}] = pos{c1, r1}
			}
		}
	}

	styleIsDate := map[int]bool{}
	isDateStyle := func(id int) bool {
		if v, ok := styleIsDate[id]; ok {
			return v
		}
		st, err := f.GetStyle(id)
		v := err == nil && st != nil && numFmtIsDate(st.NumFmt, st.CustomNumFmt)
		styleIsDate[id] = v
		return v
	}

	cellAt := func(col, row int) (Cell, error) {
		name, err := excelize.CoordinatesToCellName(col, row)
		if err != nil {
			return Cell{}, err
		}
		raw := ""
		if row-1 < len(rows) && col-1 < len(rows[row-1]) {
			raw = rows[row-1][col-1]
		}
		if formula, _ := f.GetCellFormula(sheet, name); formula != "" {
			return Cell{Value: raw, Kind: KindFormula}, nil
		}
		if strings.TrimSpace(raw) == "" {
			return Cell{Value: raw, Kind: KindText}, nil
		}
		typ, err := f.GetCellType(sheet, name)
		if err != nil {
			return Cell{}, err
		}
		switch typ {
		case excelize.CellTypeSharedString, excelize.CellTypeInlineString, excelize.CellTypeFormula:
			// CellTypeFormula（t="str"）是「公式算出的字符串」—— 没有公式文本时
			// 它就是一段普通字符串（有公式的已经在上面拦下了）。
			return Cell{Value: raw, Kind: KindText}, nil
		case excelize.CellTypeBool:
			return Cell{Value: raw, Kind: KindBool}, nil
		case excelize.CellTypeError:
			return Cell{Value: raw, Kind: KindError}, nil
		case excelize.CellTypeDate:
			return Cell{Value: raw, Kind: KindDate}, nil
		default: // 数字（t="n" 或不写 t）
			if sid, err := f.GetCellStyle(sheet, name); err == nil && isDateStyle(sid) {
				return Cell{Value: raw, Kind: KindDate}, nil
			}
			return Cell{Value: raw, Kind: KindNumber}, nil
		}
	}

	s := &Sheet{Format: FormatXLSX, Header: rows[headerIdx], Notices: notices}
	width := len(s.Header)
	for i := headerIdx + 1; i < len(rows); i++ {
		line := i + 1
		n := max(width, len(rows[i]))
		cells := make([]Cell, n)
		for c := 1; c <= n; c++ {
			cell, err := cellAt(c, line)
			if err != nil {
				return nil, fileErr("第 %d 行读取失败：%v", line, err)
			}
			if src, ok := fill[pos{c, line}]; ok && strings.TrimSpace(cell.Value) == "" {
				if src.row <= headerIdx+1 {
					// 数据格与表头合并：列对不上，同表头合并那条。
					return nil, fileErr("第 %d 行第 %d 列与表头合并在一起，请取消合并", line, c)
				}
				top, err := cellAt(src.col, src.row)
				if err != nil {
					return nil, fileErr("第 %d 行读取失败：%v", src.row, err)
				}
				top.Merged = true
				cell = top
			}
			cells[c-1] = cell
		}
		s.appendRow(line, cells)
	}
	return s, nil
}

// appendRow 跳过整行空白（Excel 里删掉内容但没删行，或者行尾的格式残留），
// 其余的按原行号收下。
func (s *Sheet) appendRow(line int, cells []Cell) {
	for _, c := range cells {
		if strings.TrimSpace(c.Value) != "" {
			s.Rows = append(s.Rows, SheetRow{Line: line, Cells: cells})
			return
		}
	}
}

func blankStrings(r []string) bool {
	for _, v := range r {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

// numFmtIsDate 判断一个数字格式是不是日期 / 时间格式。
//
// 内置格式按 ECMA-376 §18.8.30 的编号：14–22、45–47 是日期时间；
// 27–36、50–58 是东亚区域（含简体中文）的内置日期格式 —— 中文 Excel 把「1-2」
// 转成的正是这一组里的 m"月"d"日"。自定义格式按格式串里有没有日期时间占位符判断，
// 引号里的字面量与方括号里的颜色 / 条件不算。
func numFmtIsDate(id int, custom *string) bool {
	switch {
	case id >= 14 && id <= 22, id >= 27 && id <= 36, id >= 45 && id <= 47, id >= 50 && id <= 58:
		return true
	}
	if custom == nil {
		return false
	}
	code := strings.ToLower(*custom)
	if code == "general" || code == "@" {
		return false
	}
	inQuote, inBracket := false, false
	for i := 0; i < len(code); i++ {
		ch := code[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
		case inQuote:
		case ch == '[':
			inBracket = true
		case ch == ']':
			inBracket = false
		case inBracket:
		case ch == '\\':
			i++ // 转义的下一个字符是字面量
		case ch == 'y', ch == 'd', ch == 'h', ch == 's', ch == 'm':
			// 'e'（纪年）刻意不认：科学计数法 0.00E+00 里也有它。
			return true
		}
	}
	return false
}

// numberText 把一个数字格的原始值变成**十进制字符串**，不经过任何浮点运算。
//
// 原始值可能是「12.3」，也可能是某个程序写出来的「12.300000000000001」或
// 「1.23E+1」。ParseFloat 再用 'f' + 精度 -1 格式化，得到的是**能唯一还原这个
// float64 的最短十进制串** —— 12.300000000000001 与 12.3 是同一个 float64 时，
// 出来的就是「12.3」。之后的金额换算一律在这个字符串上做（parseYuan），
// 一次乘法都没有：Number("0.29") * 100 是 28.999999999999996。
//
// 超过 2^53 的整数在 float64 里已经不精确（SKU 编码里的长数字最容易撞上），
// 返回 ok=false，由调用方报「超出精度，请把这一列设为文本」。
func numberText(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return raw, false
	}
	if math.Abs(f) >= 1<<53 {
		return raw, false
	}
	return strconv.FormatFloat(f, 'f', -1, 64), true
}
