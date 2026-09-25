// SDK 冒烟：用这个 SDK 对**真的跑起来的** Keel 打一次 GET /products。
//
//     docker compose up -d --build
//     make sdk-smoke
//
// ## 它和 scripts/smoke.sh 是两件事
//
// smoke.sh 用 curl 证明「那一栈通着」。这个脚本证明的是另一件：**SDK 本身**
// 能把契约里描述的那个响应，在真实网络上、真实响应头下、真实 JSON 里读出来。
// 类型检查一条都盖不住这些 —— tsc 只知道服务端**答应**返回什么。
//
// ## 断言的选法
//
// 只断言那些「被测逻辑没了就必然不成立」的事：
//   - 非空列表   ← 迁移 + 种子 + 租户解析 + RLS 四件事全对才可能
//   - 分页自洽   ← items 的条数必须等于 min(total, page_size)
//   - 钳制生效   ← page_size=100000 必须回 100（internal/service 的 MaxPageSize），
//                  服务端不钳制的话这条立刻红；这是从客户端唯一看得见钳制的地方
//   - 翻页有效   ← 第 1 页和第 2 页是不同的商品，不是同一页回两遍
//   - Problem    ← 4xx 必须是一个能被解析成 Problem 的响应体，而不是 gin 默认的
//                  text/plain "404 page not found"
//
// 退出码非 0 表示失败。

import { KeelClient, ProblemError, type ProductListPage } from "./client.mts";

// ---------------------------------------------------------------------------
// 配置。变量名与 scripts/smoke.sh、compose.yaml 保持一致，三处不会各说各话。
// ---------------------------------------------------------------------------

const port = process.env["KEEL_HTTP_PORT"] ?? "8080";
const base = process.env["KEEL_BASE"] ?? `http://localhost:${port}`;
// 契约 servers 里本地那条是 http://localhost:8080/api/v1 —— 前缀属于 server，不属于路径。
const baseUrl = `${base}/api/v1`;
// 多商家形态（compose.multi.yaml）靠 Host 定位租户；单商家形态下这个是空的。
const smokeHost = process.env["KEEL_SMOKE_HOST"] ?? "";
const timeoutSeconds = Number(process.env["KEEL_SMOKE_TIMEOUT"] ?? "60");

const client = new KeelClient({
    baseUrl,
    ...(smokeHost === "" ? {} : { headers: { Host: smokeHost } }),
});

// ---------------------------------------------------------------------------
// 断言
// ---------------------------------------------------------------------------

let failures = 0;

function check(name: string, ok: boolean, detail: string): void {
    if (ok) {
        console.log(`  ✓ ${name}：${detail}`);
        return;
    }
    failures += 1;
    console.error(`  ✗ ${name}：${detail}`);
}

function checkEqual<T>(name: string, actual: T, expected: T): void {
    check(name, Object.is(actual, expected), `期望 ${String(expected)}，实际 ${String(actual)}`);
}

// ---------------------------------------------------------------------------
// 等服务起来。
//
// 只对「连不上」重试。ProblemError 是服务端给出的判断，重试它等于把一个真实的
// 失败拖到超时才报，而且报出来的还是超时 —— 指向错误的方向。
// ---------------------------------------------------------------------------

async function waitForProducts(): Promise<ProductListPage> {
    let lastError: unknown;
    for (let i = 1; i <= timeoutSeconds; i += 1) {
        try {
            return await client.listProducts();
        } catch (err) {
            if (err instanceof ProblemError) throw err;
            lastError = err;
            await new Promise((resolve) => setTimeout(resolve, 1000));
        }
    }
    throw new Error(`${timeoutSeconds}s 内连不上 ${baseUrl}/products：${String(lastError)}`);
}

