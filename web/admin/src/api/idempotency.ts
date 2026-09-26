// 幂等键。
//
// 契约里 7 条 /admin/ 的 POST 带 `Idempotency-Key`（merchants / staff /
// uploads / products / publication / skus / categories）。**界面必须带**，
// 而且带对 —— 带错的两种方式都有对应的报错，都能在界面上看见：
//
//   · 同一把钥匙配不同的请求体 → 422 `idempotency-key-reused`
//   · 同一把钥匙还在处理中       → 409 `idempotency-key-in-flight`
//
// ## 一把钥匙管的是「一次提交」，不是「一次点击」
//
// 幂等键存在的全部理由是：请求发出去了，而响应没回来（超时、断网、
// 用户手抖点了两次）。这时**重发必须用同一把钥匙**，否则服务端会当成
// 第二笔，于是「加一个 SKU」变成加了两个。
//
// 反过来，一次**得到了明确拒绝**的提交（422 字段不合法、404 类目不存在、
// 403 权限不够），用户会改点什么再提交 —— 那是**另一次提交**，请求体不一样，
// 沿用旧钥匙正好撞上 `idempotency-key-reused`。所以要换新的。
//
// 分界线因此不是「成功 / 失败」，是「这次提交结束了没有」：
//
//   | 结果                              | 钥匙 |
//   |-----------------------------------|------|
//   | 网络错误 / 没拿到响应             | 留着 |
//   | 503（合规检查没结论，建议退避重试）| 留着 |
//   | 409 idempotency-key-in-flight     | 留着 |
//   | 2xx                               | 换   |
//   | 其余 4xx（422 / 404 / 403 / 409） | 换   |

import { ProblemError, ProblemType } from "./client.ts";

function newKey(): string {
    // crypto.randomUUID 在 https 与 localhost 下都有（安全上下文）。
    // 退化分支不是装饰：后台在局域网里用裸 http 打开时它是 undefined，
    // 而那时候抛一个 "randomUUID is not a function" 会让整个提交按钮失灵。
    const c: Crypto | undefined = globalThis.crypto;
    if (typeof c?.randomUUID === "function") return c.randomUUID();
    const bytes = new Uint8Array(16);
    if (typeof c?.getRandomValues === "function") c.getRandomValues(bytes);
    else for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

/**
 * 一次提交的幂等键。
 *
 * 用法：一个表单 / 对话框持有一个实例，提交时 `key()`，
 * 提交完 `settle(err)` 让它自己决定留还是换。
 */
export class IdempotentSubmission {
    #key: string | null = null;

    /** 本次提交要带的钥匙。第一次调用时生成，之后原样返回。 */
    key(): string {
        this.#key ??= newKey();
        return this.#key;
    }

    /** 丢掉当前钥匙，下次 `key()` 会生成新的。 */
    rotate(): void {
        this.#key = null;
    }

    /**
     * 一次提交结束后调用。`err` 为 undefined 表示成功。
     * 判据见文件头那张表。
     */
    settle(err?: unknown): void {
        if (err === undefined) {
            this.rotate();
            return;
        }
        if (!(err instanceof ProblemError)) {
            // 网络错误、响应体不是 Problem —— 这两种都「不知道服务端做了没有」，
            // 正是幂等键要覆盖的场景。留着。
            return;
        }
        const type = err.problem.type;
        if (type === ProblemType.idempotencyInFlight) return;
        if (err.status === 503) return;
        this.rotate();
    }
}

/**
 * 把一次带幂等键的提交包起来：拿钥匙 → 跑 → 无论成败都 settle。
 *
 * 写成一个函数而不是让每个调用点自己 try/finally，是因为漏掉 settle 的两种
 * 后果都很隐蔽：忘了 rotate 会在下次提交撞 422，忘了保留会在超时重发时
 * 造出第二笔。
 */
export async function withIdempotency<T>(
    submission: IdempotentSubmission,
    run: (key: string) => Promise<T>,
): Promise<T> {
    const key = submission.key();
    try {
        const out = await run(key);
        submission.settle();
        return out;
    } catch (err) {
        submission.settle(err);
        throw err;
    }
}
