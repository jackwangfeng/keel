package catalogimport

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// 这一组测试不碰数据库：表格解析是边界情况最多的那一块，它们不该只在要起
// Postgres 的测试里才被看见（包注释第一节）。

const header = "商品标题,副标题,类目,规格名,规格值,SKU 编码,基准价（元）,库存,重量（克）,图片 URL,描述\n"

func mustParse(t *testing.T, data []byte) *Parsed {
	t.Helper()
	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func codes(issues []Issue) []string {
	var out []string
	for _, i := range issues {
		out = append(out, i.Code)
	}
	return out
}

func hasIssue(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func TestParseYuanHasNoFloatError(t *testing.T) {
	// 0.29 * 100 在浮点里是 28.999999999999996；这里必须是 29。
	for in, want := range map[string]int64{
		"0.29": 29, "199": 19900, "199.9": 19990, "199.90": 19990, "¥12.5": 1250,
		"12.30元": 1230, " 7.07 ": 707, "0": 0, "99999999.99": 9_999_999_999,
		"1.1": 110, "2.675": -1, "1,299.00": -1, "-1": -1, "1e3": -1, "": -1, "abc": -1,
		"100000000.01": -1, "100000000.00": 10_000_000_000, ".5": -1,
	} {
		got, err := ParseYuan(in)
		if want < 0 {
			if err == nil {
				t.Errorf("ParseYuan(%q) = %d，期望报错", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParseYuan(%q) = %d, %v；期望 %d", in, got, err, want)
		}
	}
}

func TestNumberTextUsesShortestRoundTrip(t *testing.T) {
	// 某些程序把 12.3 写成 12.300000000000001：它与 12.3 是同一个 float64，
	// 最短还原串就是「12.3」，于是金额换算拿到的是用户填的那个数。
	for in, want := range map[string]string{
		"12.300000000000001": "12.3", "0.28999999999999998": "0.29", "1.23E+1": "12.3",
		"6901234567890": "6901234567890", "10": "10", "100.0": "100",
	} {
		got, ok := numberText(in)
		if !ok || got != want {
			t.Errorf("numberText(%q) = %q,%v；期望 %q", in, got, ok, want)
		}
	}
	if _, ok := numberText("12345678901234567890"); ok {
		t.Error("超过 2^53 的整数已经不精确，必须报出来")
	}
}

func TestCSVMergesSpecRowsIntoOneProduct(t *testing.T) {
	csv := header +
		"雪纺连衣裙,夏季新款,女装,颜色;尺码,红;M,D-R-M,199.9,10,300,https://img.example.com/a.jpg,描述一\n" +
		"雪纺连衣裙,,,颜色;尺码,红;L,D-R-L,199.90,5,,https://img.example.com/b.jpg,\n" +
		"雪纺连衣裙,夏季新款,女装,颜色;尺码,蓝;M,D-B-M,209,0,,,\n" +
		"手冲咖啡壶,,,,,POT-1,89,3,,,\n"
	p := mustParse(t, []byte(csv))
	if len(p.Groups) != 2 {
		t.Fatalf("期望 2 件商品，得到 %d", len(p.Groups))
	}
	g := p.Groups[0]
	if len(g.Rows) != 3 || g.Subtitle != "夏季新款" || g.Category != "女装" || g.Description != "描述一" {
		t.Fatalf("第一件商品合并不对：%+v", g)
	}
	if len(g.ImageURLs) != 2 {
		t.Errorf("图片地址应当合并去重成 2 个，得到 %v", g.ImageURLs)
	}
	for _, r := range p.Rows {
		if len(r.Errors) > 0 {
			t.Errorf("第 %d 行不该有错：%v", r.Line, r.Errors)
		}
	}
	if *p.Rows[1].PriceCents != 19990 || *p.Rows[2].PriceCents != 20900 {
		t.Errorf("价格换算不对：%d %d", *p.Rows[1].PriceCents, *p.Rows[2].PriceCents)
	}
	if !p.GroupOK(0) || !p.GroupOK(1) {
		t.Error("两件商品都应当可导入")
	}
	if p.Rows[0].Line != 2 {
		t.Errorf("行号应当与 Excel 左侧一致（表头第 1 行），得到 %d", p.Rows[0].Line)
	}
}

func TestGroupRulesAreEnforced(t *testing.T) {
	csv := header +
		"连衣裙,副标题A,,颜色,红,A1,10,1,,,\n" +
		"连衣裙,副标题B,,颜色,蓝,A2,10,1,,,\n" + // 副标题冲突
		"连衣裙,,,颜色,红,A3,10,1,,,\n" + // 规格组合重复
		"连衣裙,,,尺码,M,A4,10,1,,,\n" + // 规格名不一致
		"连衣裙,,,,,A5,10,1,,,\n" + // 多行商品却没填规格
		"T恤,,,,,A1,10,1,,,\n" + // SKU 编码与第 2 行重复
		",,,,,A6,10,1,,,\n" // 没有标题
	p := mustParse(t, []byte(csv))
	want := map[int]string{
		3: CodeGroupConflict, 4: CodeSpecDuplicate, 5: CodeSpecMismatch, 6: CodeSpecMismatch,
		7: CodeDuplicateSKUCode, 8: CodeRequired,
	}
	for line, code := range want {
		r := p.LineRow(line)
		if !hasIssue(r.Errors, code) {
			t.Errorf("第 %d 行应当有 %s，实际 %v", line, code, codes(r.Errors))
		}
	}
	if len(p.LineRow(2).Errors) != 0 {
		t.Errorf("第 2 行本身没错：%v", p.LineRow(2).Errors)
	}
	if !hasIssue(p.LineRow(2).Warnings, CodeGroupBlocked) {
		t.Error("第 2 行被同组的错拖累，应当有 group_blocked 提示")
	}
	if p.GroupOK(0) {
		t.Error("连衣裙那一组有错，不能导入")
	}
}

func TestRowLevelValidation(t *testing.T) {
	csv := header +
		strings.Repeat("长", 201) + ",,,,,B1,10,1,,,\n" +
		"商品2,,,,,,10,1,,,\n" +
		"商品3,,,,,B3,1.234,1,,,\n" +
		"商品4,,,,,B4,10,-1,,,\n" +
		"商品5,,,,,B5,10,1,1.5,,\n" +
		"商品6,,,,,B6,10,1,,ftp://x/y.jpg,\n" +
		"商品7,,,颜色;尺码,红,B7,10,1,,,\n" +
		"商品8,,,,,B8,0,1,,,\n"
	p := mustParse(t, []byte(csv))
	for line, code := range map[int]string{
		2: CodeTooLong, 3: CodeRequired, 4: CodeInvalidPrice, 5: CodeInvalidInteger,
		6: CodeInvalidInteger, 7: CodeImageURLInvalid, 8: CodeSpecInvalid,
	} {
		if r := p.LineRow(line); !hasIssue(r.Errors, code) {
			t.Errorf("第 %d 行应当有 %s，实际 %v", line, code, codes(r.Errors))
		}
	}
	if r := p.LineRow(9); len(r.Errors) != 0 || !hasIssue(r.Warnings, CodePriceZero) {
		t.Errorf("0 元是提示不是错误：%v / %v", r.Errors, r.Warnings)
	}
}

func TestHeaderProblemsAreFileErrors(t *testing.T) {
	_, err := Parse([]byte("商品标题,库存\n连衣裙,1\n"))
	var fe *FileError
	if !errors.As(err, &fe) || len(fe.Issues) != 2 {
		t.Fatalf("缺 SKU 编码与基准价两列，应当是带两条原因的 FileError，得到 %v", err)
	}
	_, err = Parse([]byte(header))
	if !errors.As(err, &fe) {
		t.Fatalf("只有表头应当是 FileError，得到 %v", err)
	}
	// 表头写法宽容：半角括号、多余空格、未知列。
	p := mustParse(t, []byte("商品标题 , SKU编码,基准价(元),库存,备注\n连衣裙,X1,1,1,随便写\n"))
	if len(p.Rows) != 1 || len(p.Rows[0].Errors) != 0 {
		t.Fatalf("表头别名没认出来：%+v", p.Rows)
	}
	if len(p.Notices) != 1 || !strings.Contains(p.Notices[0], "备注") {
		t.Errorf("未知列应当提示并忽略：%v", p.Notices)
	}
}

func TestRowLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString(header)
	for i := 0; i <= MaxRows; i++ {
		b.WriteString("商品,,,,,C")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(",1,1,,,\n")
	}
	_, err := Parse([]byte(b.String()))
	var fe *FileError
	if !errors.As(err, &fe) || !strings.Contains(fe.Issues[0], "2000") {
		t.Fatalf("2001 行应当被拒并说出上限，得到 %v", err)
	}
}

func TestSizeAndFormatSniffing(t *testing.T) {
	if _, err := Parse(make([]byte, MaxFileBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超过 5 MB 应当是 ErrTooLarge，得到 %v", err)
	}
	if _, err := Parse([]byte("\xD0\xCF\x11\xE0 old xls")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("老 xls 应当是 ErrUnsupportedFormat，得到 %v", err)
	}
	if _, err := Parse([]byte("a\x00b")); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("二进制内容应当是 ErrUnsupportedFormat，得到 %v", err)
	}
}

func TestCSVInGBKAndWithBOM(t *testing.T) {
	body := header + "连衣裙,,,,,G1,10,1,,,\n"
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	p := mustParse(t, gbk)
	if p.Rows[0].Title != "连衣裙" || len(p.Rows[0].Errors) != 0 {
		t.Fatalf("GBK 的 csv 没读对：%+v", p.Rows[0])
	}
	if len(p.Notices) == 0 {
		t.Error("按 GBK 读应当提示一句")
	}
	p = mustParse(t, append([]byte("\xEF\xBB\xBF"), body...))
	if p.Rows[0].Title != "连衣裙" {
		t.Fatalf("带 BOM 的 csv 表头没认出来：%+v", p.Rows[0])
	}
}

// xlsxWith 造一份 xlsx：表头 + 由 fill 填的内容。
func xlsxWith(t *testing.T, fill func(f *excelize.File, sheet string)) []byte {
	t.Helper()
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	sheet := "Sheet1"
	for i, c := range Columns {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellStr(sheet, cell, c.Header)
	}
	fill(f, sheet)
	buf, err := f.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestXLSXExcelPitfalls(t *testing.T) {
	data := xlsxWith(t, func(f *excelize.File, s string) {
		// 第 2、3 行：标题合并跨两行（常见写法），两个规格。
		_ = f.SetCellStr(s, "A2", "合并标题的连衣裙")
		_ = f.MergeCell(s, "A2", "A3")
		_ = f.SetCellStr(s, "D2", "颜色")
		_ = f.SetCellStr(s, "E2", "红")
		_ = f.SetCellStr(s, "F2", "M-RED")
		_ = f.SetCellValue(s, "G2", 12.3) // 数字格
		_ = f.SetCellValue(s, "H2", 10)
		_ = f.SetCellStr(s, "D3", "颜色")
		_ = f.SetCellStr(s, "E3", "蓝")
		_ = f.SetCellStr(s, "F3", "M-BLUE")
		_ = f.SetCellValue(s, "G3", 0.29)
		_ = f.SetCellValue(s, "H3", 5)

		// 第 4 行：价格是公式。
		_ = f.SetCellStr(s, "A4", "公式商品")
		_ = f.SetCellStr(s, "F4", "F-1")
		_ = f.SetCellFormula(s, "G4", "=1+1")
		_ = f.SetCellValue(s, "H4", 1)

		// 第 5 行：SKU 编码被存成数字、价格被 Excel 转成了日期。
		_ = f.SetCellStr(s, "A5", "日期商品")
		_ = f.SetCellValue(s, "F5", 6901234567890)
		_ = f.SetCellValue(s, "G5", 44928) // 2023-01-02 的序列号
		date, _ := f.NewStyle(&excelize.Style{NumFmt: 14})
		_ = f.SetCellStyle(s, "G5", "G5", date)
		_ = f.SetCellValue(s, "H5", 1)

		// 第 6 行：整行空白，第 7 行：正常。行号要跳过空白行但保持原值。
		_ = f.SetCellStr(s, "A7", "空行之后")
		_ = f.SetCellStr(s, "F7", "AFTER")
		_ = f.SetCellStr(s, "G7", "1")
		_ = f.SetCellStr(s, "H7", "1")
	})
	p := mustParse(t, data)
	if p.Format != FormatXLSX {
		t.Fatalf("格式应当是 xlsx，得到 %s", p.Format)
	}
	r3 := p.LineRow(3)
	if r3 == nil || r3.Title != "合并标题的连衣裙" || !hasIssue(r3.Warnings, CodeMergedCell) {
		t.Fatalf("合并单元格应当填到第 3 行并提示：%+v", r3)
	}
	if len(p.Groups[0].Rows) != 2 || !p.GroupOK(0) {
		t.Fatalf("合并标题的两行应当是一件可导入的商品：%+v / %v %v", p.Groups[0], p.LineRow(2).Errors, r3.Errors)
	}
	if *p.LineRow(2).PriceCents != 1230 || *r3.PriceCents != 29 {
		t.Errorf("数字格价格换算不对：%d %d", *p.LineRow(2).PriceCents, *r3.PriceCents)
	}
	if !hasIssue(p.LineRow(4).Errors, CodeFormulaCell) {
		t.Errorf("公式格应当报错：%v", codes(p.LineRow(4).Errors))
	}
	r5 := p.LineRow(5)
	if !hasIssue(r5.Errors, CodeDateCell) {
		t.Errorf("被转成日期的价格应当报错：%v", codes(r5.Errors))
	}
	if r5.SKUCode != "6901234567890" || !hasIssue(r5.Warnings, CodeNumericSKUCode) {
		t.Errorf("数字格 SKU 编码应当原样还原并提示：%q %v", r5.SKUCode, codes(r5.Warnings))
	}
	if p.LineRow(6) != nil || p.LineRow(7) == nil {
		t.Error("整行空白应当跳过，后面的行保留原行号")
	}
}

func TestXLSXHeaderMergeIsRejected(t *testing.T) {
	data := xlsxWith(t, func(f *excelize.File, s string) {
		_ = f.MergeCell(s, "A1", "B1")
		_ = f.SetCellStr(s, "A2", "x")
	})
	_, err := Parse(data)
	var fe *FileError
	if !errors.As(err, &fe) || !strings.Contains(fe.Issues[0], "合并") {
		t.Fatalf("表头合并应当是 FileError，得到 %v", err)
	}
}

func TestXLSXPrefersNamedSheet(t *testing.T) {
	f := excelize.NewFile()
	_ = f.SetCellStr("Sheet1", "A1", "这是说明页")
	_, _ = f.NewSheet(SheetName)
	for i, c := range Columns {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellStr(SheetName, cell, c.Header)
	}
	_ = f.SetSheetRow(SheetName, "A2", &[]any{"咖啡壶", "", "", "", "", "K1", "89", "3"})
	buf, _ := f.WriteToBuffer()
	p := mustParse(t, buf.Bytes())
	if len(p.Rows) != 1 || p.Rows[0].Title != "咖啡壶" {
		t.Fatalf("应当读名为「商品」的那张表：%+v", p.Rows)
	}
}

func TestTemplatesRoundTrip(t *testing.T) {
	// 模板本身必须能被自己的解析器读懂：只有表头 → 「一行数据都没有」，
	// 而不是「缺少必填列」（后者说明模板与表头识别对不上）。
	x, err := TemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"xlsx": x, "csv": TemplateCSV()} {
		_, err := Parse(data)
		var fe *FileError
		if !errors.As(err, &fe) || !strings.Contains(fe.Issues[0], "一行数据都没有") {
			t.Errorf("%s 模板：期望「一行数据都没有」，得到 %v", name, err)
		}
	}
	// 在 xlsx 模板里填一行（像用户那样），能读出来；数据区是文本格式，
	// 所以写进去的「00123」不会丢前导 0。
	f, err := excelize.OpenReader(bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.SetSheetRow(SheetName, "A2", &[]any{"咖啡壶", "", "", "", "", "00123", "89.5", "3"})
	buf, _ := f.WriteToBuffer()
	p := mustParse(t, buf.Bytes())
	if p.Rows[0].SKUCode != "00123" || *p.Rows[0].PriceCents != 8950 {
		t.Fatalf("模板填写后读回不对：%+v", p.Rows[0])
	}
}

func TestNumFmtIsDate(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		id     int
		custom *string
		want   bool
	}{
		{14, nil, true}, {22, nil, true}, {31, nil, true}, {57, nil, true},
		{0, nil, false}, {2, nil, false}, {49, nil, false},
		{164, s(`yyyy"年"m"月"d"日"`), true}, {164, s(`0.00E+00`), false},
		{164, s(`#,##0.00"元"`), false}, {164, s(`[Red]0.00`), false}, {164, s(`hh:mm`), true},
		{164, s(`@`), false}, {164, s(`General`), false},
	} {
		if got := numFmtIsDate(c.id, c.custom); got != c.want {
			cs := "<nil>"
			if c.custom != nil {
				cs = *c.custom
			}
			t.Errorf("numFmtIsDate(%d, %s) = %v，期望 %v", c.id, cs, got, c.want)
		}
	}
}
