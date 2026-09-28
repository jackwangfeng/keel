// AI 员工提案（AI 经营 M9/M10，docs/AI经营-M10M11设计.md §1/§4）的界面规则：
// 提案种类的显示名、payload / result / outcome 按种类翻译成人话。**全部是纯函数**，
// 由 `make admin-test` 用 `node --test` 直接跑。
//
// 与 promotionRules.ts / orderRules.ts 同一个约定：刻意只有 `import type`（运行时被类型
// 剥离抹掉）加两个同样零依赖的 ./money.ts、./orderRules.ts，这一步不需要 node_modules。
//
// ## payload / result / outcome 都是「服务端写的、但形状随种类变」的 `{[key:string]: unknown}`
//
// 字段名与后端 internal/service/agent_proposal_kinds.go、agent_proposal_outcome.go 逐个对齐
// （M10/M11 设计文档 §1 的执行参数表、§4 的 outcome 字段表），但**按不可信输入取值**：
// 类型不对 / 字段缺失一律退到「—」，不猜、不抛错——AI 员工写错了形状，这里也不能崩。

import type { components } from "@contract/schema.js";
import { REFUND_STATUS } from "./orderRules.ts";
import { rateLabel, centsToYuanInput } from "./money.ts";

type S = components["schemas"];
export type AgentProposalKind = S["AgentProposal"]["kind"];
export type AgentOutcomeVerdict = "positive" | "neutral" | "negative";
type TagType = "info" | "success" | "warning" | "danger" | "primary";
type Rec = Record<string, unknown>;

const DASH = "—";

/** 提案种类的显示名（契约 AgentProposal.kind）。`Record<AgentProposalKind, …>` 让契约多一种时这里编译不过。 */
export const KIND_LABEL: Record<AgentProposalKind, string> = {
    inventory_adjust: "加库存",
    flash_price: "限时折扣",
    coupon: "发券",
    product_copy: "改文案",
    refund_decision: "售后审核",
};

/** 执行后复盘的结论（outcome.verdict，00122）。 */
export const VERDICT: Record<AgentOutcomeVerdict, { text: string; tag: TagType }> = {
    positive: { text: "正面", tag: "success" },
    neutral: { text: "中性", tag: "info" },
    negative: { text: "负面", tag: "danger" },
};

function str(v: unknown): string | undefined {
    return typeof v === "string" && v !== "" ? v : undefined;
}
function num(v: unknown): number | undefined {
    return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}
function bool(v: unknown): boolean | undefined {
    return typeof v === "boolean" ? v : undefined;
}
function arr(v: unknown): unknown[] {
    return Array.isArray(v) ? v : [];
}
function rec(v: unknown): Rec {
    return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {};
}
function yuanText(cents: number): string {
    return `¥${centsToYuanInput(cents)}`;
}

// --------------------------------------------------------------- payload：加库存

export interface InventoryAdjustView {
    storeText: string;
    skuText: string;
    deltaText: string;
    reason?: string;
}

/** `inventory_adjust` 执行参数：{store_id, sku_id, delta, reason}（M9）。 */
export function inventoryAdjustPayload(payload: Rec): InventoryAdjustView {
    const storeId = num(payload["store_id"]);
    const skuId = num(payload["sku_id"]);
    const delta = num(payload["delta"]);
    return {
        storeText: storeId === undefined ? DASH : `门店 #${storeId}`,
        skuText: skuId === undefined ? DASH : `SKU #${skuId}`,
        deltaText: delta === undefined ? DASH : `${delta >= 0 ? "+" : ""}${delta} 件`,
        reason: str(payload["reason"]),
    };
}

// --------------------------------------------------------------- payload：限时折扣

export interface FlashPriceItemView {
    skuId: number;
    discountText: string;
}

export interface FlashPriceView {
    name: string;
    storeText: string;
    items: FlashPriceItemView[];
    /** ISO 时间，交给 ui/format.ts 的 datetime() 显示——这里不重复实现时区格式化。 */
    startsAt?: string;
    endsAt?: string;
}

/** `flash_price` 执行参数：{name, store_id?, items:[{sku_id, discount_rate}], starts_at, ends_at}（M10）。 */
export function flashPricePayload(payload: Rec): FlashPriceView {
    const items = arr(payload["items"]).map((it) => {
        const r = rec(it);
        const skuId = num(r["sku_id"]) ?? 0;
        const rate = num(r["discount_rate"]);
        return { skuId, discountText: rate === undefined ? DASH : rateLabel(rate) };
    });
    const storeId = num(payload["store_id"]);
    return {
        name: str(payload["name"]) ?? DASH,
        storeText: storeId === undefined ? "全店" : `门店 #${storeId}`,
        items,
        startsAt: str(payload["starts_at"]),
        endsAt: str(payload["ends_at"]),
    };
}

