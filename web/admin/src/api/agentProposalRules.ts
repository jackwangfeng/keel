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
    channel_stock_rule: "调渠道分配",
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
    /** 精准发放（2026-10-16）：发给哪群人、留多少对照；普通的券没有这一行。 */
    targetText?: string;
}

const SEGMENT_LABEL: Record<string, string> = {
    lapsed: "沉睡顾客",
    one_time: "只买过一次的顾客",
    cart_abandon: "加购未买的顾客",
    high_value: "高价值顾客",
};

/**
 * `coupon` 执行参数：{name, coupon_type, threshold_cents, discount_cents, discount_rate,
 * max_discount_cents, valid_days | valid_start_at + valid_end_at, total_count, per_user_limit, claimable}（M10）。
 * 有效期二选一：领后 N 天，或固定时段（2026-09-28 起；结束不含）。
 */
// 规则文件不引 ui/format.ts（那边连着 API 客户端，node --test 下解析不了）：同样的写法就地一份。
function localTime(value: string): string {
    const t = new Date(value);
    return Number.isNaN(t.getTime()) ? value : t.toLocaleString("zh-CN", { hour12: false });
}

export function couponPayload(payload: Rec): CouponView {
    const type = num(payload["coupon_type"]);
    const thresholdCents = num(payload["threshold_cents"]);
    const discountCents = num(payload["discount_cents"]);
    const discountRate = num(payload["discount_rate"]);
    const maxDiscountCents = num(payload["max_discount_cents"]);
    const validDays = num(payload["valid_days"]);
    const validStart = str(payload["valid_start_at"]);
    const validEnd = str(payload["valid_end_at"]);
    const totalCount = num(payload["total_count"]);
    const perUserLimit = num(payload["per_user_limit"]);
    const claimable = bool(payload["claimable"]);
    const target = payload["target"] as Rec | undefined;
    let targetText: string | undefined;
    if (target && typeof target === "object") {
        const seg = str(target["segment"]) ?? "";
        const size = num(target["estimated_size"]);
        const holdout = num(target["holdout_pct"]);
        targetText = `定向发给「${SEGMENT_LABEL[seg] ?? seg}」${size === undefined ? "" : `约 ${size} 人`}` +
            (holdout === undefined ? "" : `，随机留 ${holdout}% 不发作对照`);
    }

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
        validDaysText:
            validStart !== undefined && validEnd !== undefined
                ? `${localTime(validStart)} 至 ${localTime(validEnd)} 可用`
                : validDays === undefined
                  ? DASH
                  : `领后 ${validDays} 天内有效`,
        totalText: totalCount === undefined ? DASH : `${totalCount} 张`,
        perUserLimitText: perUserLimit === undefined ? DASH : `每人限 ${perUserLimit} 张`,
        claimableText: claimable === undefined ? DASH : claimable ? "进领券中心" : "不进领券中心",
        targetText,
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
    /** 上新整理（2026-10-16）：挪类目。名字是提案那一刻的快照。 */
    categoryChanged: boolean;
    beforeCategory: string;
    afterCategory?: string;
}

/**
 * `product_copy` 执行参数：{product_id, title?, subtitle?, before_title, before_subtitle?}（M10），
 * 上新整理起另有 {category_id?, category_name?, before_category_id?, before_category_name?}。
 */
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
        categoryChanged: num(payload["category_id"]) !== undefined,
        beforeCategory: str(payload["before_category_name"]) || "（未分类）",
        afterCategory: str(payload["category_name"]),
    };
}

// --------------------------------------------------------------- payload：调渠道分配

export interface ChannelStockRuleChangeView {
    /** 「门店级」或「SKU #id」。 */
    cellText: string;
    prevText: string;
    nextText: string;
}

export interface ChannelStockPreviewView {
    skuText: string;
    availableText: string;
    beforeText: string;
    afterText: string;
}

export interface ChannelStockRuleView {
    bindingText: string;
    storeText: string;
    changes: ChannelStockRuleChangeView[];
    /** 提案时的试算：这些 SKU 的对外可售数从几变成几（至多 40 格，§4.3）。 */
    preview: ChannelStockPreviewView[];
}

/** ratio_bp（万分比，10000 = 100%）→ "80%" / "80.5%"。 */
function bpPercent(bp: number): string {
    return `${parseFloat((bp / 100).toFixed(2))}%`;
}

function capQtyText(capQty: number | undefined): string {
    return capQty === undefined ? "不封顶" : `封顶 ${capQty}`;
}

function channelRuleText(ratioBp: number | undefined, safetyQty: number | undefined, capQty: number | undefined): string {
    if (ratioBp === undefined || safetyQty === undefined) return DASH;
    return `比例 ${bpPercent(ratioBp)} · 安全库存 ${safetyQty} 件 · ${capQtyText(capQty)}`;
}

