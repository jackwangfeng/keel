package handler_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 经营报表的 CSV 导出（GET /admin/reports/products.csv、stores.csv）。数据是 report_test.go 的
// seedStandard（那张表写在那个文件头）；这里核对格式（BOM、表头、元、防公式注入、文件名）
// 与判权 / 范围和 JSON 版一致。

func (fx *reportFixture) csv(t *testing.T, role permRole, path string) (*httptest.ResponseRecorder, [][]string) {
	t.Helper()
	w := getAs(t, fx.sh.Host, "/api/v1/admin/reports/"+path, fx.tokens[role])
	wantStatus(t, w, http.StatusOK, "GET "+path+"（"+role.String()+"）")
	body := w.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("\xEF\xBB\xBF")) {
		t.Fatalf("%s 没有 UTF-8 BOM：中文 Excel 双击打开会是乱码", path)
	}
	rows, err := csv.NewReader(bytes.NewReader(body[3:])).ReadAll()
	if err != nil {
		t.Fatalf("%s 不是合法的 CSV：%v\n%s", path, err, body)
	}
	return w, rows
}

func TestReportCSVExports(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	w, rows := fx.csv(t, roleAdmin, "products.csv?"+rptWin)
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, `filename="product-ranking-2025-03-10-2025-03-11.csv"`) ||
		!strings.Contains(cd, "filename*=UTF-8''%E5%95%86%E5%93%81%E6%8E%92%E8%A1%8C_2025-03-10_2025-03-11.csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("\r\n")) {
		t.Error("换行应当是 CRLF")
	}
	wantHeader := []string{"排名", "商品ID", "商品标题", "类目ID", "销量", "销售额（元）", "支付订单数", "已退件数", "已退金额（元）"}
	if len(rows) != 3 || strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
		t.Fatalf("商品排行 CSV = %q", rows)
	}
	// 与 JSON 版同一份数字（report_test.go 的 TestReportProductRanking）：P2 5000 分 2 件 2 单已退 300；
	// P1 2900 分 6 件 3 单已退 3 件 1500。
	if got := rows[1]; got[0] != "1" || got[1] != fmt.Sprint(fx.p2) || got[4] != "2" || got[5] != "50.00" ||
		got[6] != "2" || got[7] != "0" || got[8] != "3.00" {
		t.Errorf("第 1 行 = %q", got)
	}
	if got := rows[2]; got[1] != fmt.Sprint(fx.ProductID) || got[4] != "6" || got[5] != "29.00" ||
		got[6] != "3" || got[7] != "3" || got[8] != "15.00" {
		t.Errorf("第 2 行 = %q", got)
	}

	// 防公式注入：以 = 开头的标题前面加单引号；带逗号、引号的标题按 RFC 4180 引起来。
	adminExec(t, `UPDATE products SET title = '=HYPERLINK("http://evil","点我")' WHERE id = $1`, fx.p2)
	adminExec(t, `UPDATE products SET title = '连衣裙, "夏季"款' WHERE id = $1`, fx.ProductID)
	_, rows = fx.csv(t, roleAdmin, "products.csv?"+rptWin)
	if rows[1][2] != `'=HYPERLINK("http://evil","点我")` {
		t.Errorf("以 = 开头的标题没有被转义成文本：%q", rows[1][2])
	}
	if rows[2][2] != `连衣裙, "夏季"款` {
		t.Errorf("带逗号与引号的标题读回来是 %q", rows[2][2])
	}

	// 门店对比：先门店后大区小计；数字与 JSON 版一致（N1 支付 4500 退款 2000 净 2500，3 单）。
	_, rows = fx.csv(t, roleAdmin, "stores.csv?"+rptWin)
	if rows[0][0] != "类型" || rows[0][10] != "净销售额（元）" {
		t.Fatalf("门店对比表头 = %q", rows[0])
	}
	var n1 []string
	stores, regions := 0, 0
	for _, r := range rows[1:] {
		switch r[0] {
		case "门店":
			stores++
			if regions > 0 {
				t.Errorf("门店行排在了大区小计之后：%q", rows)
			}
			if r[3] == fmt.Sprint(fx.N1) {
				n1 = r
			}
		case "大区小计":
			regions++
			if r[3] != "" || r[5] != "" {
				t.Errorf("大区小计那一行的门店列应当留空：%q", r)
			}
		default:
			t.Errorf("不认识的类型 %q", r[0])
		}
	}
	if n1 == nil || n1[7] != "3" || n1[8] != "45.00" || n1[9] != "20.00" || n1[10] != "25.00" || n1[6] != "否" {
		t.Errorf("N1 那一行 = %q", n1)
	}
	if regions < 2 || stores < 4 {
		t.Errorf("门店 %d 行、大区 %d 行", stores, regions)
	}

	// 窗口写错：422 Problem JSON，不是一份空 CSV。
	w = getAs(t, fx.sh.Host, "/api/v1/admin/reports/products.csv?period=custom&start_date=2025-03-12&end_date=2025-03-10",
		fx.tokens[roleAdmin])
	if p := problemOf(t, w, http.StatusUnprocessableEntity); p.Status != 422 {
		t.Errorf("坏窗口：%+v", p)
	}
	// 不带会话：401。
	wantStatus(t, getNoAuth(t, fx.sh.Host, "/api/v1/admin/reports/stores.csv?"+rptWin), http.StatusUnauthorized, "匿名导出")
}

// 范围与 JSON 版同一个判据：门店管理员只导得出自己门店的数，大区管理员只有本大区。
func TestReportCSVExportsAreScoped(t *testing.T) {
	fx := newReportFixture(t)
	fx.seedStandard(t)

	// 门店管理员（N1）：商品排行只算 O1 O2 O8 —— P1 5 件 2500，P2 2000。
	_, rows := fx.csv(t, roleStore, "products.csv?"+rptWin)
	if len(rows) != 3 || rows[1][1] != fmt.Sprint(fx.ProductID) || rows[1][4] != "5" || rows[1][5] != "25.00" ||
		rows[2][5] != "20.00" {
		t.Errorf("门店管理员的商品排行 CSV = %q", rows)
	}
	_, rows = fx.csv(t, roleStore, "stores.csv?"+rptWin)
	if len(rows) != 3 || rows[1][3] != fmt.Sprint(fx.N1) || rows[2][0] != "大区小计" {
		t.Errorf("门店管理员的门店对比 CSV 应当只有 N1 与它的大区：%q", rows)
	}
	_, rows = fx.csv(t, roleRegion, "stores.csv?"+rptWin)
	storeRows := 0
	for _, r := range rows[1:] {
		if r[0] == "门店" {
			storeRows++
			if r[1] != fmt.Sprint(fx.North) {
				t.Errorf("大区管理员导出了别的大区的门店：%q", r)
			}
		}
	}
	if storeRows != 2 {
		t.Errorf("大区管理员的门店对比 CSV 应当是华北两家：%q", rows)
	}
	// 范围外：收窄成只有表头的空表（与 JSON 版的全零一致），不是 403。
	_, rows = fx.csv(t, roleStore, fmt.Sprintf("products.csv?store_id=%d&%s", fx.E1, rptWin))
	if len(rows) != 1 {
		t.Errorf("门店管理员带范围外的门店应当只剩表头：%q", rows)
	}
}
