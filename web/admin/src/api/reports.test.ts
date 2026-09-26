// 经营概览看板的界面规则。`node --test src/api/reports.test.ts` 直接跑，不要 node_modules。
//
// 守的是「看错了数」那一类：上期为 0 时算出一个 Infinity%、自定义没选完就发出 422、
// 退款率的变化被写成百分比而不是百分点、刻度把最大值截在轴外。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    compactYuan,
    delta,
    deltaText,
    filenameFromDisposition,
    inclusiveDays,
    linePath,
    nearestIndex,
    niceTicks,
    previousLabel,
    rateDeltaText,
    rateText,
    reportCsvPath,
    reportQuery,
    xAt,
    yAt,
    type PlotBox,
} from "./reports.ts";

test("筛选 → query：自定义没选完不发请求；门店只在选了时带上", () => {
    assert.deepEqual(reportQuery({ period: "today" }), { period: "today" });
    assert.equal(reportQuery({ period: "custom" }), null);
    assert.equal(reportQuery({ period: "custom", dateRange: null }), null);
    assert.deepEqual(reportQuery({ period: "custom", dateRange: ["2026-09-01", "2026-09-10"], storeId: 3 }), {
        period: "custom",
        start_date: "2026-09-01",
        end_date: "2026-09-10",
        store_id: 3,
    });
    // 非自定义时日期不带（服务端也不读，但带上会让人以为它生效了）。
    assert.deepEqual(reportQuery({ period: "last_7_days", dateRange: ["2026-09-01", "2026-09-10"], storeId: null }), {
        period: "last_7_days",
    });
});

test("自定义天数含首尾，366 天是一整个闰年", () => {
    assert.equal(inclusiveDays("2026-09-01", "2026-09-01"), 1);
    assert.equal(inclusiveDays("2024-01-01", "2024-12-31"), 366);
    assert.equal(inclusiveDays("2024-01-01", "2025-01-01"), 367);
});

test("环比：上期为 0 时没有百分比，只有方向", () => {
    assert.deepEqual(delta(150, 100), { pct: 50, dir: "up" });
    assert.deepEqual(delta(50, 100), { pct: -50, dir: "down" });
    assert.deepEqual(delta(0, 0), { pct: null, dir: "flat" });
    assert.deepEqual(delta(100, 0), { pct: null, dir: "up" });
    // 净销售额可以为负：分母取绝对值，方向仍按大小。
    assert.deepEqual(delta(-50, -100), { pct: 50, dir: "up" });
    assert.equal(deltaText(delta(150, 100)), "↑ 50.0%");
    assert.equal(deltaText(delta(97, 100)), "↓ 3.0%");
    assert.equal(deltaText(delta(100, 0)), "↑ 上期为 0");
    assert.equal(deltaText(delta(5, 5)), "持平");
});

test("比率：null 显示为「—」，变化按百分点", () => {
    assert.equal(rateText(null), "—");
    assert.equal(rateText(0.2848), "28.5%");
    assert.equal(rateDeltaText(0.3, 0.25), "↑ 5.0 个百分点");
    assert.equal(rateDeltaText(0.25, 0.3), "↓ 5.0 个百分点");
    assert.equal(rateDeltaText(null, 0.3), "—");
    assert.equal(previousLabel("today"), "较昨日同时段");
});

test("纵轴刻度：上界不小于最大值，步长是 1/2/5 × 10^n；全零时不塌", () => {
    for (const max of [1, 7, 99, 101, 12345, 790000]) {
        const ticks = niceTicks(max);
        assert.equal(ticks.length, 5);
        assert.ok(ticks[4]! >= max, `max ${max} → ${ticks.join(",")}`);
        const step = ticks[1]!;
        const lead = step / 10 ** Math.floor(Math.log10(step));
        assert.ok([1, 2, 5].includes(Math.round(lead)), `step ${step}`);
    }
    assert.deepEqual(niceTicks(0), [0, 1, 2, 3, 4]);
});

test("折线几何：两端贴边、0 在底边、负值钳到底边、悬停取最近的点", () => {
    const box: PlotBox = { width: 110, height: 60, left: 10, right: 0, top: 0, bottom: 10 };
    assert.equal(xAt(0, 3, box), 10);
    assert.equal(xAt(2, 3, box), 110);
    assert.equal(xAt(0, 1, box), 60);
    assert.equal(yAt(0, 100, box), 50);
    assert.equal(yAt(100, 100, box), 0);
    assert.equal(linePath([0, 100, -5], 100, box), "M10.0,50.0 L60.0,0.0 L110.0,50.0");
    assert.equal(nearestIndex(10, 3, box), 0);
    assert.equal(nearestIndex(84, 3, box), 1);
    assert.equal(nearestIndex(86, 3, box), 2);
    assert.equal(nearestIndex(500, 3, box), 2);
});

test("刻度上的金额简写", () => {
    assert.equal(compactYuan(35000), "¥350");
    assert.equal(compactYuan(1_000_000), "¥1万");
    assert.equal(compactYuan(1_250_000), "¥1.3万");
});

test("CSV 导出：地址与界面那张表的查询逐字相同，空参数不带", () => {
    assert.equal(reportCsvPath("stores", { period: "today" }), "/admin/reports/stores.csv?period=today");
    assert.equal(
        reportCsvPath("products", { period: "custom", start_date: "2026-09-01", end_date: "2026-09-30", sort_by: "quantity", limit: 10, store_id: undefined }),
        "/admin/reports/products.csv?period=custom&start_date=2026-09-01&end_date=2026-09-30&sort_by=quantity&limit=10",
    );
    assert.equal(reportCsvPath("products", {}), "/admin/reports/products.csv");
});

test("CSV 导出：文件名优先取 filename*（中文），其次 filename，都没有用兜底", () => {
    const h = "attachment; filename=\"product-ranking-2026-09-01-2026-09-30.csv\"; filename*=UTF-8''%E5%95%86%E5%93%81%E6%8E%92%E8%A1%8C_2026-09-01_2026-09-30.csv";
    assert.equal(filenameFromDisposition(h, "x.csv"), "商品排行_2026-09-01_2026-09-30.csv");
    assert.equal(filenameFromDisposition('attachment; filename="a.csv"', "x.csv"), "a.csv");
    assert.equal(filenameFromDisposition("attachment; filename*=UTF-8''%E0%A4%A", "x.csv"), "x.csv");
    assert.equal(filenameFromDisposition(null, "x.csv"), "x.csv");
});
