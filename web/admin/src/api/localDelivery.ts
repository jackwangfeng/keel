// 同城配送（契约 `/admin/stores/{store_id}/local-delivery`，00110）的类型别名与取数。
//
// 类型全部取自契约产物 web/src/api/schema.d.ts，与 freight.ts / coupons.ts 同一个做法。
//
// **有围栏、不是默认店的门店**才按这份配置收配送费（`AdminLocalDelivery.active`），
// 默认门店与没有围栏的门店照旧走运费模板——这份配置能存，只是不生效，界面据 `active`
// 显示是哪种情况。计算只有服务端一份实现，这里只取数 / 存整体替换。

import type { components } from "@contract/schema.js";
import { keel } from "./client.ts";

type S = components["schemas"];

export type AdminLocalDelivery = S["AdminLocalDelivery"];
export type LocalDeliveryConfig = S["LocalDeliveryConfig"];
export type DeliveryTier = S["DeliveryTier"];
export type LocalDeliveryQuote = S["LocalDeliveryQuote"];

export async function getLocalDelivery(storeId: number): Promise<AdminLocalDelivery> {
    return keel.get("/admin/stores/{store_id}/local-delivery", { path: { store_id: storeId } });
}

/** 整体替换（契约 `PUT`）。权限与门店运费模板相同：`can.operateStore`。 */
export async function putLocalDelivery(storeId: number, body: LocalDeliveryConfig): Promise<AdminLocalDelivery> {
    return keel.request("put", "/admin/stores/{store_id}/local-delivery", { path: { store_id: storeId }, body });
}
