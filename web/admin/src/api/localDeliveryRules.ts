// 同城配送配置页（门店围栏 + 距离分档收配送费，00110）的纯逻辑：
// 公里 ↔ 米的换算、提交前的本地校验、订单详情里怎么念距离。
//
// **计算只有服务端一份实现**（契约 `PUT /admin/stores/{store_id}/local-delivery` 那一段
// 描述的口径）：按距离分档、超出最后一档或算不出距离按最后一档收、满额免配送费、起送价。
// 这里的校验只是提前把同一组 422 的规矩说一遍，服务端仍是唯一的执行者。
//
// 距离在契约里以整数米存（`DeliveryTier.within_m`），界面按「公里」输入、允许到
// 毫米级（三位小数）。换算方式与 money.ts 的元 ↔ 分同一个理由：按字符串拆开算，
// 不做浮点乘法——`Number("1.5") * 1000` 在多数情况下没事，但这条路径上一旦哪天
// 换成更细的单位，同一类误差就会出现，不如从一开始就用同一套字符串解析。
//
// 只 import 同目录零运行时依赖的 money.ts（它本身零 import）：`make admin-test`
// 用 `node --test` 直接跑这个文件，不需要 node_modules，与 freightRules.ts 同一个约定。

import { centsToYuanInput, yuanToCents } from "./money.ts";

const KM_RE = /^(\d+)(?:\.(\d{0,3}))?$/;

/**
 * 「1」「1.5」「0.001」→ 米（整数）。空串、负数、超过三位小数、非数字、
 * 超出安全整数范围 → null（调用方据此提示，而不是悄悄截断或四舍五入）。
 */
export function kmToMeters(input: string): number | null {
    const s = input.trim();
    const m = KM_RE.exec(s);
    if (m === null) return null;
    const whole = m[1] ?? "0";
    const frac = (m[2] ?? "").padEnd(3, "0");
    // 整数部分先做长度闸门：超过 10 位时乘 1000 之后就出了 2^53 的安全范围。
    const trimmed = whole.replace(/^0+(?=\d)/, "");
    if (trimmed.length > 10) return null;
    const meters = Number(trimmed) * 1000 + Number(frac);
    return Number.isSafeInteger(meters) ? meters : null;
}

/** 米 → 表单里回显的「1」「1.5」「0.001」。负数与非整数不接受。 */
export function metersToKmInput(meters: number): string {
    if (!Number.isSafeInteger(meters) || meters < 0) return "";
    const whole = Math.floor(meters / 1000);
    const frac = meters % 1000;
    if (frac === 0) return String(whole);
    return `${whole}.${String(frac).padStart(3, "0").replace(/0+$/, "")}`;
}

/** 一条分档的最小形状（契约 `DeliveryTier` 的子集，免得这里 import 契约类型）。 */
export interface DeliveryTierLike {
    within_m: number;
    fee_cents: number;
}

/**
 * 提交前的本地校验。成立返回 null，否则返回一句给运营看的话。
 * 与服务端同一组规矩（契约 `PUT .../local-delivery` 的 422 `invalid-request` 那一段）：
 * 至多 10 档、`within_m` 严格递增且落在 1–100000 米（1 米 ~ 100 公里）、单档配送费
 * 至多 100 元、起送价与免配送费门槛至多 10 万元。服务端仍会再判一遍。
 */
export function checkLocalDeliveryDraft(
    tiers: readonly DeliveryTierLike[],
    minOrderCents: number,
    freeOverCents: number,
): string | null {
    if (tiers.length > 10) return `距离分档最多 10 档，现在有 ${tiers.length} 档`;
    let prev = 0;
    for (let i = 0; i < tiers.length; i += 1) {
        const t = tiers[i]!;
        if (t.within_m < 1 || t.within_m > 100000) return `第 ${i + 1} 档的距离必须在 1 米到 100 公里之间`;
        if (t.within_m <= prev) return `第 ${i + 1} 档的距离必须比上一档大（距离分档要严格递增）`;
        prev = t.within_m;
        if (t.fee_cents < 0 || t.fee_cents > 10000) return `第 ${i + 1} 档的配送费不能超过 100 元`;
    }
    if (minOrderCents < 0 || minOrderCents > 10000000) return "起送价不能超过 10 万元";
    if (freeOverCents < 0 || freeOverCents > 10000000) return "满多少免配送费不能超过 10 万元";
    return null;
}

/**
 * 订单详情里怎么念距离（契约 `LocalDeliveryQuote.distance_m`）：算不出距离（地址或
 * 门店没有坐标）时说明按最后一档收，否则「x.x 公里」——只用于显示，不参与任何计算。
 */