async function main(): Promise<void> {
    console.log(`==> SDK 冒烟，目标 ${baseUrl}`);

    // --- 1. GET /products ---------------------------------------------------
    const first = await waitForProducts();
    console.log(`==> GET /products 原样响应：\n${JSON.stringify(first, null, 2)}`);

    check("items 是数组", Array.isArray(first.items), `typeof ${typeof first.items}`);
    check("列表非空（迁移 + 种子 + 租户解析 + RLS 全通）", first.items.length >= 1, `本页 ${first.items.length} 件`);
    checkEqual("默认页码", first.page, 1);
    // 契约里 PageSize 的 default 是 20，Go 侧 service.DefaultPageSize 与它一致。
    checkEqual("默认每页条数", first.page_size, 20);
    check("total 不小于本页条数", first.total >= first.items.length, `total=${first.total}, 本页 ${first.items.length}`);
    checkEqual("本页条数 = min(total, page_size)", first.items.length, Math.min(first.total, first.page_size));

    // 逐件查字段。契约说 id / title / min_price_cents / status 是必填的，
    // 而「类型上必填」和「线上真有」是两件事 —— 后者只有真打一次才知道。
    for (const [index, item] of first.items.entries()) {
        const where = `items[${index}]`;
        check(`${where}.id 是正整数`, Number.isInteger(item.id) && item.id > 0, `id=${String(item.id)}`);
        check(`${where}.title 非空`, typeof item.title === "string" && item.title.length > 0, `title=${String(item.title)}`);
        check(
            `${where}.min_price_cents 是整数分`,
            Number.isInteger(item.min_price_cents),
            `min_price_cents=${String(item.min_price_cents)}`,
        );
        // 契约：前台列表只返回 status=1（上架）。
        checkEqual(`${where}.status 为上架`, item.status, 1);
    }

    // --- 2. 分页钳制是服务端的业务规则，客户端只如实透出 ---------------------
    const clamped = await client.listProducts({ page_size: 100000 });
    // service.MaxPageSize = 100。服务端回显的是**钳制之后**的值 ——
    // 回显原始输入的话，这一条会拿到 100000。
    checkEqual("page_size=100000 被服务端钳到 100", clamped.page_size, 100);
    check(
        "钳制后本页条数不超过 100",
        clamped.items.length <= 100,
        `本页 ${clamped.items.length} 件`,
    );

    const beyond = await client.listProducts({ page: 99999 });
    checkEqual("越界页码回空页而不是报错", beyond.items.length, 0);
    checkEqual("越界页也回显 total", beyond.total, first.total);

    // --- 3. 真的翻得动页 -----------------------------------------------------
    if (first.total >= 2) {
        const p1 = await client.listProducts({ page: 1, page_size: 1 });
        const p2 = await client.listProducts({ page: 2, page_size: 1 });
        checkEqual("page_size=1 第 1 页恰好 1 件", p1.items.length, 1);
        checkEqual("page_size=1 第 2 页恰好 1 件", p2.items.length, 1);
        check(
            "第 1 页与第 2 页不是同一件商品（OFFSET 真的生效）",
            p1.items[0]?.id !== p2.items[0]?.id,
            `page1.id=${String(p1.items[0]?.id)}, page2.id=${String(p2.items[0]?.id)}`,
        );
    } else {
        check("翻页检查", false, `total=${first.total}，不足 2 件，翻页检查没有区分力`);
    }

    // --- 4. 错误走 RFC 9457 Problem，并且被解析成有类型的错误 ----------------
    //
    // /products/{product_id} 在契约里有，Go 侧还没注册这条路由，于是落到
    // app.Router 的 NoRoute —— 那里刻意写了 Problem body。把那段删掉，gin 会回
    // text/plain 的 "404 page not found"，下面第一条断言当场红。
    let caught: unknown = undefined;
    try {
        await client.get("/products/{product_id}", { path: { product_id: 999999999 } });
    } catch (err) {
        caught = err;
    }
    const problemSeen = caught instanceof ProblemError ? caught : undefined;
    if (problemSeen === undefined) {
        // 不 rethrow：这一条本身就是被测的断言，把它变成异常会让输出里看不出
        // 「是这一条红了」，而只看到一个栈。
        check(
            "4xx 必须是能解析成 Problem 的响应体",
            false,
            caught === undefined
                ? "请求居然成功了（服务端不该有这条路由）"
                : `拿到的是 ${caught instanceof Error ? `${caught.name}: ${caught.message}` : String(caught)}`,
        );
    } else {
        console.log(`==> 错误响应原样解析：${JSON.stringify(problemSeen.problem)}`);
        checkEqual("Problem 的 HTTP 状态码", problemSeen.status, 404);
        checkEqual("Problem.status 与 HTTP 状态码一致", problemSeen.problem.status, 404);
        // 客户端按 type 分支（「这家店不存在」和「服务挂了」得区分得开），
        // 所以 type 必须是那个稳定的 URI，不是随便一句话。
        checkEqual("Problem.type", problemSeen.problem.type, "https://keel.dev/problems/not-found");
        check(
            "Problem.title 非空",
            typeof problemSeen.problem.title === "string" && problemSeen.problem.title.length > 0,
            `title=${String(problemSeen.problem.title)}`,
        );
    }

    console.log("");
    if (failures === 0) {
        console.log("SDK 冒烟全部通过。");
        return;
    }
    console.error(`SDK 冒烟失败：${failures} 条断言未通过。`);
    process.exitCode = 1;
}

main().catch((err: unknown) => {
    console.error(`SDK 冒烟异常退出：${err instanceof Error ? err.stack ?? err.message : String(err)}`);
    process.exitCode = 1;
});