// --------------------------------------------------------------- payload：发券

const COUPON_TYPE_LABEL: Record<number, string> = { 1: "满减", 2: "折扣", 3: "立减" };

export interface CouponView {
    name: string;
    typeText: string;
    thresholdText: string;
    faceText: string;
    validDaysText: string;
    totalText: string;
    perUserLimitText: string;
    claimableText: string;
}

/**
 * `coupon` 执行参数：{name, coupon_type, threshold_cents, discount_cents, discount_rate,
 * max_discount_cents, valid_days, total_count, per_user_limit, claimable}（M10）。
 */
export function couponPayload(payload: Rec): CouponView {
    const type = num(payload["coupon_type"]);
    const thresholdCents = num(payload["threshold_cents"]);
    const discountCents = num(payload["discount_cents"]);
    const discountRate = num(payload["discount_rate"]);
    const maxDiscountCents = num(payload["max_discount_cents"]);
    const validDays = num(payload["valid_days"]);
    const totalCount = num(payload["total_count"]);
    const perUserLimit = num(payload["per_user_limit"]);
    const claimable = bool(payload["claimable"]);

    let faceText = DASH;
    if (type === 2 && discountRate !== undefined) {
        faceText = rateLabel(discountRate);
        if (maxDiscountCents !== undefined && maxDiscountCents > 0) faceText += `，封顶 ${yuanText(maxDiscountCents)}`;
    } else if (discountCents !== undefined) {
        faceText = yuanText(discountCents);
    }

    return {
        name: str(payload["name"]) ?? DASH,
        typeText: type === undefined ? DASH : (COUPON_TYPE_LABEL[type] ?? `类型 ${type}`),
        thresholdText: thresholdCents === undefined || thresholdCents <= 0 ? "无门槛" : `满 ${yuanText(thresholdCents)}`,
        faceText,
        validDaysText: validDays === undefined ? DASH : `领后 ${validDays} 天内有效`,
        totalText: totalCount === undefined ? DASH : `${totalCount} 张`,
        perUserLimitText: perUserLimit === undefined ? DASH : `每人限 ${perUserLimit} 张`,
        claimableText: claimable === undefined ? DASH : claimable ? "进领券中心" : "不进领券中心",
    };
}

// --------------------------------------------------------------- payload：改文案

export interface ProductCopyDiffView {
    productId?: number;
    beforeTitle: string;
    afterTitle?: string;
    beforeSubtitle?: string;
    afterSubtitle?: string;
    titleChanged: boolean;
    subtitleChanged: boolean;
}

/** `product_copy` 执行参数：{product_id, title?, subtitle?, before_title, before_subtitle?}（M10）。 */
export function productCopyPayload(payload: Rec): ProductCopyDiffView {
    const afterTitle = str(payload["title"]);
    const afterSubtitle = str(payload["subtitle"]);
    return {
        productId: num(payload["product_id"]),
        beforeTitle: str(payload["before_title"]) ?? DASH,
        afterTitle,
        beforeSubtitle: str(payload["before_subtitle"]),
        afterSubtitle,
        titleChanged: afterTitle !== undefined,
        subtitleChanged: afterSubtitle !== undefined,
    };
}

// --------------------------------------------------------------- payload：售后审核

export interface RefundDecisionView {
    refundNo: string;
    actionText: string;
    reason?: string;
    amountText: string;
}

/** `refund_decision` 执行参数：{refund_no, action: "approve"|"reject", reject_reason?, amount_cents}（M10）。 */
export function refundDecisionPayload(payload: Rec): RefundDecisionView {
    const action = str(payload["action"]);
    const amount = num(payload["amount_cents"]);
    return {
        refundNo: str(payload["refund_no"]) ?? DASH,
        actionText: action === "approve" ? "同意" : action === "reject" ? "驳回" : (action ?? DASH),
        reason: str(payload["reject_reason"]),
        amountText: amount === undefined ? DASH : yuanText(amount),
    };
}

// --------------------------------------------------------------- result

export interface ResultLine {
    label: string;
    value: string;
}

export interface ResultView {
    /** 加库存（M9）：可售库存前后。 */
    beforeAfter?: { before: number; after: number };
    /** M10 各种类的 `result.detail`。 */
    lines: ResultLine[];
    /** `result.detail.promotion_id` 存在时给「营销活动」列表页的入口（没有按 id 的详情路由，只能链去列表）。 */
    hasPromotionLink: boolean;
    /** `result.detail.coupon_template_id` 存在时给「优惠券」列表页的入口，同上。 */
    hasCouponLink: boolean;
    /** 失败结果：{error_type, error}。 */
    failure?: { errorType?: string; error: string };
}

