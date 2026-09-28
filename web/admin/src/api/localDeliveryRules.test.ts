// 同城配送配置页纯逻辑的单元测试，已列进仓库根 Makefile 的 `admin-test` 目标。
// 单跑：`cd web/admin && node --test src/api/localDeliveryRules.test.ts`。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
    checkLocalDeliveryDraft,
    emptyRuleDraft,
    kmToMeters,
    localDistanceText,
    metersToKmInput,
    parseRuleDraft,
    ruleDraftOf,
    sortTierDrafts,
    tiersSummary,
    type DeliveryTierLike,
    type RuleDraft,
} from "./localDeliveryRules.ts";

const tier = (withinM: number, feeCents: number): DeliveryTierLike => ({ within_m: withinM, fee_cents: feeCents });

test("公里 → 米：字符串解析，不做浮点乘法", () => {
    assert.equal(kmToMeters("1"), 1000);
    assert.equal(kmToMeters("1.5"), 1500);
    assert.equal(kmToMeters("0.001"), 1);
    assert.equal(kmToMeters("2.999"), 2999);
    assert.equal(kmToMeters(""), null);
    assert.equal(kmToMeters("-1"), null);
    assert.equal(kmToMeters("1.5000"), null); // 超过三位小数
    assert.equal(kmToMeters("abc"), null);
});

test("米 → 公里回显", () => {
    assert.equal(metersToKmInput(1000), "1");
    assert.equal(metersToKmInput(1500), "1.5");
    assert.equal(metersToKmInput(1), "0.001");
    assert.equal(metersToKmInput(0), "0");
    assert.equal(metersToKmInput(-1), "");
});

test("本地校验：严格递增、1–100000 米、单档 ≤ 100 元、起送价与免配送费门槛 ≤ 10 万元", () => {
    assert.equal(checkLocalDeliveryDraft([tier(1000, 500), tier(3000, 800)], 0, 0), null);
    assert.match(checkLocalDeliveryDraft([tier(3000, 500), tier(1000, 800)], 0, 0) ?? "", /严格递增/);
    assert.match(checkLocalDeliveryDraft([tier(1000, 500), tier(1000, 800)], 0, 0) ?? "", /严格递增/);
    assert.match(checkLocalDeliveryDraft([tier(0, 500)], 0, 0) ?? "", /1 米到 100 公里/);
    assert.match(checkLocalDeliveryDraft([tier(100001, 500)], 0, 0) ?? "", /1 米到 100 公里/);
    assert.match(checkLocalDeliveryDraft([tier(1000, 10001)], 0, 0) ?? "", /不能超过 100 元/);
    assert.match(checkLocalDeliveryDraft([], 10000001, 0) ?? "", /起送价不能超过/);
    assert.match(checkLocalDeliveryDraft([], 0, 10000001) ?? "", /满多少免配送费不能超过/);
    const eleven = Array.from({ length: 11 }, (_, i) => tier((i + 1) * 1000, 100));
    assert.match(checkLocalDeliveryDraft(eleven, 0, 0) ?? "", /最多 10 档/);
});

test("订单详情里的距离怎么念", () => {
    assert.equal(localDistanceText(null), "地址无坐标，按最后一档");
    assert.equal(localDistanceText(1500), "1.5 公里");
    assert.equal(localDistanceText(999), "1.0 公里");
    assert.equal(localDistanceText(0), "0.0 公里");
});

test("分档摘要：模板列表 / 生效规则共用", () => {
    assert.equal(tiersSummary([]), "不收配送费");
    assert.equal(tiersSummary([tier(3000, 300), tier(5000, 500)]), "≤3 公里 ¥3、≤5 公里 ¥5");
});

test("表单草稿 ↔ 服务端配置往返", () => {
    const cfg = { min_order_cents: 2000, free_over_cents: 5000, fee_tiers: [tier(3000, 300), tier(5000, 500)] };
    const draft = ruleDraftOf(cfg);
    assert.deepEqual(draft, { minOrder: "20", freeOver: "50", tiers: [{ withinKm: "3", feeYuan: "3" }, { withinKm: "5", feeYuan: "5" }] });
    const back = parseRuleDraft(draft);
    assert.deepEqual(back, { ok: true, config: cfg });

    assert.deepEqual(emptyRuleDraft(), { minOrder: "0", freeOver: "0", tiers: [] });
});

test("表单草稿校验：金额 / 距离解析失败，或不满足 checkLocalDeliveryDraft 的规矩", () => {
    const bad = parseRuleDraft({ minOrder: "abc", freeOver: "0", tiers: [] });
    assert.equal(bad.ok, false);
    if (!bad.ok) assert.match(bad.msg, /起送价.*不是合法金额/);

    const badTier = parseRuleDraft({ minOrder: "0", freeOver: "0", tiers: [{ withinKm: "abc", feeYuan: "1" }] });
    assert.equal(badTier.ok, false);
    if (!badTier.ok) assert.match(badTier.msg, /第 1 档的距离.*不是合法的公里数/);

    const badOrder = parseRuleDraft({ minOrder: "0", freeOver: "0", tiers: [{ withinKm: "5", feeYuan: "1" }, { withinKm: "3", feeYuan: "1" }] });
    assert.equal(badOrder.ok, false);
    if (!badOrder.ok) assert.match(badOrder.msg, /严格递增/);
});

test("按公里排序草稿：解析不出的排到最后", () => {
    const tiers: RuleDraft["tiers"] = [{ withinKm: "5", feeYuan: "5" }, { withinKm: "abc", feeYuan: "9" }, { withinKm: "3", feeYuan: "3" }];
    sortTierDrafts(tiers);
    assert.deepEqual(
        tiers.map((t) => t.withinKm),
        ["3", "5", "abc"],
    );
});
