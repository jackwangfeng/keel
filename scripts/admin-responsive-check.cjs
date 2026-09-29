#!/usr/bin/env node
// 商家后台的手机 / 电脑布局自动检查：每个页面（含页内标签页、「新建」弹窗）按两种尺寸各打开一次，
// 查横向撑破、被裁掉的控件、太小的点击目标、被截断的按钮文字、控制台报错与失败的接口，并各截一张整页图。
//
//   node scripts/admin-responsive-check.cjs [后台地址] [输出目录]
//   默认 https://eshop.zzss.fun/admin （演示站免登录）→ ./tmp/responsive
//
// 只读：只点标签页与「新建 / 新增 / 添加」按钮打开弹窗看布局，**不提交任何表单**。
// LOCAL_DIST=<目录>：本地预览。后台页面本身（index.html 与 assets）从这个目录读，接口仍打线上 ——
// 改完不用发布就能检查。目录是 `vite build --base /admin/` 的产物（演示站要注入免登录脚本才进得去）。
// 需要 puppeteer-core 与本机 Chrome：PUPPETEER_CORE（模块路径，默认 ~/.cache/keel-ui-test/node_modules/puppeteer-core）、
// CHROME（默认 /usr/bin/google-chrome）、PROXY（可选，如 http://127.0.0.1:8890）。
//
// 判据（「不合格」进报告的 issues；「提醒」进 warnings）：
//   overflow   页面本身能横向滚动（documentElement.scrollWidth > 视口宽）。表格等自带横向滚动的容器内部不算。
//   clipped    可点的控件有一部分在视口右边之外，且不在可横向滚动的容器里（用户够不着）。
//   tap        手机尺寸下可点的控件小于 32×32 像素（提醒；表格单元里的链接不算）。
//   truncated  按钮 / 标签页文字被截断（scrollWidth > clientWidth）。
//   console    页面报的 JS 错误；接口 4xx / 5xx（401/403 除外）。
"use strict";
const path = require("path");
const fs = require("fs");
const os = require("os");

const BASE = (process.argv[2] || "https://eshop.zzss.fun/admin").replace(/\/$/, "");
const OUT = process.argv[3] || path.join(process.cwd(), "tmp", "responsive");
const puppeteer = require(process.env.PUPPETEER_CORE || path.join(os.homedir(), ".cache/keel-ui-test/node_modules/puppeteer-core"));

const VIEWPORTS = [
    { name: "mobile", width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true },
    { name: "desktop", width: 1440, height: 900, deviceScaleFactor: 1 },
];
// 列表页；detail 是从列表页上找第一个指向详情的链接（没有就跳过）。
const PAGES = [
    { path: "/overview" },
    { path: "/products", detail: /\/admin\/products\/\d+$/ },
    { path: "/products/import" },
    { path: "/categories" },
    { path: "/orders" },
    { path: "/refunds" },
    { path: "/payment-returns" },
    { path: "/coupons" },
    { path: "/promotions" },
    { path: "/freight-templates" },
    { path: "/local-delivery-templates" },
    { path: "/regions", detail: /\/admin\/regions\/\d+$/ },
    { path: "/stores", detail: /\/admin\/stores\/\d+$/ },
    { path: "/staff" },
    { path: "/agents" },
    { path: "/shop-settings" },
];
const sleep = ms => new Promise(r => setTimeout(r, ms));

