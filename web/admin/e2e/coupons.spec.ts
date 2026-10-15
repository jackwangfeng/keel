// 优惠券：后台建一张可领取的满减券 → 买家在领券中心看到并领到 → 后台停用 → 领券中心不再有它。
import { test, expect, openAdmin, dialog, formItem, uniq, buyerLogin } from "./support.ts";

interface Tpl { id: number; name: string }

test("满减券：建、买家领、停用", async ({ page, request }) => {
    const name = `E2E券${uniq()}`;
    await openAdmin(page, "/coupons");
    await page.getByRole("button", { name: "新建券" }).click();
    const d = dialog(page);
    await formItem(d, "名称").locator("input").fill(name);
    await formItem(d, "满（元）").locator("input").fill("50");
    await formItem(d, "减（元）").locator("input").fill("5");
    await formItem(d, "有效期").getByText("领取后 N 天").click();
    await formItem(d, "天数").locator("input").fill("7");
    const claimable = formItem(d, "可领取").locator(".el-switch");
    if (!(await claimable.evaluate((e) => e.classList.contains("is-checked")))) await claimable.click();
    await d.getByRole("button", { name: "创建" }).click();
    await expect(d).toBeHidden();
    const row = page.locator(".el-table__row", { hasText: name });
    await expect(row).toBeVisible();

    const buyer = await buyerLogin(request);
    const center = await buyer.call<{ items: Tpl[] }>("GET", "/coupon-templates");
    const tpl = center.items.find((t) => t.name === name);
    expect(tpl, "领券中心里没有刚建的券").toBeDefined();
    await buyer.call("POST", `/coupon-templates/${tpl!.id}/claim`, {});
    const mine = await buyer.call<{ items: { template_id?: number; name?: string }[] }>("GET", "/coupons");
    expect(JSON.stringify(mine)).toContain(name);

    await row.getByRole("button", { name: "停用" }).click();
    await page.locator(".el-message-box:visible").getByRole("button", { name: /确定|OK/ }).click();
    await expect
        .poll(async () => (await buyer.call<{ items: Tpl[] }>("GET", "/coupon-templates")).items.some((t) => t.name === name))
        .toBe(false);
});
