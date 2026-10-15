// 批量导入：上传一份 csv → 预检（类目按名字对上）→ 确认导入 → 商品以草稿进库，SKU 与库存都在。
import { test, expect, openAdmin, uniq } from "./support.ts";

test("csv 批量导入两件商品（含一件双规格）", async ({ page, admin }) => {
    const tag = uniq();
    const cat = `E2E导入类目${tag}`;
    await admin.call("POST", "/admin/categories", { name: cat });
    const csv =
        "﻿商品标题,类目,规格名,规格值,SKU 编码,基准价（元）,库存\n" +
        `E2E导入苹果${tag},${cat},,,IMP-A-${tag},12.80,40\n` +
        `E2E导入薯片${tag},${cat},口味,原味,IMP-B1-${tag},8.90,20\n` +
        `E2E导入薯片${tag},${cat},口味,黄瓜味,IMP-B2-${tag},8.90,15\n`;

    await openAdmin(page, "/products/import");
    await page.locator('input[type="file"]').setInputFiles({ name: `e2e-${tag}.csv`, mimeType: "text/csv", buffer: Buffer.from(csv) });
    await page.getByRole("button", { name: "上传并预检" }).click();
    await expect(page.getByText(/3 行、2 件商品/)).toBeVisible();
    await expect(page.getByText(/将导入\s*2\s*件商品（3 个 SKU）/)).toBeVisible();
    // 从确认导入那一条的响应里拿商品 id 再逐个核对：按关键词查列表会撞上异步的检索数据
    const committed = page.waitForResponse((r) => r.url().endsWith("/api/v1/admin/product-imports") && r.request().method() === "POST");
    await page.getByRole("button", { name: "确认导入" }).click();
    const outcome = (await (await committed).json()) as { products: { product_id?: number; status: string }[] };
    await expect(page.getByRole("button", { name: "确认导入" })).toBeDisabled();
    const ids = outcome.products.map((p) => p.product_id).filter((x): x is number => typeof x === "number");
    expect(ids).toHaveLength(2);
    const titles: string[] = [];
    for (const id of ids) {
        const p = await admin.call<{ title: string; status: number; skus?: unknown[] }>("GET", `/admin/products/${id}`);
        expect(p.status).toBe(0); // 一律草稿
        titles.push(p.title);
    }
    expect(titles.sort()).toEqual([`E2E导入苹果${tag}`, `E2E导入薯片${tag}`]);
});
