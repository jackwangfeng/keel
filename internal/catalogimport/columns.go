package catalogimport

import (
	"bytes"
	"strings"
	"unicode"

	"github.com/xuri/excelize/v2"
)

// ColumnKey 是一列在代码里的名字。表头文字可以有好几种写法（全角半角括号、
// 带不带空格），代码里只认这个。
type ColumnKey string

const (
	ColTitle       ColumnKey = "title"
	ColSubtitle    ColumnKey = "subtitle"
	ColCategory    ColumnKey = "category"
	ColSpecNames   ColumnKey = "spec_names"
	ColSpecValues  ColumnKey = "spec_values"
	ColSKUCode     ColumnKey = "sku_code"
	ColPrice       ColumnKey = "price"
	ColStock       ColumnKey = "stock"
	ColWeight      ColumnKey = "weight"
	ColImageURL    ColumnKey = "image_url"
	ColDescription ColumnKey = "description"
)

// Column 是模板里的一列。
type Column struct {
	Key      ColumnKey
	Header   string // 模板里写的表头，也是报错时引用的列名
	Required bool
	Note     string // 「填写说明」那张表里的说明
	Example  string // 说明页里的示例值
	Width    float64
}

// Columns 是模板的全部列，顺序就是模板里的顺序。
//
// **这张表是唯一真相源**：模板生成、表头识别、报错里的列名、「填写说明」页
// 全从这里来。把列名在别处再写一遍，就是在制造「模板改了而校验没跟上」的机会。
var Columns = []Column{
	{ColTitle, "商品标题", true,
		"必填，不超过 200 字。**同一个标题的多行合并成同一件商品**（一行一个规格 / SKU）；" +
			"副标题、类目、描述只需在该商品的第一行填，后面的行留空或填相同内容。",
		"雪纺碎花连衣裙", 28},
	{ColSubtitle, "副标题", false, "可空，不超过 200 字。", "夏季新款 显瘦", 18},
	{ColCategory, "类目", false,
		"可空。填已有类目的名字，或从一级写到末级的路径（用 > 或 / 分隔，如「服装 > 女装 > 连衣裙」）。" +
			"留空或对不上时，系统按标题用向量相似度推荐类目，预检结果里可以改。",
		"女装", 16},
	{ColSpecNames, "规格名", false,
		"可空。多个维度用分号分隔，如「颜色;尺码」。同一商品有多行时每一行都要填，且维度相同。",
		"颜色;尺码", 12},
	{ColSpecValues, "规格值", false,
		"与规格名一一对应，同样用分号分隔，如「红;M」。同一商品内规格值组合不能重复。",
		"红;M", 12},
	{ColSKUCode, "SKU 编码", true,
		"必填，不超过 64 字，在本店内唯一（与已有商品也不能重复）。**本列请保持文本格式**：" +
			"Excel 会把长数字编码转成科学计数法、吃掉前导 0，把「1-2」转成日期。",
		"DRESS-RED-M", 18},
	{ColPrice, "基准价（元）", true,
		"必填，单位元，最多两位小数，如 199 或 199.90。不要写千分位逗号。", "199.90", 12},
	{ColStock, "库存", true, "必填，非负整数。写进默认门店的库存。", "100", 8},
	{ColWeight, "重量（克）", false, "可空，非负整数，单位克。", "350", 10},
	{ColImageURL, "图片 URL", false,
		"可空，http / https 地址，多张用分号或换行分隔。**本期只记录不下载**：" +
			"导入结果里会列出这些地址，图片请在商品页上传。",
		"https://example.com/dress-red.jpg", 30},
	{ColDescription, "描述", false, "可空，商品详情文字。", "100% 雪纺，可机洗", 30},
}

// ColumnByKey 取一列的定义。
func ColumnByKey(k ColumnKey) Column {
	for _, c := range Columns {
		if c.Key == k {
			return c
		}
	}
	panic("未定义的列：" + string(k))
}

// normalizeHeader 把表头文字规范成可比较的形状：去空白、全角转半角、转小写。
// 于是「基准价(元)」「基准价（元）」「 基准价 （元） 」是同一列，
// 「SKU编码」「sku 编码」也是。
func normalizeHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsSpace(r) || r == '\u200b' || r == '\ufeff' || r == '*' {
			continue
		}
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// headerAliases 是表头的别名（规范化之后比）。只收几个真实会出现的写法，
// 不做模糊匹配 —— 猜错一列的代价是整列数据进了错的字段。
var headerAliases = map[string]ColumnKey{
	"标题":    ColTitle,
	"商品名称":  ColTitle,
	"sku编码": ColSKUCode,
	"货号":    ColSKUCode,
	"基准价":   ColPrice,
	"价格(元)": ColPrice,
	"售价(元)": ColPrice,
	"重量":    ColWeight,
	"图片url": ColImageURL,
	"图片地址":  ColImageURL,
	"商品描述":  ColDescription,
	"规格":    ColSpecNames,
	"规格名称":  ColSpecNames,
	"规格值名称": ColSpecValues,
	"库存数量":  ColStock,
	"分类":    ColCategory,
	"商品类目":  ColCategory,
	"商品副标题": ColSubtitle,
}

