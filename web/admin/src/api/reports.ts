// 经营概览看板的界面规则：筛选怎么变成 query、环比怎么算、比率怎么显示、折线图的几何。
// **全部是纯函数**，由 `make admin-test` 用 `node --test` 直接跑（reports.test.ts）。
//
// 刻意只有 `import type`（运行时被类型剥离抹掉）：与 geo.ts / money.ts / orderRules.ts
// 同一个约定，这一步不需要 node_modules。
//
// ## 口径不在这里
//
// 销售额、退款、客单价、退款率、「今天」从几点算起……全部由服务端按契约
// （ReportWindow / ReportMetrics）算好，界面只展示。这里唯一「算」的是环比 ——
// 它是两个服务端数字之间的比，不引入任何新口径。
//
// ## 图表为什么是手写的 SVG
//
// 看板上只有一张折线（趋势）和一组横条（门店对比），几何就是下面这几十行。
// 引一个图表库（echarts 按需引入也要 300 KB 上下的 gzip 前体积）换来的是
// 一套要单独学的配置 DSL 和一份要跟着升级的依赖；手写 SVG 的体积增量是 0，
// 颜色、字体、悬停提示跟 Element Plus 的界面是同一套。

import type { components } from "@contract/schema.js";

type S = components["schemas"];
export type ReportPeriod = S["ReportWindow"]["period"];

export const PERIOD_OPTIONS: { value: ReportPeriod; label: string }[] = [
    { value: "today", label: "今日" },
    { value: "yesterday", label: "昨日" },
    { value: "last_7_days", label: "近 7 天" },
    { value: "last_30_days", label: "近 30 天" },
    { value: "custom", label: "自定义" },
];

/** 上一周期叫什么（环比那一行的说明）。与契约 ReportWindow 的「上一周期」逐条对应。 */
export function previousLabel(period: ReportPeriod): string {
    switch (period) {
        case "today":
            return "较昨日同时段";
        case "yesterday":
            return "较前日";
        case "last_7_days":
            return "较前 7 天";
        case "last_30_days":
            return "较前 30 天";
        default:
            return "较上一周期";
    }
}

export interface ReportFilterForm {
    period: ReportPeriod;
    /** 自定义时的 [起, 止]，YYYY-MM-DD，都含。 */
    dateRange?: [string, string] | null;
    storeId?: number | null;
}

export interface ReportQuery {
    period: ReportPeriod;
    start_date?: string;
    end_date?: string;
    store_id?: number;
}

/**
 * 筛选 → query。自定义但没选日期时返回 null（调用方不发请求）：发出去是一个注定的 422，
 * 而用户只是还没选完。
 */
export function reportQuery(f: ReportFilterForm): ReportQuery | null {
    const q: ReportQuery = { period: f.period };
    if (f.period === "custom") {
        if (f.dateRange === undefined || f.dateRange === null) return null;
        q.start_date = f.dateRange[0];
        q.end_date = f.dateRange[1];
    }
    if (f.storeId !== undefined && f.storeId !== null) q.store_id = f.storeId;
    return q;
}

/** 与服务端的 366 天上限同一个数（契约 ReportEndDate）。界面上提前拦，服务端仍会 422。 */
export const MAX_CUSTOM_DAYS = 366;

/** 两个 YYYY-MM-DD 之间含首尾共几天。按 UTC 解析，与时区无关。 */
export function inclusiveDays(start: string, end: string): number {
    const a = Date.parse(`${start}T00:00:00Z`);
    const b = Date.parse(`${end}T00:00:00Z`);
    return Math.round((b - a) / 86_400_000) + 1;
}

export interface Delta {
    /** 变化的百分比（+12.5 表示涨 12.5%）。上一周期是 0 时没有意义，为 null。 */
    pct: number | null;
    dir: "up" | "down" | "flat";
}

/**
 * 环比。上一周期是 0 时 pct 为 null（「从 0 涨到 100」不是一个百分比），方向仍然给出。
 * 退款这类「涨了是坏事」的指标，好坏由调用方决定颜色，这里只说方向。
 */
