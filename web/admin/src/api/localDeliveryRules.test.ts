// 同城配送配置页纯逻辑的单元测试。`node --test src/api/localDeliveryRules.test.ts`
// 直接跑，不要 node_modules。
//
// 注：这个文件目前**没有**被列进仓库根 Makefile 的 `admin-test` 目标（那份文件不在
// web/admin/src 之内，本次改动的范围限定在 web/admin/src，没有一并加这一行）。
// 单跑：`cd web/admin && node --test src/api/localDeliveryRules.test.ts`。
import { test } from "node:test";
import assert from "node:assert/strict";
import { checkLocalDeliveryDraft, kmToMeters, localDistanceText, metersToKmInput, type DeliveryTierLike } from "./localDeliveryRules.ts";

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
