// 准备两份会话，写在 e2e/.auth/（gitignore）：
//   platform.json  平台级管理员（引导 token 换来的）
//   session.json   本店管理员 —— 用例都用这一份。商家日常就是这个身份，而且有些事平台级账号
//                  按规则做不了（比如管 AI 员工：role-forbidden）。由平台会话切进店里
//                  （X-Keel-Merchant）建一个新管理员、签一次性 token、换会话，每次跑都新建。
//
// 平台会话的来源，顺序：
//   1. KEEL_E2E_SESSION_JSON 给了就用它（整份平台级 StaffSession）；
//   2. 上次缓存的那份还能用（GET /admin/staff 200）就接着用 —— 本机反复跑时引导 token 早已用掉；
//   3. 否则用引导 token 换：从 `docker logs <项目名>-app-1` 里取 bootstrap_token=…。
//      新起的栈（CI 每次都是）库里没有在岗平台管理员，进程启动时会把它打到 stdout。
import { request, type FullConfig } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { SESSION_FILE, PLATFORM_FILE } from "./support.ts";

async function works(baseURL: string, token: string): Promise<boolean> {
    const ctx = await request.newContext({ baseURL });
    try {
        // 连不上时 Playwright 的报错会把整段请求（含 Authorization 头）打出来，进 CI 日志 —— 换成干净的一句
        const res = await ctx
            .get("/api/v1/admin/staff", { headers: { Authorization: `Bearer ${token}` } })
            .catch((e: Error) => {
                throw new Error(`连不上后台 ${baseURL}（${e.message.split("\n")[0]}）：栈起了吗？KEEL_E2E_ADMIN_URL 对吗？`);
            });
        return res.ok();
    } finally {
        await ctx.dispose();
    }
}

export default async function globalSetup(config: FullConfig): Promise<void> {
    const baseURL = config.projects[0]!.use.baseURL!;
    fs.mkdirSync(path.dirname(SESSION_FILE), { recursive: true, mode: 0o700 });
    const platform = await platformSession(baseURL);
    fs.writeFileSync(PLATFORM_FILE, JSON.stringify(platform), { mode: 0o600 });
    fs.writeFileSync(SESSION_FILE, JSON.stringify(await merchantAdmin(baseURL, platform.token)), { mode: 0o600 });
}

async function merchantAdmin(baseURL: string, platformToken: string): Promise<unknown> {
    const ctx = await request.newContext({ baseURL });
    const call = async (method: "GET" | "POST", url: string, data?: unknown, merchant?: string) => {
        const headers: Record<string, string> = { Authorization: `Bearer ${platformToken}`, "Idempotency-Key": randomUUID() };
        if (merchant) headers["X-Keel-Merchant"] = merchant;
        const res = await ctx.fetch(`/api/v1${url}`, { method, headers, data });
        if (!res.ok()) throw new Error(`${method} ${url} → ${res.status()} ${await res.text()}`);
        return res.json();
    };
    try {
        let code = process.env.KEEL_E2E_MERCHANT;
        if (!code) {
            const list = (await call("GET", "/admin/merchants")) as { items: { code: string; status: number }[] };
            code = list.items.find((m) => m.status === 1)?.code;
            if (!code) throw new Error("平台里没有在营业的店");
        }
        const staff = (await call("POST", "/admin/staff", { email: `e2e-shop-admin-${Date.now()}@keel.invalid`, role: 1 }, code)) as { id: number };
        const { token } = (await call("POST", `/admin/staff/${staff.id}/login-token`, {}, code)) as { token: string };
        const res = await ctx.post("/api/v1/admin/auth/session", { data: { token } });
        if (!res.ok()) throw new Error(`本店管理员换会话失败：${res.status()} ${await res.text()}`);
        return res.json();
    } finally {
        await ctx.dispose();
    }
}

async function platformSession(baseURL: string): Promise<{ token: string }> {
    const given = process.env.KEEL_E2E_SESSION_JSON;
    if (given) return JSON.parse(given) as { token: string };
    if (fs.existsSync(PLATFORM_FILE)) {
        const cached = JSON.parse(fs.readFileSync(PLATFORM_FILE, "utf8")) as { token: string };
        if (await works(baseURL, cached.token)) return cached;
    }

    const project = process.env.KEEL_E2E_COMPOSE_PROJECT ?? process.env.COMPOSE_PROJECT_NAME ?? "keelci";
    // slog 写 stderr，docker logs 把两路分开给，所以合并后再找
    const all = execFileSync("sh", ["-c", `docker logs ${project}-app-1 2>&1`], { encoding: "utf8", maxBuffer: 64 << 20 });
    const token = [...all.matchAll(/bootstrap_token=(\S+)/g)].pop()?.[1];
    if (!token) {
        throw new Error(
            `${project}-app-1 的日志里没有 bootstrap_token：库里已经有平台管理员了。` +
                "设 KEEL_E2E_SESSION_JSON（整份会话），或换一套新起的栈。",
        );
    }
    const ctx = await request.newContext({ baseURL });
    const res = await ctx.post("/api/v1/admin/auth/bootstrap", {
        data: { token, email: "e2e-platform@keel.invalid" },
    });
    if (!res.ok()) throw new Error(`引导 token 换会话失败：${res.status()} ${await res.text()}`);
    const sess = (await res.json()) as { token: string };
    await ctx.dispose();
    return sess;
}
