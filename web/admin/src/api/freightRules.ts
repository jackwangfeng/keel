// 运费模板编辑页的纯逻辑：省级行政区清单、一条规则给人看的样子、提交前的本地校验。
//
// 本地校验只是体验 —— 服务端（service/admin_freight.go 的 validateFreightTemplate）
// 是唯一的执行者，这里把同样的几条规矩先说一遍，让运营不必提交之后才看到 422：
// 恰好一条默认规则、一个省只能出现一次（规则之间、规则与不配送之间都算）。
//
// **运费怎么算不在这里**：计算只有服务端一份实现（service/freight_calc.go），
// 界面只展示规则，不重算金额 —— 两份实现迟早会在进位上分叉。
//
// 只 import 同目录的 money.ts（它本身零 import）：`make admin-test` 用 `node --test`
// 直接跑这个文件，不需要 node_modules，与 geo.ts / money.ts 同一个约定。

import { centsToYuanInput } from "./money.ts";

/** 34 个省级行政区（GB/T 2260 的 6 位码）。与 service/freight_region.go 是同一份清单。 */
export const PROVINCES: ReadonlyArray<{ code: string; name: string }> = [
    { code: "110000", name: "北京" },
    { code: "120000", name: "天津" },
    { code: "130000", name: "河北" },
    { code: "140000", name: "山西" },
    { code: "150000", name: "内蒙古" },
    { code: "210000", name: "辽宁" },
    { code: "220000", name: "吉林" },
    { code: "230000", name: "黑龙江" },
    { code: "310000", name: "上海" },
    { code: "320000", name: "江苏" },
    { code: "330000", name: "浙江" },
    { code: "340000", name: "安徽" },
    { code: "350000", name: "福建" },
    { code: "360000", name: "江西" },
    { code: "370000", name: "山东" },
    { code: "410000", name: "河南" },
    { code: "420000", name: "湖北" },
    { code: "430000", name: "湖南" },
    { code: "440000", name: "广东" },
    { code: "450000", name: "广西" },
    { code: "460000", name: "海南" },
    { code: "500000", name: "重庆" },
    { code: "510000", name: "四川" },
    { code: "520000", name: "贵州" },
    { code: "530000", name: "云南" },
    { code: "540000", name: "西藏" },
    { code: "610000", name: "陕西" },
    { code: "620000", name: "甘肃" },
    { code: "630000", name: "青海" },
    { code: "640000", name: "宁夏" },
    { code: "650000", name: "新疆" },
    { code: "710000", name: "台湾" },
    { code: "810000", name: "香港" },
    { code: "820000", name: "澳门" },
];

const NAME = new Map(PROVINCES.map((p) => [p.code, p.name]));

/** 区划码 → 省名；不认识的码原样返回。 */
export function provinceName(code: string): string {
    return NAME.get(code) ?? code;
}

/** 「北京、天津、河北」；空数组给「其余地区」（默认规则）。 */
export function regionsLabel(codes: readonly string[]): string {
    return codes.length === 0 ? "其余地区（默认）" : codes.map(provinceName).join("、");
}

/** 计费方式 1 按件 / 2 按重量。 */
export type ChargeMode = 1 | 2;

/** 一条规则的最小形状（契约 FreightRule 的子集，免得这里 import 契约类型）。 */
export interface RuleLike {
    region_codes: string[];
    first_unit: number;
    first_fee_cents: number;
    additional_unit: number;
    additional_fee_cents: number;
    free_threshold_cents: number;
    free_quantity: number;
}

/** 首件 / 首重的量怎么念：按件「2 件」，按重量「1.5 kg」或「500 g」。 */
export function unitText(mode: ChargeMode, n: number): string {
    if (mode === 1) return `${n} 件`;
    return n >= 1000 && n % 100 === 0 ? `${n / 1000} kg` : `${n} g`;
}

/** 分 → 「8 元」「8.50 元」。 */
function yuanText(cents: number): string {
    return `${centsToYuanInput(cents)} 元`;
}

/** 一条规则给人看的一句话：「首件 8 元，每续 1 件 2 元；满 99 元包邮」。 */
export function ruleSummary(mode: ChargeMode, r: RuleLike): string {
    const first = mode === 1 ? "首" : "首重";
    const next = mode === 1 ? "续" : "续重";
    const head =
        mode === 1
            ? `${first}${r.first_unit === 1 ? "件" : ` ${r.first_unit} 件`} ${yuanText(r.first_fee_cents)}`
            : `${first} ${unitText(mode, r.first_unit)} ${yuanText(r.first_fee_cents)}`;
    const tail = `每${next} ${unitText(mode, r.additional_unit)} ${yuanText(r.additional_fee_cents)}`;
    const free: string[] = [];
    if (r.free_threshold_cents > 0) free.push(`满 ${yuanText(r.free_threshold_cents)}包邮`);
    if (r.free_quantity > 0) free.push(`满 ${r.free_quantity} 件包邮`);
    return `${head}，${tail}${free.length > 0 ? `；${free.join("或")}` : ""}`;
}

/**
 * 提交前的本地校验。成立返回 null，否则返回一句给运营看的话。
 * 与服务端同一组规矩（见文件头）；服务端仍会再判一遍。
 */
export function checkTemplateDraft(rules: readonly RuleLike[], undeliverable: readonly string[]): string | null {
    const defaults = rules.filter((r) => r.region_codes.length === 0).length;
    if (defaults !== 1) {
        return defaults === 0
            ? "要有一条「其余地区」的默认规则（不选任何省）"
            : `只能有一条「其余地区」的默认规则，现在有 ${defaults} 条`;
    }
    const seen = new Map<string, string>();
    const claim = (code: string, where: string): string | null => {
        const prev = seen.get(code);
        if (prev !== undefined) return `${provinceName(code)}同时出现在「${prev}」与「${where}」：一个省只能有一种运费`;
        seen.set(code, where);
        return null;
    };
    for (let i = 0; i < rules.length; i += 1) {
        const r = rules[i]!;
        if (r.first_unit < 1 || r.additional_unit < 1) return `第 ${i + 1} 条规则的首件（首重）与续件（续重）必须至少是 1`;
        for (const c of r.region_codes) {
            const bad = claim(c, `第 ${i + 1} 条规则`);
            if (bad !== null) return bad;
        }
    }
    for (const c of undeliverable) {
        const bad = claim(c, "不配送地区");
        if (bad !== null) return bad;
    }
    return null;
}

/** 某一条规则（或不配送）之外已经被占用的省：多选框里把它们置灰。 */
export function takenElsewhere(rules: readonly RuleLike[], undeliverable: readonly string[], self: number | "undeliverable"): Set<string> {
    const out = new Set<string>();
    rules.forEach((r, i) => {
        if (i !== self) r.region_codes.forEach((c) => out.add(c));
    });
    if (self !== "undeliverable") undeliverable.forEach((c) => out.add(c));
    return out;
}
