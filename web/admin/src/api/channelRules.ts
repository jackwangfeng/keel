// 渠道管理页的界面规则（纯函数，`make admin-test` 用 node --test 直接跑）。
//
// 万分比 ↔ 百分数、元 ↔ 分的换算，规则表单的校验，「渠道」菜单的开关探测。
// 只 import 同目录的 money.ts（它本身零依赖），不碰契约以外的运行时模块。
//
// 校验与服务端的约束是同一组数（迁移 00301 的 CHECK 与 internal/service/admin_channel.go）：
//   ratio_bp 0..10000、safety_qty ≥ 0、cap_qty ≥ 0 或空、SKU 级库存规则要同时给门店；
//   markup_bp -9000..100000、fixed_cents > 0 且只能配给 SKU。
// 前端先拦一遍只是为了让人在提交前就看到中文原因；服务端照样会拒。

import { centsToYuanInput, yuanToCents } from "./money.ts";

/** 角色位：1 商品源、2 库存源、4 销售渠道。 */
export const ROLE_BITS = [
    { bit: 1, text: "商品源" },
    { bit: 2, text: "库存源" },
    { bit: 4, text: "销售渠道" },
] as const;

/** 33.33（%）→ 3333（万分比）。四舍五入到整数 bp：33.33 * 100 是 3332.9999999999995。 */
export function percentToBp(p: number): number {
    return Math.round(p * 100);
}

/** 3333 → 33.33。 */
export function bpToPercent(bp: number): number {
    return Math.round(bp) / 100;
}

/** 渠道账号状态：1 启用 / 2 停用 / 3 凭据失效。 */
export function bindingStatusLabel(s: number): { text: string; type: "success" | "info" | "danger" } {
    switch (s) {
        case 1:
            return { text: "启用", type: "success" };
        case 2:
            return { text: "停用", type: "info" };
        case 3:
            return { text: "凭据失效", type: "danger" };
        default:
            return { text: `状态 ${s}`, type: "info" };
    }
}

/** 5 → "商品源、销售渠道"。 */
export function rolesLabel(roles: number): string {
    const out = ROLE_BITS.filter((r) => (roles & r.bit) !== 0).map((r) => r.text);
    return out.length === 0 ? "—" : out.join("、");
}

/** 渠道能当的角色（ChannelKind.roles）里有哪几位，给新建对话框的勾选框用。 */
export function roleOptions(kindRoles: number): { bit: number; text: string }[] {
    return ROLE_BITS.filter((r) => (kindRoles & r.bit) !== 0).map((r) => ({ bit: r.bit, text: r.text }));
}

/** 位数组 ↔ 位掩码。 */
export function rolesFromBits(bits: readonly number[]): number {
    return bits.reduce((acc, b) => acc | b, 0);
}
export function bitsFromRoles(roles: number): number[] {
    return ROLE_BITS.filter((r) => (roles & r.bit) !== 0).map((r) => r.bit);
}

/** 渠道显示名。认识的给中文 / 品牌名，不认识的原样。 */
export function channelLabel(channel: string): string {
    switch (channel) {
        case "shopify":
            return "Shopify";
        case "meituan":
            return "美团";
        case "eleme":
            return "饿了么";
        case "fake":
            return "测试渠道";
        default:
            return channel;
    }
}

/** Shopify 店铺账号：xxx.myshopify.com（店铺名小写字母、数字、连字符）。 */
export function shopifyDomainOk(s: string): boolean {
    return /^[a-z0-9][a-z0-9-]*\.myshopify\.com$/.test(s.trim());
}

export interface StockRuleForm {
    storeId: number | null;
    skuId: number | null;
    ratioPercent: number;
    safetyQty: number;
    capQty: number | null;
}