func headerKey(h string) (ColumnKey, bool) {
	n := normalizeHeader(h)
	if n == "" {
		return "", false
	}
	for _, c := range Columns {
		if normalizeHeader(c.Header) == n {
			return c.Key, true
		}
	}
	k, ok := headerAliases[n]
	return k, ok
}

// ---------------------------------------------------------------------------
// 模板
// ---------------------------------------------------------------------------

// TemplateCSV 生成 csv 模板：UTF-8 BOM + 一行表头。
//
// BOM 不是装饰：没有它，中文 Excel 双击打开这个文件时按 GBK 解，表头一片乱码。
// 不带示例行 —— 示例行最常见的结局是被原样导入，成为店里的一件「雪纺碎花连衣裙」。
// 示例在 xlsx 模板的「填写说明」页里。
func TemplateCSV() []byte {
	var b bytes.Buffer
	b.WriteString("\xEF\xBB\xBF")
	for i, c := range Columns {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c.Header)
	}
	b.WriteString("\r\n")
	return b.Bytes()
}

// TemplateXLSX 生成 xlsx 模板：「商品」表只有表头，「填写说明」表逐列说明 + 示例。
//
// 「商品」表的数据区整列设成**文本格式**（内置格式 49，即「@」）。这是对
// 「Excel 自动转格式」最便宜的一道预防：文本格里的「1-2」不会变成日期，
// 「00123」不会丢前导 0，「6901234567890」不会变成 6.90123E+12。
// 用户从别处粘贴进来的格式仍然可能覆盖它，所以校验那一侧照样要查（Parse）。
func TemplateXLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	if err := f.SetSheetName("Sheet1", SheetName); err != nil {
		return nil, err
	}
	textStyle, err := f.NewStyle(&excelize.Style{NumFmt: 49})
	if err != nil {
		return nil, err
	}
	headStyle, err := f.NewStyle(&excelize.Style{
		NumFmt: 49,
		Font:   &excelize.Font{Bold: true},
		Fill:   excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#EEF2F7"}},
	})
	if err != nil {
		return nil, err
	}
	reqStyle, err := f.NewStyle(&excelize.Style{
		NumFmt: 49,
		Font:   &excelize.Font{Bold: true, Color: "#C0392B"},
		Fill:   excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#EEF2F7"}},
	})
	if err != nil {
		return nil, err
	}
	for i, c := range Columns {
		col, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			return nil, err
		}
		if err := f.SetColStyle(SheetName, col, textStyle); err != nil {
			return nil, err
		}
		if err := f.SetColWidth(SheetName, col, col, c.Width); err != nil {
			return nil, err
		}
		cell := col + "1"
		if err := f.SetCellStr(SheetName, cell, c.Header); err != nil {
			return nil, err
		}
		st := headStyle
		if c.Required {
			st = reqStyle
		}
		if err := f.SetCellStyle(SheetName, cell, cell, st); err != nil {
			return nil, err
		}
	}
	if err := f.SetPanes(SheetName, &excelize.Panes{
		Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft",
	}); err != nil {
		return nil, err
	}

	const help = "填写说明"
	if _, err := f.NewSheet(help); err != nil {
		return nil, err
	}
	for i, h := range []string{"列", "必填", "说明", "示例"} {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellStr(help, cell, h)
		_ = f.SetCellStyle(help, cell, cell, headStyle)
	}
	for i, c := range Columns {
		row := i + 2
		req := ""
		if c.Required {
			req = "是"
		}
		for j, v := range []string{c.Header, req, strings.ReplaceAll(c.Note, "**", ""), c.Example} {
			cell, _ := excelize.CoordinatesToCellName(j+1, row)
			if err := f.SetCellStr(help, cell, v); err != nil {
				return nil, err
			}
		}
	}
	tail := len(Columns) + 3
	for i, line := range []string{
		"单次最多 2000 行、文件不超过 5 MB；超出请拆成多个文件。",
		"不要使用公式（请「粘贴为数值」）；表头行不要合并单元格。",
		"导入的商品一律是草稿，不会自动上架；上架时才做广告法违禁词拦截，预检里会先标出来。",
		"同一份文件重复确认导入不会重复建商品。",
	} {
		cell, _ := excelize.CoordinatesToCellName(1, tail+i)
		_ = f.SetCellStr(help, cell, line)
	}
	_ = f.SetColWidth(help, "A", "A", 14)
	_ = f.SetColWidth(help, "B", "B", 6)
	_ = f.SetColWidth(help, "C", "C", 90)
	_ = f.SetColWidth(help, "D", "D", 30)
	f.SetActiveSheet(0)

	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
