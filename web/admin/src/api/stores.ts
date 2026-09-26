// 门店 / 大区的取数。好几个页面都要「全部大区」「全部门店」（下拉框、
// 库存对话框里 store-ambiguous 的引导），放一处。
//
// 列表接口是分页的（page_size 上限 100），这里翻页取全。一家商家几十家店
// 是常态、几千家不是——真到那个量级，下拉框本身就该换成远程搜索了。

import { keel, type AdminRegion, type AdminStore } from "./client.ts";

export async function listAllRegions(): Promise<AdminRegion[]> {
    const out: AdminRegion[] = [];
    for (let page = 1; ; page += 1) {
        const res = await keel.get("/admin/regions", { query: { page, page_size: 100 } });
        out.push(...res.items);
        if (out.length >= res.total || res.items.length === 0) return out;
    }
}

export async function listAllStores(): Promise<{ stores: AdminStore[]; hasDefault: boolean }> {
    const stores: AdminStore[] = [];
    let hasDefault = false;
    for (let page = 1; ; page += 1) {
        const res = await keel.get("/admin/stores", { query: { page, page_size: 100 } });
        stores.push(...res.items);
        hasDefault = res.has_default;
        if (stores.length >= res.total || res.items.length === 0) return { stores, hasDefault };
    }
}

/**
 * 「非默认、没围栏」——一家接不到任何单的店。
 *
 * 这是一个**真实存在的中间态**：建店按契约不收围栏（先建店、再画围栏），
 * 把默认位让给别家也会造出它。数据库不挡它（迁移 00020 里 stores 的定义
 * 记了那条 CHECK 为什么被删），所以界面是它唯一的出口。
 */
export function isIncomplete(store: AdminStore): boolean {
    return !store.is_default && (store.fence === null || store.fence === undefined);
}

/** 价格来自哪一层：1 基准价 / 2 大区价 / 3 门店价。 */
export const PRICE_SOURCE: Record<1 | 2 | 3, { text: string; tag: "info" | "warning" | "success" }> = {
    1: { text: "基准价", tag: "info" },
    2: { text: "大区价", tag: "warning" },
    3: { text: "门店价", tag: "success" },
};
