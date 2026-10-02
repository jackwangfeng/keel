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

/** 探测一次 `GET /admin/channel-kinds` 的结果该怎么信。 */
export interface ChannelProbeOutcome {
    /** 这次该不该当作「开」。 */
    available: boolean;
    /** 这个结果能不能存起来、下次不再探。 */
    cacheable: boolean;
}

/**
 * 探测结果（HTTP 状态；`null` = 探测本身抛了，没拿到状态）→ {available, cacheable}。
 *
 * 200 → 开，而且可信：服务端明确说这个商家开了渠道层，存下来。
 * 404 → 关，而且可信：KEEL_CHANNELS 没开，路由根本没注册，这个状态不会因为重新登录变。
 * 其余任何状态（401/403/5xx……）或探测本身抛错 → 当作关，但**不可信**：
 * 这类结果多半是会话失效、网络抖一下，不是「这个商家真的没有渠道层」——
 * 缓存成关的话，重新登录之后菜单也不会回来，所以这一类不缓存，下次再探一次。
 */
export function channelProbeOutcome(status: number | null): ChannelProbeOutcome {
    if (status === 200) return { available: true, cacheable: true };
    if (status === 404) return { available: false, cacheable: true };
    return { available: false, cacheable: false };
}

/**
 * 「渠道」菜单开不开：探测 `GET /admin/channel-kinds`，折成 {available, cacheable}。
 * 永不抛：菜单少一项不该让框架报错。
 */
export async function channelsAvailable(probe: () => Promise<{ status: number }>): Promise<ChannelProbeOutcome> {
    try {
        const r = await probe();
        return channelProbeOutcome(r.status);
    } catch {
        return channelProbeOutcome(null);
    }
}

/**
 * 深度相等，键序无关：用来判断一个自由对象（渠道 config）是不是**真的**变了，
 * 还是只是「原样取出来再拼了一遍，字段顺序不一样而已」。
 * 只认 JSON 能表达的形状（object / array / 原子值）——config 从 PATCH 请求体来，够用。
 */
export function deepEqual(a: unknown, b: unknown): boolean {
    if (a === b) return true;
    if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
    if (Array.isArray(a) || Array.isArray(b)) {
        if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
        return a.every((v, i) => deepEqual(v, b[i]));
    }
    const ao = a as Record<string, unknown>;
    const bo = b as Record<string, unknown>;
    const ak = Object.keys(ao).sort();
    const bk = Object.keys(bo).sort();
    if (ak.length !== bk.length || ak.some((k, i) => k !== bk[i])) return false;
    return ak.every((k) => deepEqual(ao[k], bo[k]));
}


/**
 * 商品由哪个渠道管理时的标注（AdminProduct.managed_by）。null / 空 → null（不标）。
 * "shopify" → "由 Shopify 管理"；不认识的渠道原样："由 <渠道> 管理"。
 */
export function managedLabel(channel: string | null | undefined): string | null {
    if (channel === null || channel === undefined || channel === "") return null;
    return `由 ${channelLabel(channel)} 管理`;
}

// ------------------------------------------------------------ 渠道订单（第三期）

type TagType = "success" | "info" | "warning" | "danger" | "primary";

/** 渠道单的规整状态（契约 ChannelOrderStatus）：1 待付款 … 7 已拒单。异常不是状态，另外标红。 */
export function channelOrderStatusLabel(s: number): { text: string; type: TagType } {
    switch (s) {
        case 1:
            return { text: "待付款", type: "info" };
        case 2:
            return { text: "新单", type: "warning" };
        case 3:
            return { text: "已接单", type: "primary" };
        case 4:
            return { text: "已发货", type: "primary" };
        case 5:
            return { text: "已完成", type: "success" };
        case 6:
            return { text: "已取消", type: "info" };
        case 7:
            return { text: "已拒单", type: "danger" };
        default:
            return { text: `状态 ${s}`, type: "info" };
    }
}

