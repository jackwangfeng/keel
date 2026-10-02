import { test } from "node:test";
import assert from "node:assert/strict";
import {
    bindingStatusLabel,
    bitsFromRoles,
    bpToPercent,
    channelProbeOutcome,
    channelsAvailable,
    deepEqual,
    managedLabel,
    percentToBp,
    priceRuleBody,
    priceRuleSummary,
    roleOptions,
    rolesFromBits,
    rolesLabel,
    shopifyDomainOk,
    stockRuleBody,
    stockRuleLevel,
    validatePriceRule,
    validateStockRule,
} from "./channelRules.ts";

test("万分比 ↔ 百分数：往返不漂", () => {
    assert.equal(percentToBp(33.33), 3333);
    assert.equal(bpToPercent(3333), 33.33);
    assert.equal(percentToBp(-15), -1500);
    assert.equal(bpToPercent(-1500), -15);
    assert.equal(percentToBp(100), 10000);
    assert.equal(percentToBp(0.07), 7);
    assert.equal(percentToBp(0.29), 29);
    for (let bp = -9000; bp <= 10000; bp += 37) assert.equal(percentToBp(bpToPercent(bp)), bp);
});

test("固定价 12.30 元 ↔ 1230 分", () => {
    assert.equal(priceRuleBody({ skuId: 9, markupPercent: 0, fixedYuan: "12.30" }).fixed_cents, 1230);
    assert.equal(priceRuleSummary({ markup_bp: 0, fixed_cents: 1230 }), "固定价 ¥12.30");
    assert.equal(priceRuleSummary({ markup_bp: 1500, fixed_cents: null }), "加价 +15%");
    assert.equal(priceRuleSummary({ markup_bp: -1500 }), "加价 -15%");
});

test("状态与角色的文字", () => {
    assert.deepEqual(bindingStatusLabel(1), { text: "启用", type: "success" });
    assert.deepEqual(bindingStatusLabel(2), { text: "停用", type: "info" });
    assert.deepEqual(bindingStatusLabel(3), { text: "凭据失效", type: "danger" });
    assert.equal(rolesLabel(5), "商品源、销售渠道");
    assert.equal(rolesLabel(7), "商品源、库存源、销售渠道");
    assert.equal(rolesLabel(0), "—");
    assert.deepEqual(roleOptions(5).map((o) => o.bit), [1, 4]);
    assert.equal(rolesFromBits([1, 4]), 5);
    assert.deepEqual(bitsFromRoles(6), [2, 4]);
});

test("库存规则校验", () => {
    const ok = { storeId: null, skuId: null, ratioPercent: 80, safetyQty: 0, capQty: null };
    assert.deepEqual(validateStockRule(ok), []);
    assert.equal(validateStockRule({ ...ok, skuId: 3 }).length, 1, "只给 SKU 不给门店");
    assert.deepEqual(validateStockRule({ ...ok, storeId: 2, skuId: 3 }), []);
    assert.equal(validateStockRule({ ...ok, ratioPercent: 100.5 }).length, 1, "比例 > 100");
    assert.equal(validateStockRule({ ...ok, ratioPercent: -1 }).length, 1);
    assert.equal(validateStockRule({ ...ok, ratioPercent: 33.333 }).length, 1, "超过两位小数");
    assert.equal(validateStockRule({ ...ok, safetyQty: -1 }).length, 1);
    assert.equal(validateStockRule({ ...ok, capQty: -2 }).length, 1);
    assert.deepEqual(validateStockRule({ ...ok, capQty: 0 }), []);
    assert.deepEqual(stockRuleBody({ ...ok, storeId: 2, ratioPercent: 33.33, safetyQty: 1, capQty: 5 }), {
        store_id: 2,
        sku_id: null,
        ratio_bp: 3333,
        safety_qty: 1,
        cap_qty: 5,
    });
    assert.equal(stockRuleLevel({ store_id: null, sku_id: null }), "渠道级");
    assert.equal(stockRuleLevel({ store_id: 2 }), "门店级");
    assert.equal(stockRuleLevel({ store_id: 2, sku_id: 3 }), "SKU 级");
});

