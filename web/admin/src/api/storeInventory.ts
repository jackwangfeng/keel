// 门店维度的库存清单，翻页取全。
//
// **只有存在的行。** 契约：这一行不存在时视同 0，不是「这家店不卖」——
// 一家刚开的店在录库存之前每个 SKU 都缺行。调用方拿不到某个 SKU 时按 0 处理，
// 首次录入的 expected_available_qty 传 0。

import { keel, type AdminInventory } from "./client.ts";

export async function listAllStoreInventories(storeId: number, lowStockOnly = false): Promise<AdminInventory[]> {
    const out: AdminInventory[] = [];
    for (let page = 1; ; page += 1) {
        const res = await keel.get("/admin/stores/{store_id}/inventories", {
            path: { store_id: storeId },
            query: { page, page_size: 100, ...(lowStockOnly ? { low_stock_only: true } : {}) },
        });
        out.push(...res.items);
        if (out.length >= res.total || res.items.length === 0) return out;
    }
}
