// 用例共用的东西：后台会话、直接调接口造前置状态（买家下单付款、建员工……）。
//
// 前置状态一律走接口造，界面只用来验「被测的那一步」：用界面去造前置状态，
// 一处无关的界面改动会让十条用例一起红，看不出到底哪条流程坏了。
import { test as base, expect, type APIRequestContext, type Page } from "@playwright/test";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";

export const SESSION_FILE = path.join(import.meta.dirname, ".auth", "session.json"); // 本店管理员
export const PLATFORM_FILE = path.join(import.meta.dirname, ".auth", "platform.json"); // 平台级管理员
export const STORAGE_KEY = "keel.admin.session"; // 与 src/api/client.ts 的 STORAGE_KEY 一致
export const BUYER_PHONE = process.env.KEEL_E2E_BUYER_PHONE ?? "13800000000";
export const BUYER_PASSWORD = process.env.KEEL_E2E_BUYER_PASSWORD ?? "keel-demo-2026"; // db/seed/single.sql 写明的演示口令

export interface StaffSession {
    token: string;
    expire_at: string;
    staff: { id: number; email: string; role: number; merchant_id: number | null };
}

export function readSession(): StaffSession {
    return JSON.parse(fs.readFileSync(SESSION_FILE, "utf8")) as StaffSession;
}

/** 带会话打开后台：会话放进 sessionStorage，与登录页换到会话之后存的位置相同。 */
export async function openAdmin(page: Page, url: string): Promise<void> {
    const raw = fs.readFileSync(SESSION_FILE, "utf8");
    await page.addInitScript(([k, v]) => sessionStorage.setItem(k, v), [STORAGE_KEY, raw] as const);
    await page.goto(url);
}

/** 薄封装：/api/v1 下的 JSON 接口，写请求自动带 Idempotency-Key，非 2xx 直接让用例失败并带上响应体。 */
export class Api {
    constructor(
        private readonly req: APIRequestContext,
        private readonly token?: string,
    ) {}

    async call<T = unknown>(method: string, url: string, data?: unknown): Promise<T> {
        const headers: Record<string, string> = {};
        if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
        if (method !== "GET") headers["Idempotency-Key"] = randomUUID();
        const res = await this.req.fetch(`/api/v1${url}`, { method, headers, data });
        const body = await res.text();
        expect(res.ok(), `${method} ${url} → ${res.status()} ${body.slice(0, 400)}`).toBeTruthy();
        return (body ? JSON.parse(body) : undefined) as T;
    }
}

interface Product { id: number; title: string }
interface ProductDetail { id: number; skus: { id: number; available_qty: number }[] }
interface Order { order_no: string }
interface PaymentIntent { payload: { settle: { url: string; body: string; headers: Record<string, string> } } }

/** 种子买家的会话。 */
export async function buyerLogin(req: APIRequestContext): Promise<Api> {
    const login = await new Api(req).call<{ access_token: string }>("POST", "/auth/login", {
        phone: BUYER_PHONE,
        password: BUYER_PASSWORD,
    });
    return new Api(req, login.access_token);
}

/** 买家：登录、挑一个有货的 SKU 下单、沙箱付款。返回订单号。 */
export async function buyerPaidOrder(req: APIRequestContext): Promise<{ orderNo: string; buyer: Api }> {
    const anon = new Api(req);
    const buyer = await buyerLogin(req);
    const list = await anon.call<{ items: Product[] }>("GET", "/products?page_size=50");
    let skuId: number | undefined;
    for (const p of list.items) {
        const d = await anon.call<ProductDetail>("GET", `/products/${p.id}`);
        const s = d.skus.find((x) => x.available_qty >= 2);
        if (s) {
            skuId = s.id;
            break;
        }
    }
    expect(skuId, "店里找不到可售 ≥ 2 的 SKU").toBeDefined();
    const addrs = await buyer.call<{ items: { id: number; is_default: boolean }[] } | { id: number; is_default: boolean }[]>(
        "GET",
        "/addresses",
    );
    const items = Array.isArray(addrs) ? addrs : addrs.items;
    expect(items.length, "种子买家没有收货地址").toBeGreaterThan(0);
    const addr = items.find((a) => a.is_default) ?? items[0]!;
    const stores = await anon.call<{ items: { id: number; is_default: boolean }[] }>("GET", "/stores");
    const store = stores.items.find((x) => x.is_default) ?? stores.items[0]!;
    const order = await buyer.call<Order>("POST", "/orders", {
        items: [{ sku_id: skuId, quantity: 1 }],
        address_id: addr.id,
        store_id: store.id,
    });
    const intent = await buyer.call<PaymentIntent>("POST", `/orders/${order.order_no}/payments`, { channel: "wechat" });
    const settle = intent.payload.settle;
    const res = await req.post(settle.url, { headers: settle.headers, data: settle.body });
    expect(res.ok(), `沙箱回调 ${res.status()}`).toBeTruthy();
    return { orderNo: order.order_no, buyer };
}

/** 一个当前用例专用、不会和别的用例撞名的后缀。 */
export function uniq(): string {
    return `${Date.now().toString(36)}${Math.floor(Math.random() * 1296).toString(36)}`;
}

export const test = base.extend<{ admin: Api }>({
    admin: async ({ request }, use) => {
        await use(new Api(request, readSession().token));
    },
});
export { expect };

/** 当前最上层的对话框（Element Plus 的弹窗都挂在 body 下，按可见过滤取最后一个）。 */
export function dialog(page: Page) {
    return page.locator(".el-dialog:visible").last();
}

/** 表单里按标签找那一项（.el-form-item），再在里面找输入控件。 */
export function formItem(scope: ReturnType<Page["locator"]>, label: string) {
    return scope.locator(".el-form-item").filter({ has: scope.page().locator(".el-form-item__label", { hasText: label }) }).first();
}

/** el-select：点开，再点下拉里文字包含 text 的那一项。下拉挂在 body 下，不在表单里。 */
export async function pickOption(page: Page, item: ReturnType<Page["locator"]>, text: string): Promise<void> {
    await item.locator(".el-select__wrapper, .el-select").first().click();
    await page.locator(".el-select-dropdown__item:visible", { hasText: text }).first().click();
}

/** 弹出的 ElMessage 提示（成功 / 失败）里出现这段文字。 */
export async function expectToast(page: Page, text: string | RegExp): Promise<void> {
    await expect(page.locator(".el-message", { hasText: text }).first()).toBeVisible();
}
