// 批量导入页的界面规则（契约 /admin/product-imports）：类目怎么预选、确认时带什么、
// 哪一行标红、按钮什么时候能点。
//
// 刻意只有 `import type`：`make admin-test` 用 `node --test` 直接跑这个文件的测试
// （Node 的类型剥离），不要 node_modules —— 与 geo.ts / money.ts / orderRules.ts 同一个约定。
// 别往这里加运行时 import。

import type { components } from "@contract/schema.js";

type S = components["schemas"];
type Preview = S["ProductImportPreview"];
type Product = S["ProductImportProduct"];
type Row = S["ProductImportRow"];
type Choice = S["ProductImportCategoryChoice"];
type DecisionStatus = S["ProductImportCategoryDecision"]["status"];

/** 每件商品（按首行行号）选定的类目；null 表示还没选。 */
export type Selections = Record<number, number | null>;

/**
 * 预检回来之后的初始选择。
 *
 * matched 与 recommended 预先选上（后者是「服务端判定足够可信」的推荐，契约里它就
 * 带着 category_id）；needs_review 与 unavailable **不替商家选** —— 哪怕候选列表
 * 第一个看着很像。那道门是离线评测定出来的，界面不该自己再开一道更松的门。
 */
export function initialSelections(pv: Preview): Selections {
    const out: Selections = {};
    for (const p of pv.products) {
        const d = p.category;
        out[p.first_row] = (d.status === "matched" || d.status === "recommended") && d.category_id !== undefined
            ? d.category_id
            : null;
    }
    return out;
}

/**
 * 确认导入时带的 categories。
 *
 * **推荐的类目要原样带回去**：服务端在确认这一步不调引擎、也不会自动采用推荐
 * （契约 POST /admin/product-imports 的描述）。只带 importable 且选了类目的商品；
 * matched 的也带上 —— 与服务端按名字匹配到的是同一个值，带上让「确认时用的是哪个类目」
 * 在请求里一目了然，而且商家可能把它改成了别的。
 */
export function commitChoices(pv: Preview, sel: Selections): Choice[] {
    const out: Choice[] = [];
    for (const p of pv.products) {
        const id = sel[p.first_row];
        if (p.importable && id !== null && id !== undefined) {
            out.push({ first_row: p.first_row, category_id: id });
        }
    }
    return out.sort((a, b) => a.first_row - b.first_row);
}

export interface CommitSummary {
    /** 会被导入的商品数与 SKU 数。 */
    products: number;
    skus: number;
    /** 有错误、会被跳过的商品数。 */
    blocked: number;
    /** 没错误但还没选类目的商品数（确认时它们会 failed）。 */
    missingCategory: number;
}

/** 确认按钮上方那一行：会导入多少、跳过多少、还有几件没选类目。 */
export function commitSummary(pv: Preview, sel: Selections): CommitSummary {
    const s: CommitSummary = { products: 0, skus: 0, blocked: 0, missingCategory: 0 };
    for (const p of pv.products) {
        if (!p.importable) {
            s.blocked += 1;
            continue;
        }
        const id = sel[p.first_row];
        if (id === null || id === undefined) {
            s.missingCategory += 1;
            continue;
        }
        s.products += 1;
        s.skus += p.rows.length;
    }
    return s;
}

/** 至少有一件能导入才让点「确认导入」（否则服务端回 422 import-nothing-to-import）。 */
export function canCommit(pv: Preview, sel: Selections): boolean {
    return commitSummary(pv, sel).products > 0;
}

/** 预检表格里一行的底色：自身有错 → 红；被同组拖累或有提示 / 违禁词 → 黄；否则无。 */
export function rowTone(row: Row): "error" | "warning" | "" {
    if (row.errors.length > 0) return "error";
    if (row.warnings.length > 0 || row.violations.length > 0) return "warning";
    return "";
}

/** 类目决定的显示：文字 + Element Plus 的 tag 类型。 */
export const DECISION_TEXT: Record<DecisionStatus, { text: string; tag: "success" | "primary" | "warning" | "info" }> = {
    matched: { text: "按名称匹配", tag: "success" },
    recommended: { text: "AI 推荐（已选）", tag: "primary" },
    needs_review: { text: "需人工确认", tag: "warning" },
    unavailable: { text: "需手选", tag: "info" },
};

/** 余弦分数 → 「0.83」。只用于显示，不参与任何判断。 */
export function scoreText(score: number): string {
    return score.toFixed(2);
}

/** 规格对象 → 「颜色:红 / 尺码:M」；单规格商品显示「—」。 */
export function specText(spec: Record<string, string>): string {
    const parts = Object.entries(spec).map(([k, v]) => `${k}:${v}`);
    return parts.length > 0 ? parts.join(" / ") : "—";
}

/** 某件商品在表格里的第一行：类目选择框只画在这一行上。 */
export function isFirstRowOfProduct(row: Row): boolean {
    return row.row === row.first_row;
}

/** 按首行行号找商品。 */
export function productOf(pv: Preview, firstRow: number): Product | undefined {
    return pv.products.find((p) => p.first_row === firstRow);
}

/** 某一行某个字段的违禁词命中（HighlightedText 要的形状就是 FieldError）。 */
export function violationsOf(row: Row, field: "title" | "subtitle" | "description"): S["FieldError"][] {
    return row.violations.filter((v) => v.field === field);
}
