// 店铺设置（契约 GET / PUT /admin/shop-settings，数据模型 §2 shop_preferences）的类型与界面规则。
//
// 纯函数、零依赖：`node --test src/api/shopSettings.test.ts` 直接跑。
// 取数写在页面里（ShopSettingsView.vue），这里只放「表单 ↔ 请求体」与本地预检。

import type { components } from "@contract/schema.js";

type S = components["schemas"];

export type ShopSettings = S["ShopSettings"];
export type ShopSettingsInput = S["ShopSettingsInput"];

/** 页面表单的形状。客服电话空串表示「不设」。 */
export interface ShopSettingsForm {
    timezone: string;
    autoConfirmDays: number;
    returnShipDays: number;
    servicePhone: string;
}

/** 天数的边界，与契约 ShopSettingsInput、00059 的 CHECK 一致。 */
export const DAYS_MIN = 1;
export const DAYS_MAX = 365;
export const PHONE_MAX = 32;

export function formOf(s: ShopSettings): ShopSettingsForm {
    return {
        timezone: s.timezone,
        autoConfirmDays: s.auto_confirm_days,
        returnShipDays: s.return_ship_days,
        servicePhone: s.service_phone ?? "",
    };
}

/**
 * 表单 → PUT 请求体。**整体替换**：客服电话留空就不带这个字段（服务端据此清空），
 * 而不是发一个空串 —— 空串是 422（契约 minLength 1）。
 */
export function shopSettingsBody(f: ShopSettingsForm): ShopSettingsInput {
    const body: ShopSettingsInput = {
        timezone: f.timezone.trim(),
        auto_confirm_days: f.autoConfirmDays,
        return_ship_days: f.returnShipDays,
    };
    const phone = f.servicePhone.trim();
    if (phone !== "") body.service_phone = phone;
    return body;
}

/**
 * 本地预检：返回每个字段的问题（空对象 = 可以提交）。只挡明显的输入错误，
 * 时区是不是一个真能加载的 IANA 名字由服务端判（它的时区库编进了二进制，与浏览器的未必一致）。
 */
export function validateShopSettings(f: ShopSettingsForm): Partial<Record<keyof ShopSettingsForm, string>> {
    const out: Partial<Record<keyof ShopSettingsForm, string>> = {};
    const tz = f.timezone.trim();
    if (tz === "") out.timezone = "请选择时区";
    else if (tz.toLowerCase() === "local") out.timezone = "不能用 Local（那是服务器所在的时区，不是店铺的）";
    for (const key of ["autoConfirmDays", "returnShipDays"] as const) {
        const v = f[key];
        if (!Number.isInteger(v) || v < DAYS_MIN || v > DAYS_MAX) out[key] = `只能是 ${DAYS_MIN} 到 ${DAYS_MAX} 之间的整数`;
    }
    if ([...f.servicePhone.trim()].length > PHONE_MAX) out.servicePhone = `不能超过 ${PHONE_MAX} 个字`;
    return out;
}

/** 常用时区排在前面；其余来自浏览器（Intl.supportedValuesOf），拿不到时只给常用的。 */
export const COMMON_TIMEZONES = [
    "Asia/Shanghai",
    "Asia/Hong_Kong",
    "Asia/Taipei",
    "Asia/Tokyo",
    "Asia/Singapore",
    "Europe/London",
    "Europe/Paris",
    "America/New_York",
    "America/Los_Angeles",
    "UTC",
];

export function timezoneOptions(all: readonly string[]): string[] {
    const rest = all.filter((z) => !COMMON_TIMEZONES.includes(z));
    return [...COMMON_TIMEZONES, ...rest];
}