/**
 * `channel_stock_rule` 执行参数：{binding_id, binding_name, store_id, store_name,
 * changes: [{sku_id?（空 = 门店级）, ratio_bp, safety_qty, cap_qty?, prev: {ratio_bp, safety_qty, cap_qty?, level}}],
 * preview: [{sku_id, available, before_qty, after_qty}]}（spec §4.2–4.3）。
 *
 * payload 里商品只有 `sku_id`，没有标题（与 `flash_price` 的 items 一致，这里同样直接显示
 * 「SKU #id」，不额外拉取商品目录）。
 */
export function channelStockRulePayload(payload: Rec): ChannelStockRuleView {
    const bindingName = str(payload["binding_name"]);
    const bindingId = num(payload["binding_id"]);
    const storeName = str(payload["store_name"]);
    const storeId = num(payload["store_id"]);

    const changes = arr(payload["changes"]).map((it) => {
        const r = rec(it);
        const skuId = num(r["sku_id"]);
        const prev = rec(r["prev"]);
        return {
            cellText: skuId === undefined ? "门店级" : `SKU #${skuId}`,
            prevText: channelRuleText(num(prev["ratio_bp"]), num(prev["safety_qty"]), num(prev["cap_qty"])),
            nextText: channelRuleText(num(r["ratio_bp"]), num(r["safety_qty"]), num(r["cap_qty"])),
        };
    });

    const preview = arr(payload["preview"]).map((it) => {
        const r = rec(it);
        const skuId = num(r["sku_id"]);
        const available = num(r["available"]);
        const beforeQty = num(r["before_qty"]);
        const afterQty = num(r["after_qty"]);
        return {
            skuText: skuId === undefined ? DASH : `SKU #${skuId}`,
            availableText: available === undefined ? DASH : `${available} 件`,
            beforeText: beforeQty === undefined ? DASH : `${beforeQty} 件`,
            afterText: afterQty === undefined ? DASH : `${afterQty} 件`,
        };
    });

    return {
        bindingText: bindingName ?? (bindingId === undefined ? DASH : `渠道 #${bindingId}`),
        storeText: storeName ?? (storeId === undefined ? DASH : `门店 #${storeId}`),
        changes,
        preview,
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
    if (kind === "channel_stock_rule") {
        const applied = num(detail["applied"]);
        const already = num(detail["already"]);
        if (applied !== undefined) {
            lines.push({ label: "已生效", value: `${applied} 格${already !== undefined && already > 0 ? `（另有 ${already} 格本来就是目标值）` : ""}` });
        }
    }
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

/** `channel_stock_rule` 复盘的一格（spec §4.5）：执行前后各 7 天，挂零小时（分配造成的，不含 keel 自己断货）、
 * 缺货拒单、卖出、渠道净收入。`excludedReason` 非空 = 这一格窗口里 keel 自己断货超过 2 天，不计进判据。 */
export interface ChannelOutcomeCellView {
    skuText: string;
    directionText: string;
    heldZeroText: string;
    rejectsText: string;
    soldText: string;
    netText: string;
    excludedReason?: string;
}

export interface OutcomeView {
    verdict?: AgentOutcomeVerdict;
    explanation?: string;
    metrics: OutcomeMetricView[];
    /** 只有 `channel_stock_rule` 才有：逐格的前后对比表。 */
    cells: ChannelOutcomeCellView[];
}

const DIRECTION_LABEL: Record<string, string> = { up: "上调", down: "下调", same: "不变" };

function channelOutcomeCells(outcome: Rec): ChannelOutcomeCellView[] {
    return arr(outcome["cells"]).map((it) => {
        const c = rec(it);
        const skuId = num(c["sku_id"]);
        const direction = str(c["direction"]);
        const before = rec(c["before"]);
        const after = rec(c["after"]);
        const hours = (v: number | undefined) => (v === undefined ? DASH : `${v} 小时`);
        const count = (v: number | undefined) => (v === undefined ? DASH : `${v}`);
        const net = (v: number | undefined) => (v === undefined ? DASH : yuanText(v));
        return {
            skuText: skuId === undefined ? DASH : `SKU #${skuId}`,
            directionText: direction !== undefined && direction in DIRECTION_LABEL ? DIRECTION_LABEL[direction] : DASH,
            heldZeroText: `${hours(num(before["held_zero_hours"]))} → ${hours(num(after["held_zero_hours"]))}`,
            rejectsText: `${count(num(before["stockout_rejects"]))} → ${count(num(after["stockout_rejects"]))}`,
            soldText: `${count(num(before["sold"]))} → ${count(num(after["sold"]))}`,
            netText: `${net(num(before["net_cents"]))} → ${net(num(after["net_cents"]))}`,
            excludedReason: str(c["excluded_reason"]),
        };
    });
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
    // 补货复盘（2026-10-16）
    { key: "forecast_qty", label: "预测销量", format: (v) => `${v} 件` },
    { key: "forecast_accuracy", label: "预测准确率", format: (v) => `${Math.round(v * 100)}%` },
    { key: "before_available", label: "执行前可售", format: (v) => `${v} 件` },
    { key: "counterfactual_stockout_days", label: "不补会断", format: (v) => `${v} 天` },
    { key: "avoided_stockout_days", label: "少断货", format: (v) => `${v} 天` },
    { key: "extra_sold_qty", label: "多卖", format: (v) => `${v} 件` },
    { key: "extra_sold_cents", label: "多卖金额", format: (v) => yuanText(v) },
    // 精准发券的对照实验（2026-10-16）
    { key: "treated_size", label: "发券组", format: (v) => `${v} 人` },
    { key: "holdout_size", label: "对照组", format: (v) => `${v} 人` },
    { key: "treated_conversion", label: "发券组下单率", format: (v) => `${(v * 100).toFixed(1)}%` },
    { key: "holdout_conversion", label: "对照组下单率", format: (v) => `${(v * 100).toFixed(1)}%` },
    { key: "lift", label: "提升", format: (v) => `${(v * 100).toFixed(1)} 个百分点` },
    { key: "incremental_buyers", label: "增量顾客", format: (v) => `约 ${v} 人` },
    { key: "incremental_cents", label: "增量金额", format: (v) => yuanText(v) },
    { key: "coupon_cost_cents", label: "券成本", format: (v) => yuanText(v) },
];

/**
 * `AgentProposal.outcome`：{verdict, explanation, 各种类的指标…}（00122，字段表见 docs/AI经营-M10M11设计.md §4）；
 * `channel_stock_rule`（spec §4.5）另有 `cells`，逐格前后对比，不走通用的扁平指标列表。
 */
export function describeOutcome(kind: AgentProposalKind, outcome: Rec | undefined): OutcomeView {
    if (outcome === undefined) return { metrics: [], cells: [] };
    const verdict = str(outcome["verdict"]);
    const verdictOut = verdict === "positive" || verdict === "neutral" || verdict === "negative" ? verdict : undefined;
    const explanation = str(outcome["explanation"]);
    if (kind === "channel_stock_rule") {
        return { verdict: verdictOut, explanation, metrics: [], cells: channelOutcomeCells(outcome) };
    }
    const metrics: OutcomeMetricView[] = [];
    for (const f of OUTCOME_METRIC_FIELDS) {
        const v = num(outcome[f.key]);
        if (v === undefined) continue;
        metrics.push({ label: f.label, value: f.format ? f.format(v) : String(v) });
    }
    return { verdict: verdictOut, explanation, metrics, cells: [] };
}

// --------------------------------------------------------------- 成绩单（安全除法）

/** a/b 的百分比文本，b 为 0 时给「—」而不是 NaN / Infinity——没提过案的员工不该显示「0%」（那暗示「提了但没批」）。 */
export function safeRatePercent(a: number, b: number): string {
    if (b <= 0) return DASH;
    return `${((a / b) * 100).toFixed(0)}%`;
}

// --------------------------------------------------------------- 成绩单：补货的能讲的数字

/** 成绩单里补货汇总（契约 AgentScorecardRestock）的展示文字；口径写在契约那一段说明里。 */
export interface RestockSummaryView {
    headline: string;
    accuracy: string;
    avoided: string;
    extra: string;
    footnote: string;
}

export function restockSummary(r: {
    reviewed: number;
    with_forecast: number;
    avg_forecast_accuracy?: number;
    avoided_stockout_days: number;
    extra_sold_qty: number;
    extra_sold_cents: number;
}): RestockSummaryView {
    const acc = r.avg_forecast_accuracy;
    return {
        headline: `已复盘 ${r.reviewed} 条补货提案`,
        accuracy: acc === undefined ? "预测准确率：—（这批提案没有测算快照）" : `平均预测准确率 ${Math.round(acc * 100)}%（${r.with_forecast} 条）`,
        avoided: `少断货 ${r.avoided_stockout_days} 天`,
        extra: `多卖 ${r.extra_sold_qty} 件，约 ¥${(r.extra_sold_cents / 100).toFixed(2)}`,
        footnote: "「少断货」「多卖」按「不补的话，执行前的库存按实际卖速会在哪天卖完」推算，前提是需求不因补货而变。",
    };
}