test("价格规则校验：固定价只能配 SKU", () => {
    assert.deepEqual(validatePriceRule({ skuId: null, markupPercent: 15, fixedYuan: null }), []);
    assert.equal(validatePriceRule({ skuId: null, markupPercent: 0, fixedYuan: "12.30" }).length, 1);
    assert.deepEqual(validatePriceRule({ skuId: 7, markupPercent: 0, fixedYuan: "12.30" }), []);
    assert.equal(validatePriceRule({ skuId: 7, markupPercent: 0, fixedYuan: "0" }).length, 1, "固定价 0");
    assert.equal(validatePriceRule({ skuId: 7, markupPercent: 0, fixedYuan: "1.234" }).length, 1, "三位小数");
    assert.equal(validatePriceRule({ skuId: null, markupPercent: -95, fixedYuan: null }).length, 1, "加价低于 -90%");
    assert.deepEqual(validatePriceRule({ skuId: null, markupPercent: -15, fixedYuan: "  " }), [], "空白固定价当没填");
    assert.deepEqual(priceRuleBody({ skuId: null, markupPercent: -15, fixedYuan: "" }), {
        sku_id: null,
        markup_bp: -1500,
        fixed_cents: null,
    });
});

test("探测结果 → {available, cacheable}：200/404 可信，其余状态或抛异常都当关、但不可信", () => {
    assert.deepEqual(channelProbeOutcome(200), { available: true, cacheable: true });
    assert.deepEqual(channelProbeOutcome(404), { available: false, cacheable: true });
    assert.deepEqual(channelProbeOutcome(401), { available: false, cacheable: false });
    assert.deepEqual(channelProbeOutcome(403), { available: false, cacheable: false });
    assert.deepEqual(channelProbeOutcome(500), { available: false, cacheable: false });
    assert.deepEqual(channelProbeOutcome(null), { available: false, cacheable: false });
});

test("开关探测：200 开，404 / 500 / 抛异常一律关，但只有 200 / 404 可信（可缓存）", async () => {
    assert.deepEqual(await channelsAvailable(async () => ({ status: 200 })), { available: true, cacheable: true });
    assert.deepEqual(await channelsAvailable(async () => ({ status: 404 })), { available: false, cacheable: true });
    assert.deepEqual(await channelsAvailable(async () => ({ status: 500 })), { available: false, cacheable: false });
    assert.deepEqual(
        await channelsAvailable(async () => {
            throw new Error("network");
        }),
        { available: false, cacheable: false },
    );
});

test("deepEqual：键序无关的深度相等", () => {
    assert.equal(deepEqual({ a: 1, b: 2 }, { b: 2, a: 1 }), true);
    assert.equal(deepEqual({ a: 1 }, { a: 1, b: 2 }), false);
    assert.equal(deepEqual({ a: [1, 2] }, { a: [1, 2] }), true);
    assert.equal(deepEqual({ a: [1, 2] }, { a: [2, 1] }), false);
    assert.equal(deepEqual(null, null), true);
    assert.equal(deepEqual(null, {}), false);
    assert.equal(deepEqual({ a: { b: 1 } }, { a: { b: 1 } }), true);
    assert.equal(deepEqual({ a: { b: 1 } }, { a: { b: 2 } }), false);
    assert.equal(deepEqual(1, "1"), false);
    assert.equal(deepEqual("x", "x"), true);
});

test("Shopify 店铺域名", () => {
    assert.equal(shopifyDomainOk("keel-demo.myshopify.com"), true);
    assert.equal(shopifyDomainOk(" keel-demo.myshopify.com "), true);
    assert.equal(shopifyDomainOk("keel-demo.com"), false);
    assert.equal(shopifyDomainOk("https://keel-demo.myshopify.com"), false);
    assert.equal(shopifyDomainOk("Keel.myshopify.com"), false);
    assert.equal(shopifyDomainOk(".myshopify.com"), false);
});

test("由渠道管理的标注", () => {
    assert.equal(managedLabel("shopify"), "由 Shopify 管理");
    assert.equal(managedLabel("douyin"), "由 douyin 管理");
    assert.equal(managedLabel(null), null);
    assert.equal(managedLabel(undefined), null);
    assert.equal(managedLabel(""), null);
});
