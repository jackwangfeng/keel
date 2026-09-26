// 券管理（契约 /admin/coupon-templates 那一段，数据模型 §7）的类型别名与展示用常量。
//
// 类型全部取自契约产物 web/src/api/schema.d.ts，与 client.ts 同一个做法；
// 单独一个文件而不往 client.ts 里加行，是因为那份文件另外两条并行分支也在改。

import type { ResponseBodyOf } from "@contract/client.mts";
import type { components } from "@contract/schema.js";
import { keel, type AdminProduct } from "./client.ts";

type S = components["schemas"];

export type AdminCouponTemplate = S["AdminCouponTemplate"];
export type CouponScope = S["CouponScope"];
export type CouponScopeInput = S["CouponScopeInput"];
export type CouponTemplateCreateRequest = S["CouponTemplateCreateRequest"];
export type CouponTemplatePatchRequest = S["CouponTemplatePatchRequest"];
export type CouponGrantResult = S["CouponGrantResult"];
export type CouponType = S["CouponType"];
/** `GET /admin/coupon-templates` 的响应体。 */
export type CouponTemplatePage = ResponseBodyOf<"/admin/coupon-templates", "get">;

const P = "https://keel.dev/problems/";

/** 券这一组 Problem type（internal/problem/problem.go）。 */
export const CouponProblem = {
    notApplicable: `${P}coupon-not-applicable`,
    soldOut: `${P}coupon-sold-out`,
    claimLimitReached: `${P}coupon-claim-limit-reached`,
    claimEnded: `${P}coupon-claim-ended`,
    templateLocked: `${P}coupon-template-locked`,
    templateDisabled: `${P}coupon-template-disabled`,
} as const;

export const COUPON_TYPE: Record<CouponType, string> = {
    1: "满减",
    2: "折扣",
    3: "立减",
    4: "包邮",
};

/**
 * 包邮券怎么算（00042 起可建，数据模型 §7）。对话框与列表上给运营看的那句话。
 * 与服务端的口径一致：抵的是运费、最多抵到 0，本单运费为 0 时买家用不了。
 */
export const FREE_SHIPPING_HINT =
    "抵运费：最多抵「封顶」那么多（留空即运费全免），运费最多抵到 0。本单已包邮或商家没配运费模板时，买家用不了这张券。";

export const SCOPE_TYPE: Record<CouponScope["scope_type"], string> = {
    1: "全场",
    2: "分类（含子分类）",
    3: "商品",
    4: "品牌",
    5: "大区",
    6: "门店",
};

/** 一条范围给人看的样子。 */
export function scopeLabel(s: CouponScope): string {
    const kind = SCOPE_TYPE[s.scope_type];
    if (s.scope_type === 1) return kind;
    const target = s.target_name ?? (s.scope_type === 4 ? `品牌 #${s.target_id}` : `#${s.target_id}（已删除）`);
    return `${s.include ? "" : "排除 "}${kind}：${target}`;
}

/**
 * 商品下拉要的「全部商品」。后台商品列表没有关键字搜索参数，这里翻页取全
 * （每页 100，至多 10 页）；真超过一千件，下拉框就该换成远程搜索了。
 */
export async function listProductsForPicker(): Promise<AdminProduct[]> {
    const out: AdminProduct[] = [];
    for (let page = 1; page <= 10; page += 1) {
        const res = await keel.get("/admin/products", { query: { page, page_size: 100 } });
        out.push(...res.items);
        if (out.length >= res.total || res.items.length === 0) break;
    }
    return out;
}
