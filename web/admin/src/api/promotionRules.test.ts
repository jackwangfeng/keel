// 营销活动页的界面规则。`node --test src/api/promotionRules.test.ts` 直接跑，不要 node_modules。
//
// 守的是「点下去才发现」那一类：元与分换算错一分、满 100 减 200 被放过去、秒杀忘了配额、
// 阶梯门槛按件数却被当成金额。服务端是权威（422），但这些在界面上可以提前挡住。
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildRules, emptyPromotionForm, formOfPromotion, ruleSummary, tierText, type AdminPromotion } from "./promotionRules.ts";

const range: [Date, Date] = [new Date("2026-10-01T00:00:00Z"), new Date("2026-10-08T00:00:00Z")];

test("满减：元换成分，阶梯按门槛排序，空行忽略", () => {
    const f = { ...emptyPromotionForm(), range, tiers: [{ threshold: "200", benefit: "30" }, { threshold: "", benefit: "" }, { threshold: "99.9", benefit: "0.1" }] };
    const r = buildRules(f);
    assert.ok(r.ok);
    assert.deepEqual(r.body.tiers, [
        { threshold: 9990, discount_cents: 10, discount_rate: 0 },
        { threshold: 20000, discount_cents: 3000, discount_rate: 0 },
    ]);
    assert.equal(r.body.threshold_unit, 1);
    assert.equal(r.body.starts_at, "2026-10-01T00:00:00.000Z");
});

test("满减：减的比门槛多、高一档减得少，都挡住", () => {
    const over = buildRules({ ...emptyPromotionForm(), range, tiers: [{ threshold: "100", benefit: "200" }] });
    assert.equal(over.ok, false);
    const inverted = buildRules({
        ...emptyPromotionForm(),
        range,
        tiers: [
            { threshold: "100", benefit: "20" },
            { threshold: "200", benefit: "10" },
        ],
    });
    assert.equal(inverted.ok, false);
});

test("满折按件数：门槛是件数不是分，折换成千分比", () => {
    const r = buildRules({ ...emptyPromotionForm(), type: 2, unit: 2, range, tiers: [{ threshold: "2", benefit: "9" }, { threshold: "3", benefit: "8.5" }] });
    assert.ok(r.ok);
    assert.deepEqual(r.body.tiers, [
        { threshold: 2, discount_cents: 0, discount_rate: 900 },
        { threshold: 3, discount_cents: 0, discount_rate: 850 },
    ]);
    assert.equal(buildRules({ ...emptyPromotionForm(), type: 2, unit: 2, range, tiers: [{ threshold: "1.5", benefit: "9" }] }).ok, false);
});

test("秒杀必须给配额；限时折扣配额恒为 0；范围只认大区与门店", () => {
    const sku = { skuId: 7, mode: "price" as const, value: "9.9", perUserLimit: 1, stockQty: 0 };
    assert.equal(buildRules({ ...emptyPromotionForm(), type: 4, range, skus: [sku] }).ok, false);
    const flash = buildRules({ ...emptyPromotionForm(), type: 4, range, skus: [{ ...sku, stockQty: 50 }] });
    assert.ok(flash.ok);
    assert.deepEqual(flash.body.skus, [{ sku_id: 7, per_user_limit: 1, stock_qty: 50, promo_price_cents: 990, discount_rate: 0 }]);
    const limited = buildRules({ ...emptyPromotionForm(), type: 3, range, skus: [{ ...sku, mode: "rate", value: "8.5", stockQty: 99 }] });
    assert.ok(limited.ok);
    assert.deepEqual(limited.body.skus, [{ sku_id: 7, per_user_limit: 1, stock_qty: 0, discount_rate: 850, promo_price_cents: 0 }]);
    const badScope = buildRules({ ...emptyPromotionForm(), type: 3, range, skus: [sku], scopes: [{ scope_type: 2, target_id: 1, include: true }] });
    assert.equal(badScope.ok, false);
});

test("新人礼要选券模板、不带范围", () => {
    assert.equal(buildRules({ ...emptyPromotionForm(), type: 5, range }).ok, false);
    const r = buildRules({ ...emptyPromotionForm(), type: 5, range, giftTemplateId: 3 });
    assert.ok(r.ok);
    assert.equal(r.body.gift_coupon_template_id, 3);
});

test("时间倒挂挡住", () => {
    assert.equal(buildRules({ ...emptyPromotionForm(), range: [range[1], range[0]] }).ok, false);
    assert.equal(buildRules({ ...emptyPromotionForm(), range: null }).ok, false);
});

test("标签与摘要与服务端同一个写法；表单回显再提交得到同一份规则", () => {
    assert.equal(tierText(1, 1, { threshold: 10000, discount_cents: 1000, discount_rate: 0 }), "满100减10");
    assert.equal(tierText(2, 2, { threshold: 2, discount_cents: 0, discount_rate: 900 }), "满2件9折");
    assert.equal(tierText(2, 1, { threshold: 19990, discount_cents: 0, discount_rate: 875 }), "满199.9享8.75折");
    const p: AdminPromotion = {
        id: 1,
        name: "满减",
        promotion_type: 1,
        threshold_unit: 1,
        stack_with_coupon: false,
        starts_at: range[0].toISOString(),
        ends_at: range[1].toISOString(),
        status: 0,
        phase: "offline",
        tiers: [
            { threshold: 10000, discount_cents: 1000, discount_rate: 0 },
            { threshold: 20000, discount_cents: 3000, discount_rate: 0 },
        ],
        scopes: [],
        skus: [],
        created_at: range[0].toISOString(),
        updated_at: range[0].toISOString(),
    };
    assert.equal(ruleSummary(p), "满100减10，满200减30");
    const again = buildRules(formOfPromotion(p));
    assert.ok(again.ok);
    assert.deepEqual(again.body.tiers, p.tiers);
    assert.equal(again.body.stack_with_coupon, false);
});
