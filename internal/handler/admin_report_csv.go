package handler

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/keel/keel/internal/service"
)

// 经营报表的 CSV 导出（GET /admin/reports/products.csv、/admin/reports/stores.csv）。
//
// 两条导出与对应的 JSON 接口读同一组参数、调同一个 service 方法（同一份口径、同一个判权），
// 只是最后一步写成 CSV。handler 方法各自放在 JSON 版的那个文件里（contract_test.go 的
// query 参数对账按文件读 c.Query，参数集合相同的放一起）；这里只放「结果 → CSV 字节」
// 这几个纯函数，一个 c.Query 都不读。
//
// 格式上的三条规矩：
//
//   - UTF-8 带 BOM：没有它，中文 Excel 双击打开时按本地代码页（GBK）解，表头一片乱码。
//     与批量导入模板（catalogimport.TemplateCSV）同一个理由。
//   - 金额写成「元」、两位小数：导出是给人拿去 Excel 里求和、做表的，写分要人再除一次 100。
//     整数运算拼出来，不经浮点。
//   - 防公式注入：以 = + - @ 开头（或以制表符 / 回车开头）的文本单元格前面加一个单引号。
//     商品标题是商家自己填的，但导出的文件会被转给财务、老板打开 —— 一个标题叫
//     「=HYPERLINK(...)」的商品不该在别人的 Excel 里变成一个可点的链接。
//     数字列是我们自己格式化出来的，不经过这一步（负数的「-」不能被加上引号）。

const csvBOM = "\xEF\xBB\xBF"

// csvText 处理一个来自用户输入的文本单元格（防公式注入）。
func csvText(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// csvYuan 把分写成元，两位小数，不带货币符号与千分位（Excel 认得出是数字）。
func csvYuan(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

func csvInt(v int64) string { return strconv.FormatInt(v, 10) }

// writeCSVTable 把表头与行写成带 BOM、CRLF 的 CSV。
func writeCSVTable(header []string, rows [][]string) []byte {
	var buf bytes.Buffer
	buf.WriteString(csvBOM)
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	_ = w.Write(header)
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
	return buf.Bytes()
}

// reportFileNames 是下载的文件名：中文名给认 RFC 5987 的浏览器，ASCII 名兜底。
// 都带窗口的起止日期（店铺时区），同一张表导出几次不会互相覆盖。
func reportFileNames(zh, en string, w service.ReportWindow) (utf, ascii string) {
	from := w.Current.StartDate.Format("2006-01-02")
	to := w.Current.EndDate.Format("2006-01-02")
	return fmt.Sprintf("%s_%s_%s.csv", zh, from, to), fmt.Sprintf("%s-%s-%s.csv", en, from, to)
}

func sendCSV(c *gin.Context, utfName, asciiName string, data []byte) {
	c.Header("Content-Disposition", "attachment; filename=\""+asciiName+"\"; filename*=UTF-8''"+
		url.PathEscape(utfName))
	// 报表是此刻的数字，中间层缓存一份会让「刚才导出的」与「现在看到的」对不上。
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
}

// reportProductsCSV 是商品排行的 CSV。列与契约 GET /admin/reports/products.csv 的描述逐一对应。
func reportProductsCSV(r service.ReportProductRanking) []byte {
	header := []string{"排名", "商品ID", "商品标题", "类目ID", "销量", "销售额（元）", "支付订单数", "已退件数", "已退金额（元）"}
	rows := make([][]string, 0, len(r.Items))
	for i, p := range r.Items {
		rows = append(rows, []string{
			strconv.Itoa(i + 1), csvInt(p.ProductID), csvText(p.Title), csvInt(p.CategoryID),
			csvInt(p.Quantity), csvYuan(p.AmountCents), csvInt(p.OrderCount),
			csvInt(p.RefundedQuantity), csvYuan(p.RefundedAmountCents),
		})
	}
	return writeCSVTable(header, rows)
}

// reportStoresCSV 是门店 / 大区对比的 CSV：先门店、再大区小计，第一列区分两者。
func reportStoresCSV(r service.ReportStoreComparison) []byte {
	header := []string{"类型", "大区ID", "大区", "门店ID", "门店编码", "门店", "已删除",
		"支付订单数", "支付金额（元）", "退款金额（元）", "净销售额（元）"}
	rows := make([][]string, 0, len(r.Stores)+len(r.Regions))
	for _, s := range r.Stores {
		deleted := "否"
		if s.Deleted {
			deleted = "是"
		}
		rows = append(rows, []string{
			"门店", csvInt(s.RegionID), csvText(s.RegionName), csvInt(s.StoreID), csvText(s.StoreCode),
			csvText(s.StoreName), deleted, csvInt(s.OrderCount),
			csvYuan(s.PaidCents), csvYuan(s.RefundCents), csvYuan(s.PaidCents - s.RefundCents),
		})
	}
	for _, g := range r.Regions {
		rows = append(rows, []string{
			"大区小计", csvInt(g.RegionID), csvText(g.RegionName), "", "", "", "",
			csvInt(g.OrderCount), csvYuan(g.PaidCents), csvYuan(g.RefundCents), csvYuan(g.PaidCents - g.RefundCents),
		})
	}
	return writeCSVTable(header, rows)
}
