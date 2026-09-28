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
// 刻意零运行时 import：`make admin-test` 用 `node --test` 直接跑这个文件，
// 不需要 node_modules，与 geo.ts / money.ts / freightRules.ts 同一个约定。

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
