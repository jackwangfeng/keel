// AI 员工闭环：建 AI 员工、发密钥 → 它经 MCP 提一条「改文案」提案 → 商家在后台批准
// → Keel 以 AI 员工身份执行，商品副标题真的变了。
import type { APIRequestContext } from "@playwright/test";
import { test, expect, openAdmin, uniq } from "./support.ts";

/** 最小的 MCP（Streamable HTTP）客户端：initialize → tools/call。响应可能是 JSON，也可能是 SSE。 */
async function mcpCall(req: APIRequestContext, key: string, name: string, args: unknown): Promise<string> {
    const headers: Record<string, string> = {
        Authorization: `Bearer ${key}`,
        Accept: "application/json, text/event-stream",
        "Content-Type": "application/json",
    };
    const parse = (raw: string) => {
        const line = raw.split("\n").find((l) => l.startsWith("data:"));
        return JSON.parse(line ? line.slice(5) : raw) as { result?: unknown; error?: unknown };
    };
    const init = await req.post("/api/v1/mcp", {
        headers,
        data: { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "keel-e2e", version: "1" } } },
    });
    expect(init.ok(), `initialize ${init.status()}`).toBeTruthy();
    const sid = init.headers()["mcp-session-id"];
    if (sid) headers["Mcp-Session-Id"] = sid;
    await req.post("/api/v1/mcp", { headers, data: { jsonrpc: "2.0", method: "notifications/initialized" } });
    const res = await req.post("/api/v1/mcp", {
        headers,
        data: { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name, arguments: args } },
    });
    const out = parse(await res.text());
    expect(out.error, JSON.stringify(out.error)).toBeUndefined();
    return JSON.stringify(out.result);
}

test("AI 员工提案改文案，后台批准后生效", async ({ page, request, admin }) => {
    const tag = uniq();
    const agent = await admin.call<{ id: number }>("POST", "/admin/agents", { name: `E2E AI ${tag}`, role: 2 });
    const key = await admin.call<{ secret: string }>("POST", `/admin/agents/${agent.id}/keys`, { name: "e2e" });
    const products = await admin.call<{ items: { id: number; status: number }[] }>("GET", "/admin/products?page_size=50");
    const product = products.items.find((p) => p.status === 1)!;
    const subtitle = `E2E 新副标题 ${tag}`;

    const result = await mcpCall(request, key.secret, "propose_product_copy", {
        product_id: product.id,
        subtitle,
        evidence: `e2e：近 7 天该商品详情页访问 120 次、加购 3 次（${tag}），副标题没有写卖点。`,
        expected_impact: "加购率提升",
    });
    expect(result).not.toContain('"isError":true');

    await openAdmin(page, "/agents");
    const row = page.locator(".el-table__row", { hasText: `E2E AI ${tag}` }).first();
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: "批准" }).click();
    await page.locator(".el-message-box:visible").getByRole("button", { name: "批准" }).click();

    await expect
        .poll(async () => (await admin.call<{ subtitle?: string }>("GET", `/admin/products/${product.id}`)).subtitle)
        .toBe(subtitle);
});
