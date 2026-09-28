// 店铺设置页的界面规则。`node --test src/api/shopSettings.test.ts` 直接跑，不要 node_modules。
//
// 守的是「整体替换」的两个坑：客服电话留空要**不带**这个字段（带空串是 422），
// 以及天数越界在本地就挡住、不等服务端。
import { test } from "node:test";
import assert from "node:assert/strict";
import { formOf, shopSettingsBody, timezoneOptions, validateShopSettings, type ShopSettingsForm } from "./shopSettings.ts";

const base: ShopSettingsForm = { timezone: "Asia/Shanghai", autoConfirmDays: 7, returnShipDays: 7, afterSaleDays: 15, servicePhone: "" };

test("表单 → 请求体：客服电话留空不带字段，有值时去掉首尾空白", () => {
    assert.deepEqual(shopSettingsBody(base), { timezone: "Asia/Shanghai", auto_confirm_days: 7, return_ship_days: 7, after_sale_days: 15 });
    assert.deepEqual(shopSettingsBody({ ...base, servicePhone: "  400-800  ", timezone: " UTC " }), {
        timezone: "UTC",
        auto_confirm_days: 7,
        return_ship_days: 7,
        after_sale_days: 15,
        service_phone: "400-800",
    });
    assert.equal("service_phone" in shopSettingsBody({ ...base, servicePhone: "   " }), false);
});

test("响应 → 表单：没设客服电话是空串", () => {
    const f = formOf({
        shop_name: "店",
        service_phone: null,
        timezone: "Asia/Tokyo",
        auto_confirm_days: 10,
        return_ship_days: 3,
        after_sale_days: 20,
        updated_at: null,
    });
    assert.deepEqual(f, { timezone: "Asia/Tokyo", autoConfirmDays: 10, returnShipDays: 3, afterSaleDays: 20, servicePhone: "" });
});

test("本地预检：天数 1–365 的整数、时区不能空也不能是 Local、电话至多 32 字", () => {
    assert.deepEqual(validateShopSettings(base), {});
    assert.deepEqual(validateShopSettings({ ...base, autoConfirmDays: 1, returnShipDays: 365 }), {});
    assert.ok(validateShopSettings({ ...base, autoConfirmDays: 0 }).autoConfirmDays);
    assert.ok(validateShopSettings({ ...base, returnShipDays: 366 }).returnShipDays);
    assert.ok(validateShopSettings({ ...base, afterSaleDays: 0 }).afterSaleDays);
    assert.ok(validateShopSettings({ ...base, returnShipDays: 2.5 }).returnShipDays);
    assert.ok(validateShopSettings({ ...base, timezone: "  " }).timezone);
    assert.ok(validateShopSettings({ ...base, timezone: "Local" }).timezone);
    assert.ok(validateShopSettings({ ...base, servicePhone: "1".repeat(33) }).servicePhone);
    assert.deepEqual(validateShopSettings({ ...base, servicePhone: "1".repeat(32) }), {});
});

test("时区选项：常用的排前面，不重复", () => {
    const opts = timezoneOptions(["Africa/Abidjan", "Asia/Shanghai", "UTC", "Zulu"]);
    assert.equal(opts[0], "Asia/Shanghai");
    assert.equal(new Set(opts).size, opts.length);
    assert.ok(opts.includes("Africa/Abidjan") && opts.includes("Zulu"));
});
