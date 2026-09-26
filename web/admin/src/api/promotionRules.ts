// 营销活动页（契约 /admin/promotions，数据模型 §7·二）的界面规则：活动怎么显示、
// 表单怎么变成请求体。**全部是纯函数**，由 `make admin-test` 用 `node --test` 直接跑。
//
// 与 orderRules.ts 同一个约定：只有 `import type`（运行时被类型剥离抹掉）加一个同样零依赖的
// ./money.ts，这一步不需要 node_modules。
//
// ## 这里的校验只是体验，服务端才是权威
//
// 「满 100 减 200」「高一档减得更少」「秒杀没配额」「特价与折扣二选一」这些规则服务端都有
// （service/admin_promotion.go 的 validatePromotion，422 invalid-request），界面先挡一遍只是
// 为了让运营在点提交之前就看到是哪一条不对。两边的措辞尽量一致。
//
// ## 金额只做字符串换算
//
// 运营按「元」与「折」输入，换算走 money.ts 的字符串解析，没有一次浮点运算（理由写在那个文件头）。

import type { components } from "@contract/schema.js";
import { centsToYuanInput, rateToZheInput, yuanToCents, zheToRate } from "./money.ts";

type S = components["schemas"];
export type AdminPromotion = S["AdminPromotion"];
export type PromotionType = S["PromotionType"];
export type PromotionTier = S["PromotionTier"];
export type PromotionSku = S["PromotionSku"];
export type PromotionSkuInput = S["PromotionSkuInput"];
export type PromotionCreateRequest = S["PromotionCreateRequest"];
export type PromotionPatchRequest = S["PromotionPatchRequest"];
type ScopeInput = S["CouponScopeInput"];
type Phase = AdminPromotion["phase"];
type TagType = "info" | "success" | "warning" | "danger" | "primary";

export const PROMOTION_TYPE: Record<PromotionType, string> = {
    1: "满减",
    2: "满折",
    3: "限时折扣",
    4: "秒杀",
    5: "新人礼",
};

export const PHASE: Record<Phase, { text: string; tag: TagType }> = {
    offline: { text: "已下线", tag: "info" },
    scheduled: { text: "未开始", tag: "warning" },
    running: { text: "进行中", tag: "success" },
    ended: { text: "已结束", tag: "info" },
};

/** 满减满折是「阶梯类」，限时折扣 / 秒杀是「单价类」。 */
export const isTiered = (t: PromotionType): boolean => t === 1 || t === 2;
export const isPriced = (t: PromotionType): boolean => t === 3 || t === 4;

/** 分 → 给人看的元，去掉小数末尾的 0（与服务端标签一致：1990 → "19.9"）。表单回显仍用 centsToYuanInput。 */
function yuanText(cents: number): string {
    const s = centsToYuanInput(cents);
    return s.includes(".") ? s.replace(/0+$/, "").replace(/\.$/, "") : s;
}

/** 一档的标签，与服务端的商品标签同一个写法：「满100减10」「满3件减20」「满2件9折」「满200享8.5折」。 */
export function tierText(type: PromotionType, unit: number, t: PromotionTier): string {
    const th = unit === 2 ? `${t.threshold}件` : yuanText(t.threshold);
    if (type === 2) return unit === 2 ? `满${th}${rateToZheInput(t.discount_rate)}折` : `满${th}享${rateToZheInput(t.discount_rate)}折`;
    return `满${th}减${yuanText(t.discount_cents)}`;
}

