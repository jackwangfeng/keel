// 运费模板（契约 /admin/freight-templates 那一段，数据模型 §7「运费模板」）的类型别名与取数。
//
// 类型全部取自契约产物 web/src/api/schema.d.ts，与 coupons.ts 同一个做法；
// 单独一个文件而不往 client.ts 里加行，理由也一样：那份文件别的分支也在改。

import type { ResponseBodyOf } from "@contract/client.mts";
import type { components } from "@contract/schema.js";
import { keel } from "./client.ts";

type S = components["schemas"];

export type AdminFreightTemplate = S["AdminFreightTemplate"];
export type FreightTemplateInput = S["FreightTemplateInput"];
export type FreightRule = S["FreightRule"];
export type FreightChargeMode = S["FreightChargeMode"];
/** `GET /admin/freight-templates` 的响应体。 */
export type FreightTemplatePage = ResponseBodyOf<"/admin/freight-templates", "get">;

const P = "https://keel.dev/problems/";

/** 运费这一组 Problem type（internal/problem/problem.go）。 */
export const FreightProblem = {
    conflict: `${P}freight-template-conflict`,
    inUse: `${P}freight-template-in-use`,
    notDeliverable: `${P}region-not-deliverable`,
} as const;

export const CHARGE_MODE: Record<FreightChargeMode, string> = {
    1: "按件",
    2: "按重量",
};

/** 全部模板（翻页取全）。商品编辑页的下拉只要全店模板，由调用方筛。 */
export async function listAllFreightTemplates(): Promise<AdminFreightTemplate[]> {
    const out: AdminFreightTemplate[] = [];
    for (let page = 1; page <= 20; page += 1) {
        const res = await keel.get("/admin/freight-templates", { query: { page, page_size: 100 } });
        out.push(...res.items);
        if (out.length >= res.total || res.items.length === 0) break;
    }
    return out;
}
