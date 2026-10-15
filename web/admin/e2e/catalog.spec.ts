// 商品上新：建类目 → 建商品草稿 → 加 SKU（带初始库存）→ 上架 → 买家侧看得到、能看到库存。
import { test, expect, openAdmin, dialog, formItem, pickOption, uniq } from "./support.ts";

test("建类目、建商品、加 SKU、上架，买家看得到", async ({ page, request }) => {
    const tag = uniq();
    const catName = `E2E类目${tag}`;
    const title = `E2E 测试商品 ${tag}`;

    await openAdmin(page, "/categories");
    await page.getByRole("button", { name: "新建根类目" }).click();
    await formItem(dialog(page), "名称").locator("input").fill(catName);
    await dialog(page).getByRole("button", { name: "创建" }).click();
    await expect(page.locator("#app").getByText(catName)).toBeVisible();

    await page.goto("/products");
    await page.getByRole("button", { name: "新建商品" }).click();
    await pickOption(page, formItem(dialog(page), "类目"), catName);
    await formItem(dialog(page), "标题").locator("input").fill(title);
    await dialog(page).getByRole("button", { name: "创建草稿" }).click();
    await expect(page).toHaveURL(/\/products\/\d+$/);
    const productId = Number(page.url().split("/").pop());

    await page.getByRole("tab", { name: /^SKU/ }).click();
    await page.getByRole("button", { name: "加 SKU" }).click();
    const d = dialog(page);
    await formItem(d, "货号").locator("input").fill(`E2E-${tag}`);
    // 1990 分 = ¥19.90：不是整元，曾被 step-strictly 悄悄改成 2000
    await formItem(d, "售价（分）").locator("input").fill("1990");
    await formItem(d, "初始库存").locator("input").fill("30");
    await d.getByRole("button", { name: "提交" }).click();
    await expect(page.getByRole("cell", { name: `E2E-${tag}` })).toBeVisible();

    await page.getByRole("button", { name: "上架" }).click();
    const confirm = page.locator(".el-message-box:visible");
    if (await confirm.count()) await confirm.getByRole("button", { name: /确定|上架/ }).click();
    await expect(page.getByRole("button", { name: "下架" })).toBeVisible();

    const res = await request.get(`/api/v1/products/${productId}`);
    expect(res.ok()).toBeTruthy();
    const body = (await res.json()) as { title: string; skus: { price_cents: number; available_qty: number }[] };
    expect(body.title).toBe(title);
    expect(body.skus[0]).toMatchObject({ price_cents: 1990, available_qty: 30 });
});
