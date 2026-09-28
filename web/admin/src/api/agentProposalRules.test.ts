// AI 员工提案界面规则的单元测试。`node --test src/api/agentProposalRules.test.ts` 直接跑。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    KIND_LABEL,
    VERDICT,
    couponPayload,
    describeOutcome,
    describeResult,
    flashPricePayload,
    inventoryAdjustPayload,
    productCopyPayload,
    refundDecisionPayload,
    safeRatePercent,
} from "./agentProposalRules.ts";

test("提案种类标签：五种都有，且不撞", () => {
    assert.deepEqual(Object.keys(KIND_LABEL).sort(), [
        "coupon",
        "flash_price",
        "inventory_adjust",
        "product_copy",
        "refund_decision",
    ]);
    assert.equal(new Set(Object.values(KIND_LABEL)).size, 5);
});

test("verdict 标签三档颜色不同", () => {
    assert.equal(VERDICT.positive.tag, "success");
    assert.equal(VERDICT.neutral.tag, "info");
    assert.equal(VERDICT.negative.tag, "danger");
});

test("加库存 payload：件数带正负号", () => {
    assert.equal(inventoryAdjustPayload({ store_id: 3, sku_id: 12, delta: 40, reason: "补货" }).deltaText, "+40 件");
    assert.equal(inventoryAdjustPayload({ store_id: 3, sku_id: 12, delta: -5 }).deltaText, "-5 件");
    assert.equal(inventoryAdjustPayload({}).storeText, "—");
});

test("限时折扣 payload：折扣率换算成折，全店 / 门店范围", () => {
    const v = flashPricePayload({
        name: "夏促",
        items: [{ sku_id: 1, discount_rate: 850 }, { sku_id: 2, discount_rate: 500 }],
        starts_at: "2026-10-01T00:00:00Z",
        ends_at: "2026-10-07T23:59:59Z",
    });
    assert.equal(v.storeText, "全店");
    assert.deepEqual(v.items, [
        { skuId: 1, discountText: "8.5 折" },
        { skuId: 2, discountText: "5 折" },
    ]);
    assert.equal(v.startsAt, "2026-10-01T00:00:00Z");

    const scoped = flashPricePayload({ store_id: 7, items: [] });
    assert.equal(scoped.storeText, "门店 #7");
});

test("发券 payload：满减 / 折扣（带封顶）/ 立减", () => {
    const full = couponPayload({
        name: "国庆券",
        coupon_type: 1,
        threshold_cents: 10000,
        discount_cents: 2000,
        valid_days: 7,
        total_count: 500,
        per_user_limit: 1,
        claimable: true,
    });
    assert.equal(full.typeText, "满减");
    assert.equal(full.thresholdText, "满 ¥100");
    assert.equal(full.faceText, "¥20");
    assert.equal(full.claimableText, "进领券中心");

    const rate = couponPayload({
        name: "折扣券",
        coupon_type: 2,
        threshold_cents: 0,
        discount_rate: 850,
        max_discount_cents: 5000,
        valid_days: 30,
        total_count: 100,
        per_user_limit: 1,
        claimable: false,
    });
    assert.equal(rate.thresholdText, "无门槛");
    assert.equal(rate.faceText, "8.5 折，封顶 ¥50");
    assert.equal(rate.claimableText, "不进领券中心");
});

test("改文案 payload：标题 / 副标题改了哪个就标哪个", () => {
    const titleOnly = productCopyPayload({ product_id: 9, title: "新标题", before_title: "旧标题" });
    assert.equal(titleOnly.titleChanged, true);
    assert.equal(titleOnly.subtitleChanged, false);
    assert.equal(titleOnly.beforeTitle, "旧标题");
    assert.equal(titleOnly.afterTitle, "新标题");
});

test("售后审核 payload：同意 / 驳回", () => {
    assert.equal(refundDecisionPayload({ refund_no: "RF1", action: "approve", amount_cents: 100 }).actionText, "同意");
    const rejected = refundDecisionPayload({ refund_no: "RF2", action: "reject", reject_reason: "已超过退货期", amount_cents: 200 });
    assert.equal(rejected.actionText, "驳回");
    assert.equal(rejected.reason, "已超过退货期");
    assert.equal(rejected.amountText, "¥2");
});

test("结果：加库存是 before/after，不落进 lines", () => {
    const v = describeResult("inventory_adjust", { before_available: 10, after_available: 50 });
    assert.deepEqual(v.beforeAfter, { before: 10, after: 50 });
    assert.deepEqual(v.lines, []);
});

test("结果：M10 各种类落进 detail.xxx_id，给列表页入口", () => {
    const flash = describeResult("flash_price", { detail: { promotion_id: 88 } });
    assert.equal(flash.hasPromotionLink, true);
    assert.equal(flash.lines[0]?.value, "#88");

    const coupon = describeResult("coupon", { detail: { coupon_template_id: 12 } });
    assert.equal(coupon.hasCouponLink, true);

    const copy = describeResult("product_copy", { detail: { before_title: "旧", after_title: "新" } });
    assert.equal(copy.lines[0]?.value, "旧 → 新");

    const refund = describeResult("refund_decision", { detail: { refund_no: "RF9", refund_status: 40 } });
    assert.match(refund.lines[0]?.value ?? "", /RF9/);
});

test("结果：失败态不落进 lines，走 failure", () => {
    const v = describeResult("coupon", { error_type: "invalid-request", error: "券服务没有接上" });
    assert.equal(v.failure?.error, "券服务没有接上");
    assert.deepEqual(v.lines, []);
});

test("结果：还没有 result 时给空", () => {
    const v = describeResult("coupon", undefined);
    assert.deepEqual(v, { lines: [], hasPromotionLink: false, hasCouponLink: false });
});

test("复盘：verdict + explanation + 认识的指标", () => {
    const o = describeOutcome({ verdict: "positive", explanation: "补货后没断货", sold_qty: 12, stockout_days: 0, delta_qty: 40 });
    assert.equal(o.verdict, "positive");
    assert.equal(o.explanation, "补货后没断货");
    assert.deepEqual(o.metrics, [
        { label: "期间销量", value: "12" },
        { label: "断货天数", value: "0 天" },
        { label: "补货件数", value: "40 件" },
    ]);
});

test("复盘：核销率格式化成百分比、销售额格式化成元", () => {
    const o = describeOutcome({ verdict: "neutral", use_rate: 0.256, amount_during_cents: 123456 });
    assert.deepEqual(o.metrics, [
        { label: "期间销售额", value: "¥1234.56" },
        { label: "核销率", value: "25.6%" },
    ]);
});

test("复盘：还没到点（没有 outcome）时给空", () => {
    assert.deepEqual(describeOutcome(undefined), { metrics: [] });
});

test("安全除法百分比：分母为 0 给 —，不给 0% / NaN", () => {
    assert.equal(safeRatePercent(0, 0), "—");
    assert.equal(safeRatePercent(3, 10), "30%");
    assert.equal(safeRatePercent(1, 3), "33%");
});
