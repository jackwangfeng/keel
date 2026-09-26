package service

import (
	"errors"
	"testing"
	"time"

	"github.com/keel/keel/internal/repository"
)

// 经营报表的窗口、分桶与派生指标（report.go 的 resolveReportWindow / reportBuckets /
// deriveMetrics）。不碰数据库：这三样是纯函数，边界全在日历与整数算术上，
// 数据库那一侧的口径（哪些单算、按哪个时间落窗口）由 internal/handler/report_test.go 核对。

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestReportWindowPresets(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	// 上海 2026-09-26 00:30 —— UTC 还在 25 号。「今天」必须按店铺时区是 26 号。
	now := time.Date(2026, 9, 26, 0, 30, 0, 0, sh)
	at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, sh) }

	cases := []struct {
		period                               string
		curStart, curEnd, prevStart, prevEnd time.Time
	}{
		{"", at(2026, 9, 26, 0), now, at(2026, 9, 25, 0), now.AddDate(0, 0, -1)},
		{"today", at(2026, 9, 26, 0), now, at(2026, 9, 25, 0), now.AddDate(0, 0, -1)},
		{"yesterday", at(2026, 9, 25, 0), at(2026, 9, 26, 0), at(2026, 9, 24, 0), at(2026, 9, 25, 0)},
		{"last_7_days", at(2026, 9, 19, 0), at(2026, 9, 26, 0), at(2026, 9, 12, 0), at(2026, 9, 19, 0)},
		{"last_30_days", at(2026, 8, 27, 0), at(2026, 9, 26, 0), at(2026, 7, 28, 0), at(2026, 8, 27, 0)},
	}
	for _, c := range cases {
		w, err := resolveReportWindow(c.period, "", "", now, "Asia/Shanghai", sh)
		if err != nil {
			t.Fatalf("%q: %v", c.period, err)
		}
		if !w.Current.Start.Equal(c.curStart) || !w.Current.End.Equal(c.curEnd) {
			t.Errorf("%q 本期 = [%s, %s)，想要 [%s, %s)", c.period, w.Current.Start, w.Current.End, c.curStart, c.curEnd)
		}
		if !w.Previous.Start.Equal(c.prevStart) || !w.Previous.End.Equal(c.prevEnd) {
			t.Errorf("%q 上期 = [%s, %s)，想要 [%s, %s)", c.period, w.Previous.Start, w.Previous.End, c.prevStart, c.prevEnd)
		}
	}
}

func TestReportWindowCustomAndLimits(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, sh)

	w, err := resolveReportWindow("custom", "2026-09-01", "2026-09-10", now, "Asia/Shanghai", sh)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 11, 0, 0, 0, 0, sh); !w.Current.End.Equal(want) {
		t.Errorf("custom 的 end_date 是含的，终点应当是次日 0 点 %s，得到 %s", want, w.Current.End)
	}
	if want := time.Date(2026, 8, 22, 0, 0, 0, 0, sh); !w.Previous.Start.Equal(want) {
		t.Errorf("custom 的上期应当是前面同样的 10 天，从 %s 起，得到 %s", want, w.Previous.Start)
	}

	// 366 天（含首尾）刚好放行，367 天拒。2024 是闰年：2024-01-01 到 2024-12-31 是 366 天。
	if _, err := resolveReportWindow("custom", "2024-01-01", "2024-12-31", now, "Asia/Shanghai", sh); err != nil {
		t.Errorf("366 天应当放行：%v", err)
	}
	for _, bad := range [][2]string{
		{"2024-01-01", "2025-01-01"}, // 367 天
		{"2026-09-10", "2026-09-01"}, // 起晚于止
		{"2026-09-01", ""},           // 缺一端
		{"2026/09/01", "2026-09-02"}, // 格式不对
	} {
		_, err := resolveReportWindow("custom", bad[0], bad[1], now, "Asia/Shanghai", sh)
		if !errors.Is(err, ErrAdminListBadRequest) {
			t.Errorf("custom %v 应当是 ErrAdminListBadRequest，得到 %v", bad, err)
		}
	}
	if _, err := resolveReportWindow("last_year", "", "", now, "Asia/Shanghai", sh); !errors.Is(err, ErrAdminListBadRequest) {
		t.Errorf("不认识的 period 应当 422，得到 %v", err)
	}
	// 同一天：放行，且按小时分 24 个桶。
	w, err = resolveReportWindow("custom", "2026-09-01", "2026-09-01", now, "Asia/Shanghai", sh)
	if err != nil {
		t.Fatal(err)
	}
	if gran, edges := reportBuckets(w); gran != "hour" || len(edges) != 24 {
		t.Errorf("单日窗口应当按小时 24 桶，得到 %s × %d", gran, len(edges))
	}
}