/** `AgentProposal.result`：加库存是 {before_available, after_available}；M10 是 {detail: {...}}；失败是 {error_type, error}。 */
export function describeResult(kind: AgentProposalKind, result: Rec | undefined): ResultView {
    if (result === undefined) return { lines: [], hasPromotionLink: false, hasCouponLink: false };

    const error = str(result["error"]);
    if (error !== undefined) {
        return { lines: [], hasPromotionLink: false, hasCouponLink: false, failure: { errorType: str(result["error_type"]), error } };
    }

    if (kind === "inventory_adjust") {
        const before = num(result["before_available"]);
        const after = num(result["after_available"]);
        if (before !== undefined && after !== undefined) {
            return { lines: [], hasPromotionLink: false, hasCouponLink: false, beforeAfter: { before, after } };
        }
        return { lines: [], hasPromotionLink: false, hasCouponLink: false };
    }

    const detail = rec(result["detail"]);
    const lines: ResultLine[] = [];
    const promotionId = num(detail["promotion_id"]);
    const couponTemplateId = num(detail["coupon_template_id"]);
    if (promotionId !== undefined) lines.push({ label: "活动", value: `#${promotionId}` });
    if (couponTemplateId !== undefined) lines.push({ label: "券模板", value: `#${couponTemplateId}` });
    const beforeTitle = str(detail["before_title"]);
    const afterTitle = str(detail["after_title"]);
    if (beforeTitle !== undefined || afterTitle !== undefined) {
        lines.push({ label: "标题", value: `${beforeTitle ?? DASH} → ${afterTitle ?? DASH}` });
    }
    const afterSubtitle = str(detail["after_subtitle"]);
    if (afterSubtitle !== undefined) lines.push({ label: "副标题", value: afterSubtitle });
    const refundNo = str(detail["refund_no"]);
    const refundStatus = num(detail["refund_status"]);
    if (refundNo !== undefined) {
        const statusText = refundStatus !== undefined && refundStatus in REFUND_STATUS
            ? REFUND_STATUS[refundStatus as keyof typeof REFUND_STATUS].text
            : undefined;
        lines.push({ label: "售后单", value: `${refundNo}${statusText !== undefined ? ` · ${statusText}` : ""}` });
    }
    return { lines, hasPromotionLink: promotionId !== undefined, hasCouponLink: couponTemplateId !== undefined };
}

// --------------------------------------------------------------- outcome（执行后复盘，00122）

export interface OutcomeMetricView {
    label: string;
    value: string;
}

export interface OutcomeView {
    verdict?: AgentOutcomeVerdict;
    explanation?: string;
    metrics: OutcomeMetricView[];
}

const OUTCOME_METRIC_FIELDS: { key: string; label: string; format?: (v: number) => string }[] = [
    { key: "sold_qty", label: "期间销量" },
    { key: "stockout_days", label: "断货天数", format: (v) => `${v} 天` },
    { key: "delta_qty", label: "补货件数", format: (v) => `${v} 件` },
    { key: "qty_during", label: "期间销量" },
    { key: "qty_before", label: "对照期销量" },
    { key: "qty_after", label: "执行后销量" },
    { key: "amount_during_cents", label: "期间销售额", format: (v) => yuanText(v) },
    { key: "claimed", label: "领取数" },
    { key: "used", label: "核销数" },
    { key: "use_rate", label: "核销率", format: (v) => `${(v * 100).toFixed(1)}%` },
];

/** `AgentProposal.outcome`：{verdict, explanation, 各种类的指标…}（00122，字段表见 docs/AI经营-M10M11设计.md §4）。 */
export function describeOutcome(outcome: Rec | undefined): OutcomeView {
    if (outcome === undefined) return { metrics: [] };
    const verdict = str(outcome["verdict"]);
    const metrics: OutcomeMetricView[] = [];
    for (const f of OUTCOME_METRIC_FIELDS) {
        const v = num(outcome[f.key]);
        if (v === undefined) continue;
        metrics.push({ label: f.label, value: f.format ? f.format(v) : String(v) });
    }
    return {
        verdict: verdict === "positive" || verdict === "neutral" || verdict === "negative" ? verdict : undefined,
        explanation: str(outcome["explanation"]),
        metrics,
    };
}

// --------------------------------------------------------------- 成绩单（安全除法）

/** a/b 的百分比文本，b 为 0 时给「—」而不是 NaN / Infinity——没提过案的员工不该显示「0%」（那暗示「提了但没批」）。 */
export function safeRatePercent(a: number, b: number): string {
    if (b <= 0) return DASH;
    return `${((a / b) * 100).toFixed(0)}%`;
}
