// 营销：建一个满减活动（下线状态）→ 上线（接口确认 status=1）→ 下线 → 删除（列表里消失）。
import { test, expect, openAdmin, dialog, formItem, uniq, expectToast } from "./support.ts";

function fmt(d: Date): string {
    const p = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:00`;
}

test("满减活动：建、上线、下线、删除", async ({ page, admin }) => {
    const name = `E2E满减${uniq()}`;
    await openAdmin(page, "/promotions");
    await page.getByRole("button", { name: "新建活动" }).click();
    const d = dialog(page);
    await formItem(d, "名称").locator("input").fill(name);
    const now = new Date();
    await d.getByPlaceholder("开始").fill(fmt(new Date(now.getTime() - 60_000)));
    await d.getByPlaceholder("结束").fill(fmt(new Date(now.getTime() + 86_400_000)));
    await d.getByPlaceholder("结束").press("Enter");
    const tier = formItem(d, "阶梯").locator(".tier-row").first();
    await tier.locator("input").nth(0).fill("100");
    await tier.locator("input").nth(1).fill("10");
    await d.getByRole("button", { name: "创建（下线状态）" }).click();
    await expect(d).toBeHidden();

    const row = page.locator(".el-table__row", { hasText: name });
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: "上线" }).click();
    const box = page.locator(".el-message-box:visible");
    if (await box.count()) await box.getByRole("button").last().click();
    await expect(row.getByRole("button", { name: "下线" })).toBeVisible();

    const list = await admin.call<{ items: { name: string; status: number }[] }>("GET", "/admin/promotions?page_size=100");
    expect(list.items.find((p) => p.name === name)?.status).toBe(1);

    await row.getByRole("button", { name: "下线" }).click();
    if (await page.locator(".el-message-box:visible").count())
        await page.locator(".el-message-box:visible").getByRole("button").last().click();
    await expect(row.getByRole("button", { name: "上线" })).toBeVisible();

    await row.getByRole("button", { name: "删除" }).click();
    await page.locator(".el-message-box:visible").getByRole("button", { name: "删除" }).click();
    await expectToast(page, /删除/);
    await expect(page.locator(".el-table__row", { hasText: name })).toHaveCount(0);
});
