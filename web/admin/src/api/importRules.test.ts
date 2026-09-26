// 批量导入页界面规则的单元测试。`node --test src/api/importRules.test.ts` 直接跑，不要 node_modules。
import { test } from "node:test";
import assert from "node:assert/strict";
import type { components } from "@contract/schema.js";
import {
    canCommit,
    commitChoices,
    commitSummary,
    initialSelections,
    rowTone,
    specText,
    violationsOf,
} from "./importRules.ts";

type S = components["schemas"];

function product(first: number, rows: number[], status: S["ProductImportCategoryDecision"]["status"],
    importable = true, categoryId?: number): S["ProductImportProduct"] {
    return {
        first_row: first,
        rows,
        title: `商品${first}`,
        importable,
        category: {
            status,
            candidates: [{ category_id: 99, path_name: "候选", score: 0.9 }],
            ...(categoryId === undefined ? {} : { category_id: categoryId }),
        },
    };
}

function preview(products: S["ProductImportProduct"][]): S["ProductImportPreview"] {
    return {
        file_sha256: "x",
        format: "csv",
        total_rows: 0,
        error_rows: 0,
        products,
        rows: [],
        notices: [],
        category_engine: "ok",
        category_gate: { min_score: 0.5, min_margin: 0.03 },
    };
}

const pv = preview([
    product(2, [2, 3], "recommended", true, 7),
    product(4, [4], "matched", true, 8),
    product(5, [5], "needs_review"),
    product(6, [6], "unavailable"),
    product(7, [7], "matched", false, 8),
]);

test("只预选 matched 与 recommended，需人工确认的不替商家选（哪怕候选第一个分数很高）", () => {
    assert.deepEqual(initialSelections(pv), { 2: 7, 4: 8, 5: null, 6: null, 7: 8 });
});

test("确认时带回推荐的类目，跳过不可导入与没选类目的", () => {
    const sel = initialSelections(pv);
    assert.deepEqual(commitChoices(pv, sel), [
        { first_row: 2, category_id: 7 },
        { first_row: 4, category_id: 8 },
    ]);
    sel[5] = 3;
    assert.deepEqual(commitChoices(pv, sel).map((c) => c.first_row), [2, 4, 5]);
});

test("汇总与确认按钮", () => {
    const sel = initialSelections(pv);
    assert.deepEqual(commitSummary(pv, sel), { products: 2, skus: 3, blocked: 1, missingCategory: 2 });
    assert.equal(canCommit(pv, sel), true);
    const none = preview([product(2, [2], "needs_review"), product(3, [3], "matched", false, 1)]);
    assert.equal(canCommit(none, initialSelections(none)), false);
});

test("行底色：错误优先于提示，违禁词算提示", () => {
    const base: S["ProductImportRow"] = {
        row: 2, first_row: 2, sku_code: "A", spec_values: {}, errors: [], warnings: [], violations: [],
    };
    assert.equal(rowTone(base), "");
    assert.equal(rowTone({ ...base, violations: [{ field: "title", offset: 0, length: 2 }] }), "warning");
    assert.equal(rowTone({ ...base, warnings: [{ code: "price_zero", message: "" }] }), "warning");
    assert.equal(rowTone({ ...base, errors: [{ code: "required", message: "" }], warnings: [{ code: "x", message: "" }] }), "error");
});

test("规格与违禁词的小工具", () => {
    assert.equal(specText({}), "—");
    assert.equal(specText({ 颜色: "红", 尺码: "M" }), "颜色:红 / 尺码:M");
    const row: S["ProductImportRow"] = {
        row: 2, first_row: 2, sku_code: "A", spec_values: {}, errors: [], warnings: [],
        violations: [{ field: "subtitle", offset: 2, length: 2 }, { field: "title", offset: 0, length: 1 }],
    };
    assert.equal(violationsOf(row, "subtitle").length, 1);
    assert.equal(violationsOf(row, "description").length, 0);
});