/** 列表筛选用的状态选项。 */
export const CHANNEL_ORDER_STATUS_OPTIONS = [1, 2, 3, 4, 5, 6, 7].map((s) => ({ value: s, text: channelOrderStatusLabel(s).text }));

/**
 * 一张渠道单此刻能点哪些按钮（服务端是准绳，不满足时回 409；这里只是不让人点注定失败的）：
 *   重试 —— 有异常、且还没有 keel 订单（有订单号的异常多半是发货后平台取消，要人去订单 / 售后处理，不是重试能解决的）；
 *   接单 / 拒单 —— 渠道要求接单（ChannelKind.accept_required）、新单、没异常、还没成单。
 */
export function channelOrderActions(
    o: { status: number; exception?: string | null; order_no?: string | null },
    acceptRequired: boolean,
): { retry: boolean; accept: boolean; reject: boolean } {
    const hasException = o.exception !== undefined && o.exception !== null;
    const hasOrder = o.order_no !== undefined && o.order_no !== null;
    const awaiting = acceptRequired && o.status === 2 && !hasException && !hasOrder;
    return { retry: hasException && !hasOrder, accept: awaiting, reject: awaiting };
}

/**
 * 金额拆解：keel 实付 = 顾客实付（buyer_paid，不含税）；平台总价 = 实付 + 税（美国店价外税，税不进 keel 订单）。
 * 补贴合计 = 平台补贴 + 商家补贴（keel 订单的 discount）。
 */
export function channelOrderTotals(a: { buyer_paid: number; tax: number; platform_subsidy: number; merchant_subsidy: number }): {
    paid: number;
    tax: number;
    platformTotal: number;
    subsidy: number;
} {
    return { paid: a.buyer_paid, tax: a.tax, platformTotal: a.buyer_paid + a.tax, subsidy: a.platform_subsidy + a.merchant_subsidy };
}

/** 平台申请的类别（契约 ChannelOrderRequest.kind）。 */
export function requestKindLabel(k: number): string {
    switch (k) {
        case 1:
            return "取消订单";
        case 2:
            return "部分退款";
        case 3:
            return "缺货调整";
        default:
            return `申请 ${k}`;
    }
}

/** 平台申请的状态（契约 ChannelOrderRequest.status）。 */
export function requestStatusLabel(s: number): { text: string; type: TagType } {
    switch (s) {
        case 1:
            return { text: "待处理", type: "warning" };
        case 2:
            return { text: "已同意", type: "success" };
        case 3:
            return { text: "已拒绝", type: "danger" };
        case 4:
            return { text: "超时自动同意", type: "info" };
        case 5:
            return { text: "平台已撤销", type: "info" };
        default:
            return { text: `状态 ${s}`, type: "info" };
    }
}

/** 渠道单收货地址拼一行（国家放最后，空段跳过）。 */
export function channelAddressText(a: { province: string; city: string; district: string; address: string; zip: string; country: string }): string {
    const head = [a.province, a.city, a.district, a.address].filter((x) => x !== "").join(" ");
    const tail = [a.zip, a.country].filter((x) => x !== "").join(" ");
    return [head, tail].filter((x) => x !== "").join("，") || "—";
}

/** 后台订单的来源徽标：渠道单「来自 Shopify #1001」，自营单 null。 */
export function orderSourceBadge(o: { source?: number; channel?: { kind: string; external_order_name: string } | null }): string | null {
    if (o.source !== 1) return null;
    if (o.channel === undefined || o.channel === null) return "来自渠道";
    return `来自 ${channelLabel(o.channel.kind)} ${o.channel.external_order_name}`.trim();
}

/** 后台订单的「买家」：渠道单没有 keel 买家，统一叫「渠道顾客」。 */
export function orderBuyerLabel(o: { source?: number }): string | null {
    return o.source === 1 ? "渠道顾客" : null;
}

/** 渠道退款（第三期 Task 5 直接写成功的退款行，没有支付单）：服务端总带 payment_no，渠道退款是空串。 */
export function isChannelRefund(r: { payment_no?: string | null }): boolean {
    return r.payment_no === "";
}
