// 履约：买家下单付款 → 后台订单页按单号找到 → 发货 → 买家看到「已发货」与运单号。
// 售后：买家对已付款的单申请仅退款 → 后台售后页同意 → 退款单变成已退款，订单退款金额回写。
import { test, expect, openAdmin, dialog, formItem, buyerPaidOrder, expectToast } from "./support.ts";

interface BuyerOrder {
    status: number;
    items: { id: number; quantity: number }[];
    shipments?: { tracking_no: string }[];
    refunded_cents?: number;
}

test("待发货订单发货，买家看到运单", async ({ page, request }) => {
    const { orderNo, buyer } = await buyerPaidOrder(request);
    const tracking = `SF${Date.now()}`;

    await openAdmin(page, "/orders");
    await formItem(page.locator("form").first(), "单号").locator("input").fill(orderNo);
    await page.getByRole("button", { name: "查询" }).click();
    const row = page.locator(".el-table__row", { hasText: orderNo });
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: "发货" }).click();

    const d = dialog(page);
    const carrier = formItem(d, "承运商");
    await carrier.locator(".el-select__wrapper").click();
    await page.locator(".el-select-dropdown__item:visible").first().click();
    await formItem(d, "运单号").locator("input").fill(tracking);
    await d.getByRole("button", { name: "确认发货" }).click();
    await expect(d).toBeHidden();

    await expect
        .poll(async () => (await buyer.call<BuyerOrder>("GET", `/orders/${orderNo}`)).status)
        .toBe(30);
    const o = await buyer.call<BuyerOrder>("GET", `/orders/${orderNo}`);
    expect(o.shipments?.map((x) => x.tracking_no)).toContain(tracking);
});

test("仅退款申请在后台同意，买家侧退款到账", async ({ page, request }) => {
    const { orderNo, buyer } = await buyerPaidOrder(request);
    const order = await buyer.call<BuyerOrder>("GET", `/orders/${orderNo}`);
    const refund = await buyer.call<{ refund_no: string }>("POST", `/orders/${orderNo}/refunds`, {
        items: [{ order_item_id: order.items[0]!.id, quantity: order.items[0]!.quantity }],
        refund_type: 1,
        reason_code: 1,
        reason_text: "e2e：不想要了",
    });

    await openAdmin(page, "/refunds");
    const row = page.locator(".el-table__row", { hasText: refund.refund_no });
    await expect(row).toBeVisible();
    await row.click();
    const drawer = page.locator(".el-drawer:visible");
    await drawer.locator('[data-test="approve"]').click();
    await page.getByRole("button", { name: "确认同意" }).click();
    await expectToast(page, /同意|已退款|成功/);

    await expect
        .poll(async () => (await buyer.call<BuyerOrder>("GET", `/orders/${orderNo}`)).refunded_cents ?? 0)
        .toBeGreaterThan(0);
});