/** 活动在列表上的一句话规则。 */
export function ruleSummary(p: AdminPromotion): string {
    if (isTiered(p.promotion_type)) {
        return p.tiers.length === 0 ? "（还没有阶梯，上不了线）" : p.tiers.map((t) => tierText(p.promotion_type, p.threshold_unit, t)).join("，");
    }
    if (isPriced(p.promotion_type)) {
        if (p.skus.length === 0) return "（还没有活动商品，上不了线）";
        return p.skus
            .map((s) => {
                const price = s.promo_price_cents > 0 ? `¥${yuanText(s.promo_price_cents)}` : `${rateToZheInput(s.discount_rate)}折`;
                const extra = [
                    s.stock_qty > 0 ? `配额 ${s.sold_qty}/${s.stock_qty}` : `已售 ${s.sold_qty}`,
                    s.per_user_limit > 0 ? `每人限 ${s.per_user_limit}` : "",
                ].filter((x) => x !== "");
                return `${s.title ?? `SKU #${s.sku_id}`} ${price}（${extra.join("，")}）`;
            })
            .join("；");
    }
    return `送券模板 #${p.gift_coupon_template_id ?? "?"}，已发 ${p.gift_granted_count ?? 0} 张`;
}

// ---------------------------------------------------------------- 表单

export interface TierForm {
    /** 门槛：按金额时是「元」，按件数时是件数。 */
    threshold: string;
    /** 好处：满减是「元」，满折是「折」。 */
    benefit: string;
}

export interface SkuForm {
    skuId: number | null;
    /** price：特价（元）；rate：折扣（折）。 */
    mode: "price" | "rate";
    value: string;
    perUserLimit: number;
    stockQty: number;
}

export interface PromotionForm {
    name: string;
    type: PromotionType;
    unit: 1 | 2;
    stack: boolean;
    range: [Date, Date] | null;
    tiers: TierForm[];
    scopes: ScopeInput[];
    skus: SkuForm[];
    giftTemplateId: number | null;
}

export function emptyPromotionForm(): PromotionForm {
    return {
        name: "",
        type: 1,
        unit: 1,
        stack: true,
        range: null,
        tiers: [{ threshold: "", benefit: "" }],
        scopes: [],
        skus: [],
        giftTemplateId: null,
    };
}

export function formOfPromotion(p: AdminPromotion): PromotionForm {
    return {
        name: p.name,
        type: p.promotion_type,
        unit: p.threshold_unit === 2 ? 2 : 1,
        stack: p.stack_with_coupon,
        range: [new Date(p.starts_at), new Date(p.ends_at)],
        tiers: p.tiers.map((t) => ({
            threshold: p.threshold_unit === 2 ? String(t.threshold) : centsToYuanInput(t.threshold),
            benefit: p.promotion_type === 2 ? rateToZheInput(t.discount_rate) : centsToYuanInput(t.discount_cents),
        })),
        scopes: p.scopes.map((s) => ({ scope_type: s.scope_type, target_id: s.target_id, include: s.include })),
        skus: p.skus.map((s) => ({
            skuId: s.sku_id,
            mode: s.promo_price_cents > 0 ? "price" : "rate",
            value: s.promo_price_cents > 0 ? centsToYuanInput(s.promo_price_cents) : rateToZheInput(s.discount_rate),
            perUserLimit: s.per_user_limit,
            stockQty: s.stock_qty,
        })),
        giftTemplateId: p.gift_coupon_template_id ?? null,
    };
}

/** 表单里「规则」那一半（名称、类型另拼）。新建与修改共用。 */
export type RuleBody = Pick<
    PromotionCreateRequest,
    "threshold_unit" | "stack_with_coupon" | "starts_at" | "ends_at" | "tiers" | "scopes" | "skus" | "gift_coupon_template_id"
>;

export type Built = { ok: true; body: RuleBody } | { ok: false; msg: string };

/**
 * 把表单换算成请求体的规则部分。任何一项不合法都返回一句给运营看的话。
 *
 * 阶梯类允许零档（存成草稿，上线时服务端再核完整性）；其余检查与服务端逐条对应。
 */