// 在页面里跑的检查（序列化进浏览器，不能引用外部变量）。
function inspect(isMobile, vw) {
    // 以设定的视口宽为准：手机模式下内容撑宽会把布局视口一起撑大（innerWidth 变大），用它判就永远「不撑破」。
    const desc = el => {
        let s = el.tagName.toLowerCase();
        if (el.id) s += "#" + el.id;
        const cls = [...el.classList].filter(c => !c.startsWith("is-")).slice(0, 2);
        if (cls.length) s += "." + cls.join(".");
        const t = (el.innerText || el.getAttribute("aria-label") || el.getAttribute("placeholder") || "").trim().replace(/\s+/g, " ");
        return t ? `${s}「${t.slice(0, 20)}」` : s;
    };
    const visible = el => {
        const r = el.getBoundingClientRect();
        if (r.width === 0 || r.height === 0) return false;
        const st = getComputedStyle(el);
        return st.visibility !== "hidden" && st.display !== "none" && Number(st.opacity) > 0;
    };
    const inScroller = el => {
        for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
            const ox = getComputedStyle(p).overflowX;
            if ((ox === "auto" || ox === "scroll" || ox === "hidden") && p.scrollWidth > p.clientWidth + 1) return true;
        }
        return false;
    };
    const issues = [], warnings = [];
    const sw = Math.max(document.documentElement.scrollWidth, window.innerWidth);
    if (sw > vw + 1) {
        const culprits = [...document.querySelectorAll("body *")]
            .filter(el => visible(el) && el.getBoundingClientRect().right > vw + 1 && !inScroller(el))
            .filter(el => ![...el.children].some(c => c.getBoundingClientRect().right > vw + 1)) // 只报最里层
            .slice(0, 5).map(desc);
        issues.push({ kind: "overflow", detail: `页面宽 ${sw}px > 视口 ${vw}px`, where: culprits });
    }
    const clickable = [...document.querySelectorAll("button, a[href], [role=button], [role=tab], input:not([type=hidden]), textarea, .el-select, .el-checkbox, .el-radio, .el-switch")]
        .filter(visible);
    const clipped = clickable.filter(el => { const r = el.getBoundingClientRect(); return r.right > vw + 1 && !inScroller(el); });
    if (clipped.length) issues.push({ kind: "clipped", detail: `${clipped.length} 个控件伸出屏幕右边`, where: clipped.slice(0, 5).map(desc) });
    const truncated = [...document.querySelectorAll("button, .el-tabs__item, .el-menu-item")].filter(visible)
        .filter(el => el.scrollWidth > el.clientWidth + 1 && getComputedStyle(el).overflow !== "visible");
    if (truncated.length) issues.push({ kind: "truncated", detail: `${truncated.length} 处按钮 / 标签文字被截断`, where: truncated.slice(0, 5).map(desc) });
    if (isMobile) {
        const small = clickable.filter(el => {
            if (el.closest("td, .el-pagination, .el-checkbox, .el-radio") && !el.matches(".el-checkbox, .el-radio")) return false;
            const r = el.getBoundingClientRect();
            return r.width < 32 || r.height < 32;
        });
        if (small.length) warnings.push({ kind: "tap", detail: `${small.length} 个点击目标小于 32px`, where: small.slice(0, 5).map(desc) });
    }
    return { issues, warnings, scrollWidth: sw, viewportWidth: vw };
}

async function check(page, vp, label, shotName, errors) {
    await sleep(600);
    const r = await page.evaluate(inspect, !!vp.isMobile, vp.width);
    const jsErr = errors.splice(0);
    if (jsErr.length) r.issues.push({ kind: "console", detail: `${jsErr.length} 条报错`, where: jsErr.slice(0, 5) });
    const file = `${vp.name}/${shotName}.png`;
    await page.screenshot({ path: path.join(OUT, file), fullPage: true });
    return { label, viewport: vp.name, screenshot: file, ...r };
}

const MIME = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png",
    ".woff2": "font/woff2", ".woff": "font/woff", ".ttf": "font/ttf", ".json": "application/json", ".ico": "image/x-icon" };

async function serveLocal(page, dir) {
    const basePath = new URL(BASE).pathname.replace(/\/$/, ""); // 如 /admin
    const origin = new URL(BASE).origin;
    await page.setRequestInterception(true);
    page.on("request", req => {
        const u = new URL(req.url());
        if (u.origin !== origin || !(u.pathname === basePath || u.pathname.startsWith(basePath + "/"))) return req.continue();
        let f = path.join(dir, u.pathname.slice(basePath.length));
        if (!fs.existsSync(f) || fs.statSync(f).isDirectory()) {
            // 本地没有的：页面跳转回落到 index.html（SPA），其余照常走线上（如演示站的 /admin/demo-session.json）。
            if (!req.isNavigationRequest()) return req.continue();
            f = path.join(dir, "index.html");
        }
        req.respond({ status: 200, contentType: MIME[path.extname(f)] || "application/octet-stream", body: fs.readFileSync(f) });
    });
}

