// 类目在好几个页面上都要用（商品列表的筛选、新建商品的下拉、类目页自己）。
// 拉取逻辑放一处，免得每个页面各写一遍 `GET /admin/categories` 再各自
// 拼一次树。
//
// 不做缓存：类目改完要立刻看到新的，而这是个后台，一次请求几十毫秒。
// 加缓存就要回答「什么时候失效」，而那个问题在这里不值得回答。

import { keel, type AdminCategory } from "./client.ts";

export async function listCategories(): Promise<AdminCategory[]> {
    // 契约：返回**扁平**数组，按 path 升序，含停用（status = 0），不含软删。
    return keel.get("/admin/categories");
}

/**
 * 给下拉框用的缩进标签。类目是物化路径（`/1/23/456/`），`level` 从 1 起，
 * 服务端已经按 path 排好序，所以直接按 level 缩进就是一棵树的样子。
 */
export function indentedLabel(c: AdminCategory): string {
    const pad = "　".repeat(Math.max(0, c.level - 1));
    return `${pad}${c.name}${c.status === 0 ? "（停用）" : ""}`;
}
