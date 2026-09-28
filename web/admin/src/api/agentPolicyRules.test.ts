// AI 员工自动执行策略界面规则的单元测试。`node --test src/api/agentPolicyRules.test.ts` 直接跑。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    AUTO_POLICY_KINDS,
    capFieldOf,
    capInputText,
    capLabel,
    dailyLimitLabel,
    parseCap,
    withCap,
} from "./agentPolicyRules.ts";

const base = { enabled: true, max_units: 0, min_discount_rate: 0, max_discount_cents: 0, daily_limit: 0 };

test("四种可自动执行的种类，固定顺序", () => {
    assert.deepEqual(AUTO_POLICY_KINDS, ["inventory_adjust", "flash_price", "coupon", "product_copy"]);
});

test("单笔上限字段按种类：加库存件数 / 折扣不低于 / 券面额封顶 / 改文案没有", () => {
    assert.equal(capFieldOf("inventory_adjust"), "max_units");
    assert.equal(capFieldOf("flash_price"), "min_discount_rate");
    assert.equal(capFieldOf("coupon"), "max_discount_cents");
    assert.equal(capFieldOf("product_copy"), null);
});

test("单笔上限文案", () => {
    assert.equal(capLabel("inventory_adjust", { ...base, max_units: 40 }), "单笔至多加 40 件");
    assert.equal(capLabel("flash_price", { ...base, min_discount_rate: 850 }), "折扣不低于 8.5 折");
    assert.equal(capLabel("coupon", { ...base, max_discount_cents: 5000 }), "面额不超过 ¥50");
    assert.equal(capLabel("product_copy", base), "没有单笔上限");
});

test("条数上限：0 = 不自动执行", () => {
    assert.equal(dailyLimitLabel(0), "0（不自动执行）");
    assert.equal(dailyLimitLabel(5), "每 24 小时至多 5 条");
});

test("解析单笔上限输入：件数、折扣（不低于五折）、面额", () => {
    assert.deepEqual(parseCap("inventory_adjust", "40"), { ok: true, value: 40 });
    assert.equal(parseCap("inventory_adjust", "-1").ok, false);
    assert.equal(parseCap("inventory_adjust", "abc").ok, false);

    assert.deepEqual(parseCap("flash_price", "8.5"), { ok: true, value: 850 });
    assert.equal(parseCap("flash_price", "4").ok, false, "比五折还低，界面先挡一遍");
    assert.equal(parseCap("flash_price", "5").ok, true);

    assert.deepEqual(parseCap("coupon", "50"), { ok: true, value: 5000 });
    assert.equal(parseCap("coupon", "0").ok, false);

    assert.equal(parseCap("product_copy", "1").ok, false);
});

test("回显与解析互逆", () => {
    const p1 = withCap("inventory_adjust", base, 40);
    assert.equal(capInputText("inventory_adjust", p1), "40");
    const p2 = withCap("flash_price", base, 850);
    assert.equal(capInputText("flash_price", p2), "8.5");
    const p3 = withCap("coupon", base, 5000);
    assert.equal(capInputText("coupon", p3), "50");
});

test("withCap 只改相关字段，其余原样保留", () => {
    const seeded = { ...base, max_units: 99, daily_limit: 3, enabled: false };
    const out = withCap("flash_price", seeded, 900);
    assert.equal(out.min_discount_rate, 900);
    assert.equal(out.max_units, 99, "改折扣上限不该动加库存的件数上限");
    assert.equal(out.daily_limit, 3);
    assert.equal(out.enabled, false);
});
