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

/**
 * 五种可自动执行的提案种类，固定顺序（与 GET 返回的五条一一对应）。`refund_decision` 不许自动执行（资金动作）。
 * `channel_stock_rule`（00332）是 00130 之后补的一种，放在最后，不打乱前四种原有的顺序。
 */
export const AUTO_POLICY_KINDS: AutoPolicyKind[] = ["inventory_adjust", "flash_price", "coupon", "product_copy", "channel_stock_rule"];

export const AUTO_POLICY_KIND_LABEL: Record<AutoPolicyKind, string> = {
    inventory_adjust: "加库存",
    flash_price: "限时折扣",
    coupon: "发券",
    product_copy: "改文案",
    channel_stock_rule: "调渠道分配",
};

export type CapField = "max_units" | "min_discount_rate" | "max_discount_cents" | "max_ratio_step_bp" | null;

/**
 * 单笔上限按种类取哪个字段（M11 设计 §6）；`product_copy` 没有单笔上限，只有条数上限。
 * `channel_stock_rule`（spec §6）的单笔上限是「渠道分配单次比例变化上限」（`max_ratio_step_bp`，万分比，
 * 0 = 不自动执行这种提案）；安全库存变化上限另有 `max_units`，但不在这一个输入框里（界面只给一个数）。
 */
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
        case "channel_stock_rule":
            return "max_ratio_step_bp";
    }
}

/** 百分点（0–100，两位小数）→ 万分比（0–10000）。字符串解析，理由同 money.ts 的 yuanToCents：避免浮点误差。 */
function pointsToBP(input: string): number | null {
    const m = /^(\d{1,3})(?:\.(\d{0,2}))?$/.exec(input.trim());
    if (m === null) return null;
    const whole = Number(m[1]);
    const frac = (m[2] ?? "").padEnd(2, "0");
    const bp = whole * 100 + Number(frac);
    return bp >= 0 && bp <= 10000 ? bp : null;
}

/** 万分比（0–10000）→ 百分点回显字符串，与 rateToZheInput 同一个套路（去掉多余的尾零）。 */
function bpToPointsInput(bp: number): string {
    if (!Number.isInteger(bp) || bp < 0 || bp > 10000) return "";
    const whole = Math.floor(bp / 100);
    const frac = bp % 100;
    if (frac === 0) return String(whole);
    return `${whole}.${String(frac).padStart(2, "0").replace(/0$/, "")}`;
}

/** 单笔上限那一行给人看的话，比如「单笔至多加 40 件」「折扣不低于 8.5 折」「面额不超过 ¥50.00」。 */
export function capLabel(
    kind: AutoPolicyKind,
    policy: Pick<AgentAutoPolicyInput, "max_units" | "min_discount_rate" | "max_discount_cents" | "max_ratio_step_bp">,
): string {
    switch (capFieldOf(kind)) {
        case "max_units":
            return `单笔至多加 ${policy.max_units} 件`;
        case "min_discount_rate":
            return `折扣不低于 ${rateLabel(policy.min_discount_rate)}`;
        case "max_discount_cents":
            return `面额不超过 ¥${centsToYuanInput(policy.max_discount_cents)}`;
        case "max_ratio_step_bp": {
            const bp = policy.max_ratio_step_bp ?? 0;
            return bp <= 0 ? "0（不自动执行）" : `单次比例变化至多 ${bpToPointsInput(bp)} 个百分点`;
        }
        case null:
            return "没有单笔上限";
    }
}

/** 条数上限那一行，0 = 不自动执行（契约原话）。 */
export function dailyLimitLabel(dailyLimit: number): string {
    return dailyLimit <= 0 ? "0（不自动执行）" : `每 24 小时至多 ${dailyLimit} 条`;
}

/** 把表单里那一个单笔上限值，落回策略体上——其余字段原样保留，不被表单上没显示的输入框悄悄清零。 */
export function withCap(kind: AutoPolicyKind, base: AgentAutoPolicyInput, capValue: number): AgentAutoPolicyInput {
    const field = capFieldOf(kind);
    if (field === null) return base;
    return { ...base, [field]: capValue };
}

export type CapParse = { ok: true; value: number } | { ok: false; msg: string };

/**
 * 表单里那一个单笔上限的输入字符串 → 策略体上的数值，按种类解析（件数是整数；折扣按「折」输入，
 * 千分比 500–1000，与限时折扣提案同一条上限——过五折的自动执行策略服务端会 422）；
 * 面额按「元」输入；`channel_stock_rule` 按「百分点」输入（0–100，0 = 不自动执行这种提案——与其余
 * 三种「填大于 0」不同，0 对这一种是合法值，见 spec §6）。`product_copy` 没有这一步。
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
    if (field === "max_ratio_step_bp") {
        const bp = pointsToBP(input);
        if (bp === null) return { ok: false, msg: "填 0 到 100 之间的百分点（0 = 不自动执行这种提案）" };
        return { ok: true, value: bp };
    }
    return { ok: false, msg: "这一种没有单笔上限" };
}

/** 单笔上限落回表单里的回显字符串（与 parseCap 互逆）。 */
export function capInputText(
    kind: AutoPolicyKind,
    policy: Pick<AgentAutoPolicyInput, "max_units" | "min_discount_rate" | "max_discount_cents" | "max_ratio_step_bp">,
): string {
    switch (capFieldOf(kind)) {
        case "max_units":
            return String(policy.max_units);
        case "min_discount_rate":
            return rateToZheInput(policy.min_discount_rate);
        case "max_discount_cents":
            return centsToYuanInput(policy.max_discount_cents);
        case "max_ratio_step_bp":
            return bpToPointsInput(policy.max_ratio_step_bp ?? 0);
        case null:
            return "";
    }
}