export function localDistanceText(distanceM: number | null): string {
    if (distanceM === null) return "地址无坐标，按最后一档";
    return `${(distanceM / 1000).toFixed(1)} 公里`;
}

/** 「≤3 公里 ¥3、≤5 公里 ¥5」；空数组给「不收配送费」——模板列表 / 生效规则摘要共用。 */
export function tiersSummary(tiers: readonly DeliveryTierLike[]): string {
    if (tiers.length === 0) return "不收配送费";
    return tiers.map((t) => `≤${metersToKmInput(t.within_m)} 公里 ¥${centsToYuanInput(t.fee_cents)}`).join("、");
}

// ---------------------------------------------------------------------------
// 表单草稿：起送价 + 满额免配送费 + 距离分档。模板管理（LocalDeliveryTemplateDialog）
// 与门店「自定义」（LocalDeliveryRuleForm）共用同一套换算 + 校验，不各写一遍。
// ---------------------------------------------------------------------------

export interface TierDraft {
    withinKm: string;
    feeYuan: string;
}

/** 表单里的一份草稿：金额按「元」，距离按「公里」，都是字符串（原样回显用户输入）。 */
export interface RuleDraft {
    minOrder: string;
    freeOver: string;
    tiers: TierDraft[];
}

/** 服务端形状的子集（`LocalDeliveryConfig` / `LocalDeliveryTemplateInput` 都满足）。 */
export interface RuleConfig {
    min_order_cents: number;
    free_over_cents: number;
    fee_tiers: DeliveryTierLike[];
}

/** 空白草稿：新建模板、或门店从没存过自定义规则时的默认值。 */
export function emptyRuleDraft(): RuleDraft {
    return { minOrder: "0", freeOver: "0", tiers: [] };
}

/** 服务端配置 → 表单草稿（分转元、米转公里）。 */
export function ruleDraftOf(cfg: RuleConfig): RuleDraft {
    return {
        minOrder: centsToYuanInput(cfg.min_order_cents),
        freeOver: centsToYuanInput(cfg.free_over_cents),
        tiers: cfg.fee_tiers.map((t) => ({ withinKm: metersToKmInput(t.within_m), feeYuan: centsToYuanInput(t.fee_cents) })),
    };
}

/** 按公里排序（解析不出的排到最后）；提交前与「离开输入框」时都调一次。 */
export function sortTierDrafts(tiers: TierDraft[]): void {
    tiers.sort((a, b) => {
        const ma = kmToMeters(a.withinKm) ?? Number.POSITIVE_INFINITY;
        const mb = kmToMeters(b.withinKm) ?? Number.POSITIVE_INFINITY;
        return ma - mb;
    });
}

/**
 * 表单草稿 → 服务端形状：金额 / 距离解析 + `checkLocalDeliveryDraft` 校验一起做。
 * 不合法时返回一句给运营看的话；服务端仍会再判一遍。
 */
export function parseRuleDraft(draft: RuleDraft): { ok: true; config: RuleConfig } | { ok: false; msg: string } {
    const money = (label: string, s: string): number | string => {
        const v = yuanToCents(s.trim() === "" ? "0" : s);
        return v === null ? `${label}「${s}」不是合法金额（元，至多两位小数）` : v;
    };
    const minOrder = money("起送价", draft.minOrder);
    if (typeof minOrder === "string") return { ok: false, msg: minOrder };
    const freeOver = money("满多少免配送费", draft.freeOver);
    if (typeof freeOver === "string") return { ok: false, msg: freeOver };

    const tiers: DeliveryTierLike[] = [];
    for (let i = 0; i < draft.tiers.length; i += 1) {
        const t = draft.tiers[i]!;
        const withinM = kmToMeters(t.withinKm);
        if (withinM === null) return { ok: false, msg: `第 ${i + 1} 档的距离「${t.withinKm}」不是合法的公里数（至多三位小数）` };
        const feeCents = yuanToCents(t.feeYuan);
        if (feeCents === null) return { ok: false, msg: `第 ${i + 1} 档的配送费「${t.feeYuan}」不是合法金额（元，至多两位小数）` };
        tiers.push({ within_m: withinM, fee_cents: feeCents });
    }
    const bad = checkLocalDeliveryDraft(tiers, minOrder, freeOver);
    if (bad !== null) return { ok: false, msg: bad };
    return { ok: true, config: { min_order_cents: minOrder, free_over_cents: freeOver, fee_tiers: tiers } };
}