async function run() {
    for (const vp of VIEWPORTS) fs.mkdirSync(path.join(OUT, vp.name), { recursive: true });
    const args = process.env.PROXY ? [`--proxy-server=${process.env.PROXY}`] : [];
    const browser = await puppeteer.launch({ executablePath: process.env.CHROME || "/usr/bin/google-chrome", headless: "new", args });
    const results = [];
    try {
        for (const vp of VIEWPORTS) {
            const page = await browser.newPage();
            await page.setViewport(vp);
            const errors = [];
            if (process.env.LOCAL_DIST) await serveLocal(page, process.env.LOCAL_DIST);
            page.on("pageerror", e => errors.push("JS: " + String(e.message).slice(0, 160)));
            page.on("response", r => {
                const s = r.status();
                if (s >= 400 && s !== 401 && s !== 403 && r.url().includes("/api/")) errors.push(`HTTP ${s} ${r.url().replace(/^https?:\/\/[^/]+/, "").slice(0, 100)}`);
            });
            const visit = async (url, label, shot) => {
                await page.goto(url, { waitUntil: "networkidle0", timeout: 60000 });
                await sleep(1200);
                results.push(await check(page, vp, label, shot, errors));
                // 页内标签页
                const tabs = await page.$$eval(".el-tabs__item", els => els.filter(e => e.offsetParent).map(e => e.textContent.trim()));
                for (let i = 1; i < tabs.length; i++) {
                    await page.evaluate(i => { const els = [...document.querySelectorAll(".el-tabs__item")].filter(e => e.offsetParent); els[i] && els[i].click(); }, i);
                    await sleep(1200);
                    results.push(await check(page, vp, `${label} › ${tabs[i]}`, `${shot}--tab${i}`, errors));
                }
                // 「新建」类弹窗（只打开看布局，关掉，不提交）
                if (tabs.length <= 1) {
                    const opened = await page.evaluate(() => {
                        const b = [...document.querySelectorAll("button")].find(x => x.offsetParent && /^\s*[+＋]?\s*(新建|新增|添加)/.test(x.textContent));
                        if (!b) return null; b.click(); return b.textContent.trim();
                    });
                    if (opened) {
                        await sleep(1000);
                        if (await page.$(".el-dialog, .el-drawer")) results.push(await check(page, vp, `${label} › 「${opened}」弹窗`, `${shot}--dialog`, errors));
                        await page.keyboard.press("Escape");
                        await sleep(400);
                    }
                }
            };
            for (const p of PAGES) {
                const shot = p.path.replace(/^\//, "").replace(/\//g, "_") || "root";
                try {
                    await visit(BASE + p.path, p.path, shot);
                    if (p.detail) {
                        await page.goto(BASE + p.path, { waitUntil: "networkidle0", timeout: 60000 });
                        await sleep(1000);
                        const href = await page.$$eval("a[href]", (as, src) => { const re = new RegExp(src); const a = as.find(a => re.test(a.href)); return a && a.href; }, p.detail.source);
                        if (href) await visit(href, `${p.path} › 详情`, `${shot}--detail`);
                    }
                } catch (e) {
                    results.push({ label: p.path, viewport: vp.name, issues: [{ kind: "load", detail: String(e.message).slice(0, 160) }], warnings: [] });
                }
            }
            await page.close();
        }
    } finally {
        await browser.close();
    }
    fs.writeFileSync(path.join(OUT, "report.json"), JSON.stringify(results, null, 2));
    const lines = ["# 后台布局检查", "", `地址：${BASE}　时间：${new Date().toISOString()}`, ""];
    for (const vp of VIEWPORTS) {
        const rs = results.filter(r => r.viewport === vp.name);
        const bad = rs.filter(r => r.issues.length);
        lines.push(`## ${vp.name}（${vp.width}px）：${rs.length - bad.length}/${rs.length} 通过`, "");
        for (const r of rs) {
            const mark = r.issues.length ? "✗" : (r.warnings.length ? "△" : "✓");
            lines.push(`- ${mark} ${r.label}` + (r.screenshot ? `　[截图](${r.screenshot})` : ""));
            for (const i of [...r.issues, ...r.warnings]) lines.push(`    - ${i.kind}：${i.detail}${i.where && i.where.length ? " — " + i.where.join("；") : ""}`);
        }
        lines.push("");
    }
    fs.writeFileSync(path.join(OUT, "report.md"), lines.join("\n"));
    const summary = VIEWPORTS.map(vp => {
        const rs = results.filter(r => r.viewport === vp.name);
        return `${vp.name} ${rs.filter(r => !r.issues.length).length}/${rs.length}`;
    }).join("　");
    console.log(`${summary}　报告：${path.join(OUT, "report.md")}`);
    process.exitCode = results.some(r => r.issues.length) ? 1 : 0;
}

run().catch(e => { console.error(e); process.exit(2); });
