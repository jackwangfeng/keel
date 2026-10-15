// 商家后台的浏览器端关键流程测试（make admin-e2e；CI 的 e2e 那一路也跑）。
//
// 打的是一套已经起好的单商家栈：后台（console）地址由 KEEL_E2E_ADMIN_URL 给，
// 接口走同一个源的 /api 反代（console 的 nginx），和真实部署一致。
// 浏览器用本机 Chrome（channel: chrome），不下载 Playwright 自带的浏览器 ——
// 国内网络下那一步常常下不来，而 runner 上本来就有 Chrome。
import { defineConfig } from "@playwright/test";

export default defineConfig({
    testDir: "./e2e",
    globalSetup: "./e2e/global-setup.ts",
    // 用例之间共享一个店的数据（订单号、商品），各自建各自的，互不依赖；
    // 但同一家店的计数类断言（列表行数）并发会互相干扰，所以串行。
    workers: 1,
    timeout: 60_000,
    expect: { timeout: 10_000 },
    retries: process.env.CI ? 1 : 0,
    reporter: process.env.CI ? [["list"], ["html", { open: "never", outputFolder: "e2e-report" }]] : "list",
    outputDir: "e2e-results",
    use: {
        baseURL: process.env.KEEL_E2E_ADMIN_URL ?? "http://127.0.0.1:28181",
        channel: "chrome",
        headless: true,
        locale: "zh-CN",
        viewport: { width: 1440, height: 900 },
        trace: "retain-on-failure",
        screenshot: "only-on-failure",
    },
});
