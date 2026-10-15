// 员工：加一个操作员 → 界面当场弹出一次性登录 token → 这串 token 真能换到会话，且角色是操作员。
import { test, expect, openAdmin, dialog, formItem, uniq } from "./support.ts";

test("加员工后弹出的 token 能登录", async ({ page }) => {
    const email = `e2e-staff-${uniq()}@keel.invalid`;
    await openAdmin(page, "/staff");
    await page.getByRole("button", { name: "加员工" }).click();
    const d = dialog(page);
    await formItem(d, "邮箱").locator("input").fill(email);
    await formItem(d, "姓名").locator("input").fill("E2E 操作员");
    await formItem(d, "角色").getByText("操作员").click();
    await d.getByRole("button", { name: "创建并发链接" }).click();

    const tokenBox = page.locator(".el-dialog:visible textarea[readonly]");
    await expect(tokenBox).toBeVisible();
    const token = (await tokenBox.inputValue()).trim();
    expect(token.length).toBeGreaterThan(20);

    const res = await page.request.post("/api/v1/admin/auth/session", { data: { token } });
    expect(res.status()).toBe(200);
    const sess = (await res.json()) as { staff: { email: string; role: number } };
    expect(sess.staff).toMatchObject({ email, role: 2 });

    // 不在列表里找这一行：员工列表分页，反复跑攒下的员工多了，新建的会落到后面几页
    await page.getByRole("button", { name: "关闭", exact: true }).click();
    await expect(page.locator(".el-dialog:visible textarea[readonly]")).toHaveCount(0);
});