func TestReportBucketsFollowTheShopCalendar(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	now := time.Date(2026, 9, 26, 14, 20, 0, 0, sh)

	w, _ := resolveReportWindow("today", "", "", now, "Asia/Shanghai", sh)
	gran, edges := reportBuckets(w)
	if gran != "hour" || len(edges) != 15 {
		t.Fatalf("今天 14:20 应当按小时出 00:00..14:00 共 15 桶，得到 %s × %d", gran, len(edges))
	}
	if l := bucketLabel(edges[14], sh, gran); l != "14:00" {
		t.Errorf("最后一桶的标签 = %q，想要 14:00", l)
	}

	w, _ = resolveReportWindow("last_7_days", "", "", now, "Asia/Shanghai", sh)
	gran, edges = reportBuckets(w)
	if gran != "day" || len(edges) != 7 || bucketLabel(edges[0], sh, gran) != "09-19" {
		t.Fatalf("近 7 天应当按天 09-19..09-25，得到 %s × %d（首桶 %s）", gran, len(edges), bucketLabel(edges[0], sh, gran))
	}

	// 夏令时：纽约 2026-03-08 只有 23 个小时。按日历切，窗口终点是次日 0 点，而不是 +24h。
	ny := mustLoc(t, "America/New_York")
	w, err := resolveReportWindow("custom", "2026-03-08", "2026-03-08", now, "America/New_York", ny)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Current.End.Sub(w.Current.Start); got != 23*time.Hour {
		t.Errorf("纽约 2026-03-08 应当是 23 小时，得到 %s", got)
	}
	if _, edges := reportBuckets(w); len(edges) != 23 {
		t.Errorf("夏令时那一天应当是 23 个小时桶，得到 %d", len(edges))
	}
}

func TestReportLocationFallsBackToShanghai(t *testing.T) {
	for _, bad := range []string{"", "Mars/Olympus", "Local", "local"} {
		name, loc := reportLocation(bad)
		if name != repository.DefaultShopTimezone || loc.String() != "Asia/Shanghai" {
			t.Errorf("时区 %q 应当回落到 Asia/Shanghai，得到 %s / %s", bad, name, loc)
		}
	}
	if name, _ := reportLocation("Europe/London"); name != "Europe/London" {
		t.Errorf("合法的时区应当原样使用，得到 %s", name)
	}
}

func TestDeriveMetrics(t *testing.T) {
	m := deriveMetrics(
		repository.ReportOrderTotals{OrderCount: 3, BuyerCount: 2, PaidCents: 1001},
		repository.ReportRefundTotals{RefundCount: 1, RefundCents: 300},
	)
	if m.NetSalesCents != 701 {
		t.Errorf("净销售额 = %d，想要 1001 − 300 = 701", m.NetSalesCents)
	}
	if m.AvgOrderValueCents != 501 {
		t.Errorf("客单价 = %d，想要 1001 ÷ 2 = 500.5 → 四舍五入 501", m.AvgOrderValueCents)
	}
	if m.RefundRate == nil || *m.RefundRate < 0.2997 || *m.RefundRate > 0.2998 {
		t.Errorf("退款率 = %v，想要 300 ÷ 1001", m.RefundRate)
	}
	if m.AvgOrderValueCents = deriveMetrics(repository.ReportOrderTotals{BuyerCount: 3, PaidCents: 1000},
		repository.ReportRefundTotals{}).AvgOrderValueCents; m.AvgOrderValueCents != 333 {
		t.Errorf("1000 ÷ 3 = 333.3 应当舍成 333，得到 %d", m.AvgOrderValueCents)
	}

	z := deriveMetrics(repository.ReportOrderTotals{}, repository.ReportRefundTotals{RefundCount: 1, RefundCents: 50})
	if z.RefundRate != nil {
		t.Errorf("没有支付时退款率应当是 nil（不是 0 也不是无穷），得到 %v", *z.RefundRate)
	}
	if z.AvgOrderValueCents != 0 || z.NetSalesCents != -50 {
		t.Errorf("没有买家时客单价 0、净销售额可以为负：得到 %d / %d", z.AvgOrderValueCents, z.NetSalesCents)
	}
}