export function delta(current: number, previous: number): Delta {
    const dir = current > previous ? "up" : current < previous ? "down" : "flat";
    if (previous === 0) return { pct: null, dir };
    return { pct: ((current - previous) / Math.abs(previous)) * 100, dir };
}

/** 环比的文字：「↑ 12.5%」「↓ 3.0%」「持平」「新增」（上期为 0）。 */
export function deltaText(d: Delta): string {
    if (d.dir === "flat") return "持平";
    const arrow = d.dir === "up" ? "↑" : "↓";
    if (d.pct === null) return d.dir === "up" ? `${arrow} 上期为 0` : arrow;
    return `${arrow} ${Math.abs(d.pct).toFixed(1)}%`;
}

/** 比率 → 「12.3%」；null（分母为 0）→「—」。 */
export function rateText(r: number | null | undefined): string {
    if (r === null || r === undefined) return "—";
    return `${(r * 100).toFixed(1)}%`;
}

/** 两个比率之间的变化，单位是百分点：「↑ 1.2 个百分点」。任一侧为 null 时给「—」。 */
export function rateDeltaText(cur: number | null, prev: number | null): string {
    if (cur === null || prev === null) return "—";
    const pp = (cur - prev) * 100;
    if (Math.abs(pp) < 0.05) return "持平";
    return `${pp > 0 ? "↑" : "↓"} ${Math.abs(pp).toFixed(1)} 个百分点`;
}

// ---------------------------------------------------------------------------
// 折线图几何
// ---------------------------------------------------------------------------

/**
 * 纵轴刻度：从 0 到不小于 max 的一个「整」上界，分成 count 格，每格是 1 / 2 / 5 × 10^n。
 * max ≤ 0 时给 [0, 1, …, count]，免得一条全零的线贴在一个高度为 0 的轴上。
 */
export function niceTicks(max: number, count = 4): number[] {
    if (!(max > 0)) return Array.from({ length: count + 1 }, (_, i) => i);
    const raw = max / count;
    const mag = 10 ** Math.floor(Math.log10(raw));
    const step = [1, 2, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? 10 * mag;
    return Array.from({ length: count + 1 }, (_, i) => i * step);
}

export interface PlotBox {
    width: number;
    height: number;
    left: number;
    right: number;
    top: number;
    bottom: number;
}

/** 第 i 个点的横坐标：n 个点均匀铺满绘图区；只有一个点时放正中。 */
export function xAt(i: number, n: number, box: PlotBox): number {
    const w = box.width - box.left - box.right;
    if (n <= 1) return box.left + w / 2;
    return box.left + (w * i) / (n - 1);
}

/** 值 v 的纵坐标（0 在底边，top 在上边）。 */
export function yAt(v: number, top: number, box: PlotBox): number {
    const h = box.height - box.top - box.bottom;
    return box.top + h - (top > 0 ? (v / top) * h : 0);
}

/** 一条折线的 SVG path。负值（净销售额可以为负）钳到 0 以下的底边：纵轴从 0 起。 */
export function linePath(values: number[], top: number, box: PlotBox): string {
    return values
        .map((v, i) => `${i === 0 ? "M" : "L"}${xAt(i, values.length, box).toFixed(1)},${yAt(Math.max(v, 0), top, box).toFixed(1)}`)
        .join(" ");
}

/** 鼠标横坐标 → 最近的那个点的下标（悬停十字线用）。 */
export function nearestIndex(x: number, n: number, box: PlotBox): number {
    if (n <= 1) return 0;
    const w = box.width - box.left - box.right;
    const i = Math.round(((x - box.left) / w) * (n - 1));
    return Math.min(n - 1, Math.max(0, i));
}

/** 轴上的金额（分）→ 简写：「¥1.2万」「¥350」。只用于刻度，精确值在提示框里。 */
export function compactYuan(cents: number): string {
    const yuan = cents / 100;
    if (Math.abs(yuan) >= 10_000) return `¥${(yuan / 10_000).toFixed(yuan % 10_000 === 0 ? 0 : 1)}万`;
    return `¥${Math.round(yuan)}`;
}
