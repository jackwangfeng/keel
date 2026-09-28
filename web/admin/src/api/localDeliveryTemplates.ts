// 同城配送模板（契约 `/admin/local-delivery-templates`，00111）的类型别名与取数。
//
// 一套「起送价 + 满额免配送费 + 距离分档」存成模板，门店引用它；`is_default` 的那一个
// 是没配过的围栏店的兜底。类型全部取自契约产物 web/src/api/schema.d.ts，
// 与 freight.ts / localDelivery.ts 同一个做法。
//
// 不分页（一家店至多几十个）：任何员工都能读（门店管理员挑模板要用），
// 只有全店范围的员工（管理员 / 操作员）能写——同 can.editFreight(null) 一行判据。

import type { components } from "@contract/schema.js";
import { keel } from "./client.ts";

type S = components["schemas"];

export type LocalDeliveryTemplate = S["LocalDeliveryTemplate"];
export type LocalDeliveryTemplateInput = S["LocalDeliveryTemplateInput"];

const P = "https://keel.dev/problems/";

/** 同城配送模板这一组 Problem type（internal/problem/problem.go）。 */
export const LocalDeliveryTemplateProblem = {
    conflict: `${P}local-delivery-template-conflict`,
    inUse: `${P}local-delivery-template-in-use`,
} as const;

/** 全部模板（不分页，见契约）。 */
export async function listLocalDeliveryTemplates(): Promise<LocalDeliveryTemplate[]> {
    const res = await keel.get("/admin/local-delivery-templates", {});
    return res.items;
}
