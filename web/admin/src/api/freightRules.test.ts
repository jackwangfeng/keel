// 运费模板编辑页纯逻辑的单元测试。`node --test src/api/freightRules.test.ts` 直接跑，不要 node_modules。
import { test } from "node:test";
import assert from "node:assert/strict";
import { checkTemplateDraft, PROVINCES, provinceName, regionsLabel, ruleSummary, takenElsewhere, type RuleLike } from "./freightRules.ts";

const rule = (codes: string[], over: Partial<RuleLike> = {}): RuleLike => ({
    region_codes: codes,
    first_unit: 1,
    first_fee_cents: 800,
    additional_unit: 1,
    additional_fee_cents: 200,
    free_threshold_cents: 0,
    free_quantity: 0,
    ...over,
});

test("34 个省级行政区，码唯一且都是 6 位、后四位为 0", () => {
    assert.equal(PROVINCES.length, 34);
    assert.equal(new Set(PROVINCES.map((p) => p.code)).size, 34);
    for (const p of PROVINCES) assert.match(p.code, /^[1-9]\d0000$/);
    assert.equal(provinceName("650000"), "新疆");
    assert.equal(provinceName("999999"), "999999");
});

test("规则一句话：按件 / 按重量 / 包邮条件", () => {
    assert.equal(ruleSummary(1, rule([], { free_threshold_cents: 9900 })), "首件 8 元，每续 1 件 2 元；满 99 元包邮");
    assert.equal(
        ruleSummary(2, rule([], { first_unit: 1000, first_fee_cents: 1000, additional_unit: 500, additional_fee_cents: 300 })),
        "首重 1 kg 10 元，每续重 500 g 3 元",
    );
    assert.equal(ruleSummary(1, rule([], { free_quantity: 3, free_threshold_cents: 5000 })), "首件 8 元，每续 1 件 2 元；满 50 元包邮或满 3 件包邮");
    assert.equal(regionsLabel([]), "其余地区（默认）");
    assert.equal(regionsLabel(["650000", "540000"]), "新疆、西藏");
});

test("本地校验与服务端同一组规矩", () => {
    assert.equal(checkTemplateDraft([rule([]), rule(["650000"])], ["810000"]), null);
    assert.match(checkTemplateDraft([rule(["650000"])], []) ?? "", /默认规则/);
    assert.match(checkTemplateDraft([rule([]), rule([])], []) ?? "", /只能有一条/);
    assert.match(checkTemplateDraft([rule([]), rule(["650000"]), rule(["650000"])], []) ?? "", /新疆同时出现在/);
    assert.match(checkTemplateDraft([rule([]), rule(["810000"])], ["810000"]) ?? "", /香港同时出现在「第 2 条规则」与「不配送地区」/);
    assert.match(checkTemplateDraft([rule([], { additional_unit: 0 })], []) ?? "", /至少是 1/);
});

test("别处已占用的省：给多选框置灰用", () => {
    const rules = [rule([]), rule(["650000"]), rule(["540000"])];
    assert.deepEqual([...takenElsewhere(rules, ["810000"], 1)].sort(), ["540000", "810000"]);
    assert.deepEqual([...takenElsewhere(rules, ["810000"], "undeliverable")].sort(), ["540000", "650000"]);
});