/** 库存规则表单的中文错误列表；空数组 = 可以提交。 */
export function validateStockRule(r: StockRuleForm): string[] {
    const errs: string[] = [];
    if (r.skuId !== null && r.storeId === null) errs.push("SKU 级规则要同时选门店");
    if (!Number.isFinite(r.ratioPercent) || r.ratioPercent < 0 || r.ratioPercent > 100) {
        errs.push("可售比例要在 0% 到 100% 之间");
    } else if (Math.abs(percentToBp(r.ratioPercent) - r.ratioPercent * 100) > 1e-6) {
        errs.push("可售比例最多两位小数");
    }
    if (!Number.isInteger(r.safetyQty) || r.safetyQty < 0) errs.push("安全库存要是不小于 0 的整数");
    if (r.capQty !== null && (!Number.isInteger(r.capQty) || r.capQty < 0)) {
        errs.push("上限要是不小于 0 的整数（留空 = 不封顶，0 = 在这个渠道下架）");
    }
    return errs;
}

/** 库存规则表单 → `PUT …/stock-rules` 的请求体（先过 validateStockRule）。 */
export function stockRuleBody(r: StockRuleForm): {
    store_id: number | null;
    sku_id: number | null;
    ratio_bp: number;
    safety_qty: number;
    cap_qty: number | null;
} {
    return {
        store_id: r.storeId,
        sku_id: r.skuId,
        ratio_bp: percentToBp(r.ratioPercent),
        safety_qty: r.safetyQty,
        cap_qty: r.capQty,
    };
}

export interface PriceRuleForm {
    skuId: number | null;
    markupPercent: number;
    /** 固定价（元）。null 或空串 = 不设固定价。 */
    fixedYuan: string | null;
}

function hasFixed(r: PriceRuleForm): boolean {
    return r.fixedYuan !== null && r.fixedYuan.trim() !== "";
}

export function validatePriceRule(r: PriceRuleForm): string[] {
    const errs: string[] = [];
    if (!Number.isFinite(r.markupPercent) || r.markupPercent < -90 || r.markupPercent > 1000) {
        errs.push("加价要在 -90% 到 1000% 之间");
    } else if (Math.abs(percentToBp(r.markupPercent) - r.markupPercent * 100) > 1e-6) {
        errs.push("加价最多两位小数");
    }
    if (hasFixed(r)) {
        if (r.skuId === null) errs.push("固定价只能配给某个 SKU，渠道级只能设加价");
        const cents = yuanToCents(r.fixedYuan ?? "");
        if (cents === null || cents <= 0) errs.push("固定价要是大于 0、最多两位小数的金额（元）");
    }
    return errs;
}

/** 价格规则表单 → `PUT …/price-rules` 的请求体（先过 validatePriceRule）。 */
export function priceRuleBody(r: PriceRuleForm): { sku_id: number | null; markup_bp: number; fixed_cents: number | null } {
    return {
        sku_id: r.skuId,
        markup_bp: percentToBp(r.markupPercent),
        fixed_cents: hasFixed(r) ? yuanToCents(r.fixedYuan ?? "") : null,
    };
}

/** 价格规则一行的摘要：「固定价 ¥12.30」或「加价 +15%」。 */
export function priceRuleSummary(r: { markup_bp?: number; fixed_cents?: number | null }): string {
    if (r.fixed_cents !== null && r.fixed_cents !== undefined) return `固定价 ¥${centsToYuanInput(r.fixed_cents)}`;
    const p = bpToPercent(r.markup_bp ?? 0);
    return `加价 ${p > 0 ? "+" : ""}${p}%`;
}

/** 库存规则的层级：渠道级 / 门店级 / SKU 级。 */
export function stockRuleLevel(r: { store_id?: number | null; sku_id?: number | null }): string {
    if (r.sku_id !== null && r.sku_id !== undefined) return "SKU 级";
    if (r.store_id !== null && r.store_id !== undefined) return "门店级";
    return "渠道级";
}

/**
 * 「渠道」菜单开不开：探测 `GET /admin/channel-kinds`。
 * 200 → 开；404（KEEL_CHANNELS 关着，路由没注册）与其他任何状态 → 关；探测本身抛错 → 关。
 * 永不抛：菜单少一项不该让框架报错。
 */
export async function channelsAvailable(probe: () => Promise<{ status: number }>): Promise<boolean> {
    try {
        const r = await probe();
        return r.status === 200;
    } catch {
        return false;
    }
}

