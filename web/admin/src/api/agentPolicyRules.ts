// AI 员工自动执行策略（AI 经营 M11 §6，00130）的界面规则：四种可自动执行的提案种类，
// 每种只看与它相关的那一个单笔上限字段（`AgentAutoPolicyInput` 四个数字字段都必填，
// 但契约里已经说明「单笔上限按种类取不同的字段」——界面只显示、只让人填那一个，
// 其余三个原样带回，不让人在不相关的输入框里填出一个会被忽略的数字）。**纯函数**，
// 由 `make admin-test` 用 `node --test` 直接跑，约定同 promotionRules.ts / money.ts。

import type { components } from "@contract/schema.js";
import { centsToYuanInput, rateLabel, rateToZheInput, yuanToCents, zheToRate } from "./money.ts";

type S = components["schemas"];
export type AutoPolicyKind = S["AgentAutoPolicy"]["kind"];
export type AgentAutoPolicyInput = S["AgentAutoPolicyInput"];

/** 四种可自动执行的提案种类，固定顺序（与 GET 返回的四条一一对应）。`refund_decision` 不许自动执行（资金动作）。 */
export const AUTO_POLICY_KINDS: AutoPolicyKind[] = ["inventory_adjust", "flash_price", "coupon", "product_copy"];

export const AUTO_POLICY_KIND_LABEL: Record<AutoPolicyKind, string> = {
    inventory_adjust: "加库存",
    flash_price: "限时折扣",
    coupon: "发券",
    product_copy: "改文案",
};

export type CapField = "max_units" | "min_discount_rate" | "max_discount_cents" | null;

/** 单笔上限按种类取哪个字段（M11 设计 §6）；`product_copy` 没有单笔上限，只有条数上限。 */
export function capFieldOf(kind: AutoPolicyKind): CapField {
    switch (kind) {
        case "inventory_adjust":
            return "max_units";
        case "flash_price":
            return "min_discount_rate";
        case "coupon":
            return "max_discount_cents";
        case "product_copy":
            return null;
    }
}

/** 单笔上限那一行给人看的话，比如「单笔至多加 40 件」「折扣不低于 8.5 折」「面额不超过 ¥50.00」。 */
export function capLabel(kind: AutoPolicyKind, policy: Pick<AgentAutoPolicyInput, "max_units" | "min_discount_rate" | "max_discount_cents">): string {
    switch (capFieldOf(kind)) {
        case "max_units":
            return `单笔至多加 ${policy.max_units} 件`;
        case "min_discount_rate":
            return `折扣不低于 ${rateLabel(policy.min_discount_rate)}`;
        case "max_discount_cents":
            return `面额不超过 ¥${centsToYuanInput(policy.max_discount_cents)}`;
        case null:
            return "没有单笔上限";
    }
}

/** 条数上限那一行，0 = 不自动执行（契约原话）。 */
export function dailyLimitLabel(dailyLimit: number): string {
    return dailyLimit <= 0 ? "0（不自动执行）" : `每 24 小时至多 ${dailyLimit} 条`;
}

/** 把表单里那一个单笔上限值，落回四字段的策略体上——其余字段原样保留，不被表单上没显示的输入框悄悄清零。 */
export function withCap(kind: AutoPolicyKind, base: AgentAutoPolicyInput, capValue: number): AgentAutoPolicyInput {
    const field = capFieldOf(kind);
    if (field === null) return base;
    return { ...base, [field]: capValue };
}

export type CapParse = { ok: true; value: number } | { ok: false; msg: string };

/**
 * 表单里那一个单笔上限的输入字符串 → 策略体上的数值，按种类解析（件数是整数；折扣按「折」输入，
 * 千分比 500–1000，与限时折扣提案同一条上限——过五折的自动执行策略服务端会 422）；
 * 面额按「元」输入。`product_copy` 没有这一步。
 */
export function parseCap(kind: AutoPolicyKind, input: string): CapParse {
    const field = capFieldOf(kind);
    if (field === "max_units") {
        const n = Number(input.trim());
        if (!Number.isInteger(n) || n <= 0) return { ok: false, msg: "填大于 0 的整数（件）" };
        return { ok: true, value: n };
    }
    if (field === "min_discount_rate") {
        const rate = zheToRate(input);
        if (rate === null || rate < 500) return { ok: false, msg: "填 5 到 9.99 之间的折数（不能比五折更低）" };
        return { ok: true, value: rate };
    }
    if (field === "max_discount_cents") {
        const cents = yuanToCents(input);
        if (cents === null || cents <= 0) return { ok: false, msg: "填大于 0 的元" };
        return { ok: true, value: cents };
    }
    return { ok: false, msg: "这一种没有单笔上限" };
}

/** 单笔上限落回表单里的回显字符串（与 parseCap 互逆）。 */
export function capInputText(kind: AutoPolicyKind, policy: Pick<AgentAutoPolicyInput, "max_units" | "min_discount_rate" | "max_discount_cents">): string {
    switch (capFieldOf(kind)) {
        case "max_units":
            return String(policy.max_units);
        case "min_discount_rate":
            return rateToZheInput(policy.min_discount_rate);
        case "max_discount_cents":
            return centsToYuanInput(policy.max_discount_cents);
        case null:
            return "";
    }
}