export function buildRules(f: PromotionForm): Built {
    const bad = (msg: string): Built => ({ ok: false, msg });
    if (f.range === null) return bad("请选择活动时间");
    const [start, end] = f.range;
    if (!(end.getTime() > start.getTime())) return bad("结束时间必须晚于开始时间");
    const body: RuleBody = {
        stack_with_coupon: f.stack,
        starts_at: start.toISOString(),
        ends_at: end.toISOString(),
        threshold_unit: 0,
        tiers: [],
        skus: [],
        scopes: f.scopes,
    };

    if (isTiered(f.type)) {
        body.threshold_unit = f.unit;
        const tiers: PromotionTier[] = [];
        for (const [i, t] of f.tiers.entries()) {
            if (t.threshold.trim() === "" && t.benefit.trim() === "") continue; // 空行忽略
            let threshold: number | null;
            if (f.unit === 2) threshold = /^\d+$/.test(t.threshold.trim()) ? Number(t.threshold.trim()) : null;
            else threshold = yuanToCents(t.threshold);
            if (threshold === null || threshold <= 0) {
                return bad(`第 ${i + 1} 档：门槛「${t.threshold}」不对（${f.unit === 2 ? "填正整数件数" : "填元，至多两位小数"}）`);
            }
            if (f.type === 1) {
                const off = yuanToCents(t.benefit);
                if (off === null || off <= 0) return bad(`第 ${i + 1} 档：减免「${t.benefit}」不对（填大于 0 的元）`);
                if (f.unit === 1 && threshold < off) return bad(`第 ${i + 1} 档：满 ${t.threshold} 减 ${t.benefit}，减的比门槛还多`);
                tiers.push({ threshold, discount_cents: off, discount_rate: 0 });
            } else {
                const rate = zheToRate(t.benefit);
                if (rate === null) return bad(`第 ${i + 1} 档：折扣「${t.benefit}」不对（填 0.1 到 9.99 之间的折数）`);
                tiers.push({ threshold, discount_cents: 0, discount_rate: rate });
            }
        }
        tiers.sort((a, b) => a.threshold - b.threshold);
        for (let i = 1; i < tiers.length; i += 1) {
            const a = tiers[i - 1]!;
            const b = tiers[i]!;
            if (a.threshold === b.threshold) return bad("两档的门槛一样了");
            if (f.type === 1 && b.discount_cents <= a.discount_cents) return bad("门槛更高的一档，减免必须更多");
            if (f.type === 2 && b.discount_rate >= a.discount_rate) return bad("门槛更高的一档，折扣必须更低");
        }
        body.tiers = tiers;
    } else if (isPriced(f.type)) {
        const skus: PromotionSkuInput[] = [];
        const seen = new Set<number>();
        for (const [i, s] of f.skus.entries()) {
            if (s.skuId === null) return bad(`第 ${i + 1} 个活动商品：请选择 SKU`);
            if (seen.has(s.skuId)) return bad(`SKU #${s.skuId} 出现了两次`);
            seen.add(s.skuId);
            const item: PromotionSkuInput = { sku_id: s.skuId, per_user_limit: s.perUserLimit, stock_qty: 0 };
            if (s.mode === "price") {
                const price = yuanToCents(s.value);
                if (price === null || price <= 0) return bad(`SKU #${s.skuId}：特价「${s.value}」不对（填大于 0 的元）`);
                item.promo_price_cents = price;
                item.discount_rate = 0;
            } else {
                const rate = zheToRate(s.value);
                if (rate === null) return bad(`SKU #${s.skuId}：折扣「${s.value}」不对（填 0.1 到 9.99 之间的折数）`);
                item.discount_rate = rate;
                item.promo_price_cents = 0;
            }
            if (f.type === 4) {
                if (!(s.stockQty > 0)) return bad(`SKU #${s.skuId}：秒杀必须给活动配额（件）`);
                item.stock_qty = s.stockQty;
            }
            skus.push(item);
        }
        for (const sc of f.scopes) {
            if (sc.scope_type !== 5 && sc.scope_type !== 6) return bad("限时折扣 / 秒杀的范围只能按大区或门店限定（商品已经由活动商品点名）");
        }
        body.skus = skus;
    } else {
        if (f.giftTemplateId === null || !(f.giftTemplateId > 0)) return bad("新人礼要选一批券");
        if (f.scopes.length > 0) return bad("新人礼没有适用范围");
        body.gift_coupon_template_id = f.giftTemplateId;
        body.scopes = [];
    }
    return { ok: true, body };
}
