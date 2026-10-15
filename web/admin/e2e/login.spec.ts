// 登录：管理员给新员工签一次性 token → 新员工在登录页换到会话 → 进到后台首页。
import { test, expect, uniq } from "./support.ts";

test("一次性 token 登录进后台", async ({ page, admin }) => {
    const staff = await admin.call<{ id: number }>("POST", "/admin/staff", {
        email: `e2e-login-${uniq()}@keel.invalid`,
        role: 1,
    });
    const { token } = await admin.call<{ token: string }>("POST", `/admin/staff/${staff.id}/login-token`, {});

    await page.goto("/login");
    await page.getByRole("tab", { name: "已有登录 token" }).click();
    await page.getByPlaceholder("一次性登录 token").fill(token);
    await page.getByRole("button", { name: "换取会话" }).last().click();

    await expect(page).toHaveURL(/\/overview$/);
    await expect(page.getByText("销售趋势")).toBeVisible();

    // 同一串只能用一次
    const again = await page.request.post("/api/v1/admin/auth/session", { data: { token } });
    expect(again.status()).toBe(401);
});
