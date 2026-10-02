// AI 员工提案界面规则的单元测试。`node --test src/api/agentProposalRules.test.ts` 直接跑。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    KIND_LABEL,
    VERDICT,
    channelStockRulePayload,
    couponPayload,
    describeOutcome,
    describeResult,
    flashPricePayload,
    inventoryAdjustPayload,
    productCopyPayload,
    refundDecisionPayload,
    safeRatePercent,
} from "./agentProposalRules.ts";

test("提案种类标签：六种都有，且不撞", () => {
    assert.deepEqual(Object.keys(KIND_LABEL).sort(), [
        "channel_stock_rule",
        "coupon",
        "flash_price",
        "inventory_adjust",
        "product_copy",
        "refund_decision",
    ]);
    assert.equal(new Set(Object.values(KIND_LABEL)).size, 6);
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
    assert.equal(full.validDaysText, "领后 7 天内有效");
    const fixed = couponPayload({
        name: "节日券",
        coupon_type: 3,
        discount_cents: 300,
        valid_start_at: "2026-10-01T00:00:00+08:00",
        valid_end_at: "2026-10-08T00:00:00+08:00",
        total_count: 10,
        per_user_limit: 1,
    });
    assert.ok(fixed.validDaysText.includes(" 至 ") && fixed.validDaysText.endsWith("可用"), fixed.validDaysText);
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

test("调渠道分配 payload：门店级 / SKU 级改动、旧规则→新规则、试算前后可售", () => {
    const v = channelStockRulePayload({
        binding_id: 5,
        binding_name: "美团外卖",
        store_id: 3,
        store_name: "示例小店",
        changes: [
            { sku_id: 12, ratio_bp: 9000, safety_qty: 2, prev: { ratio_bp: 8000, safety_qty: 5, level: "sku" } },
            { ratio_bp: 7000, safety_qty: 3, cap_qty: 50, prev: { ratio_bp: 6000, safety_qty: 3, level: "store" } },
        ],
        preview: [{ sku_id: 12, available: 20, before_qty: 16, after_qty: 18 }],
    });
    assert.equal(v.bindingText, "美团外卖");
    assert.equal(v.storeText, "示例小店");
    assert.equal(v.changes[0]?.cellText, "SKU #12");
    assert.equal(v.changes[0]?.prevText, "比例 80% · 安全库存 5 件 · 不封顶");
    assert.equal(v.changes[0]?.nextText, "比例 90% · 安全库存 2 件 · 不封顶");
    assert.equal(v.changes[1]?.cellText, "门店级");
    assert.equal(v.changes[1]?.nextText, "比例 70% · 安全库存 3 件 · 封顶 50");
    assert.equal(v.preview[0]?.skuText, "SKU #12");
    assert.equal(v.preview[0]?.beforeText, "16 件");
    assert.equal(v.preview[0]?.afterText, "18 件");
});

test("调渠道分配 payload：缺字段 / 没有 binding_name 时退到 id 或 —", () => {
    const v = channelStockRulePayload({ binding_id: 5, store_id: 3, changes: [], preview: [] });
    assert.equal(v.bindingText, "渠道 #5");
    assert.equal(v.storeText, "门店 #3");
    assert.equal(channelStockRulePayload({}).bindingText, "—");
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

    const channelRule = describeResult("channel_stock_rule", { detail: { binding_id: 5, store_id: 3, applied: 2, already: 1 } });
    assert.equal(channelRule.lines[0]?.value, "2 格（另有 1 格本来就是目标值）");
    const channelRuleNoAlready = describeResult("channel_stock_rule", { detail: { binding_id: 5, store_id: 3, applied: 2, already: 0 } });
    assert.equal(channelRuleNoAlready.lines[0]?.value, "2 格");
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
    const o = describeOutcome("inventory_adjust", { verdict: "positive", explanation: "补货后没断货", sold_qty: 12, stockout_days: 0, delta_qty: 40 });
    assert.equal(o.verdict, "positive");
    assert.equal(o.explanation, "补货后没断货");
    assert.deepEqual(o.metrics, [
        { label: "期间销量", value: "12" },
        { label: "断货天数", value: "0 天" },
        { label: "补货件数", value: "40 件" },
    ]);
    assert.deepEqual(o.cells, []);
});

test("复盘：核销率格式化成百分比、销售额格式化成元", () => {
    const o = describeOutcome("coupon", { verdict: "neutral", use_rate: 0.256, amount_during_cents: 123456 });
    assert.deepEqual(o.metrics, [
        { label: "期间销售额", value: "¥1234.56" },
        { label: "核销率", value: "25.6%" },
    ]);
});

test("复盘：还没到点（没有 outcome）时给空", () => {
    assert.deepEqual(describeOutcome("coupon", undefined), { metrics: [], cells: [] });
});

test("复盘：调渠道分配走逐格对比，不走通用指标列表", () => {
    const o = describeOutcome("channel_stock_rule", {
        verdict: "negative",
        explanation: "SKU 12 的缺货拒单从 0 件增加到 3 件",
        window_start: "2026-09-26T00:00:00Z",
        window_end: "2026-10-03T00:00:00Z",
        comparison_start: "2026-09-19T00:00:00Z",
        cells: [
            {
                binding_id: 5,
                store_id: 3,
                sku_id: 12,
                direction: "up",
                before: { held_zero_hours: 40, empty_zero_hours: 0, stockout_rejects: 0, sold: 30, net_cents: 12000 },
                after: { held_zero_hours: 5, empty_zero_hours: 0, stockout_rejects: 3, sold: 45, net_cents: 18000 },
            },
            {
                binding_id: 5,
                store_id: 3,
                sku_id: 13,
                direction: "same",
                before: { held_zero_hours: 0, empty_zero_hours: 60, stockout_rejects: 0, sold: 10, net_cents: 4000 },
                after: { held_zero_hours: 0, empty_zero_hours: 60, stockout_rejects: 0, sold: 10, net_cents: 4000 },
                excluded_reason: "窗口里 keel 自己断货超过 2 天：缺的是货，不是分配",
            },
        ],
    });
    assert.equal(o.verdict, "negative");
    assert.deepEqual(o.metrics, []);
    assert.equal(o.cells.length, 2);
    assert.equal(o.cells[0]?.skuText, "SKU #12");
    assert.equal(o.cells[0]?.directionText, "上调");
    assert.equal(o.cells[0]?.heldZeroText, "40 小时 → 5 小时");
    assert.equal(o.cells[0]?.rejectsText, "0 → 3");
    assert.equal(o.cells[0]?.soldText, "30 → 45");
    assert.equal(o.cells[0]?.netText, "¥120 → ¥180");
    assert.equal(o.cells[0]?.excludedReason, undefined);
    assert.equal(o.cells[1]?.directionText, "不变");
    assert.equal(o.cells[1]?.excludedReason, "窗口里 keel 自己断货超过 2 天：缺的是货，不是分配");
});

test("安全除法百分比：分母为 0 给 —，不给 0% / NaN", () => {
    assert.equal(safeRatePercent(0, 0), "—");
    assert.equal(safeRatePercent(3, 10), "30%");
    assert.equal(safeRatePercent(1, 3), "33%");
});
