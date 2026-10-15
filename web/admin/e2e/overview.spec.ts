// 经营概览：首页几块卡片都渲染出来，打开过程中没有任何接口回 5xx。
import { test, expect, openAdmin } from "./support.ts";

test("经营概览能打开，接口无 5xx", async ({ page }) => {
    const bad: string[] = [];
    page.on("response", (r) => {
        if (r.url().includes("/api/") && r.status() >= 500) bad.push(`${r.status()} ${r.url()}`);
    });
    await openAdmin(page, "/overview");
    for (const title of ["销售趋势", "商品排行 Top 10", "搜索概况"]) {
        await expect(page.getByText(title, { exact: false }).first()).toBeVisible();
    }
    await expect(page.getByText(/库存预警/).first()).toBeVisible();
    expect(bad).toEqual([]);
});
